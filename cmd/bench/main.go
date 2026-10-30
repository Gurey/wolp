// bench measures a TCP proxy. `bench serve` runs the backends; `bench run`
// drives traffic at them (directly or through a proxy) and, given the proxy's
// pid, reports the CPU time it spent per test from /proc.
//
//	serve: sink on :19001 (reads to EOF, replies 1 byte), source on :19002
//	       (reads an 8-byte length, writes that many bytes), echo on :19003.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const bufSize = 256 << 10

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: bench serve | bench run [flags]")
	}
	switch os.Args[1] {
	case "serve":
		serve()
	case "run":
		run(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func serve() {
	go listen("127.0.0.1:19001", func(c net.Conn) {
		io.CopyBuffer(io.Discard, c, make([]byte, bufSize))
		c.Write([]byte{1})
	})
	go listen("127.0.0.1:19002", func(c net.Conn) {
		var n uint64
		if binary.Read(c, binary.BigEndian, &n) != nil {
			return
		}
		writeN(c, int64(n))
	})
	go listen("127.0.0.1:19003", func(c net.Conn) {
		io.Copy(c, c)
	})
	select {}
}

func listen(addr string, handle func(net.Conn)) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func() { defer c.Close(); handle(c) }()
	}
}

func writeN(w io.Writer, n int64) error {
	buf := make([]byte, bufSize)
	for n > 0 {
		chunk := min(n, int64(len(buf)))
		if _, err := w.Write(buf[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "host to connect to")
	ports := fs.String("ports", "19001,19002,19003", "sink,source,echo ports")
	pid := fs.Int("pid", 0, "proxy pid to sample CPU time from (0 = none)")
	gib := fs.Int("gib", 8, "GiB per throughput test")
	fs.Parse(args)

	var sink, source, echo string
	if _, err := fmt.Sscanf(strings.ReplaceAll(*ports, ",", " "), "%s %s %s", &sink, &source, &echo); err != nil {
		log.Fatalf("bad -ports: %v", err)
	}
	addr := func(p string) string { return net.JoinHostPort(*host, p) }
	total := int64(*gib) << 30

	measure := func(name string, f func() string) {
		cpu0 := cpuSeconds(*pid)
		start := time.Now()
		result := f()
		elapsed := time.Since(start)
		cpu := cpuSeconds(*pid) - cpu0
		fmt.Printf("%-22s %-34s proxy cpu %6.2fs (%3.0f%% of a core)\n",
			name, result, cpu, 100*cpu/elapsed.Seconds())
	}

	for _, streams := range []int{1, 8} {
		measure(fmt.Sprintf("upload %dx", streams), func() string {
			return throughput(total, parallel(streams, func() { upload(addr(sink), total/int64(streams)) }))
		})
		measure(fmt.Sprintf("download %dx", streams), func() string {
			return throughput(total, parallel(streams, func() { download(addr(source), total/int64(streams)) }))
		})
	}
	measure("rtt (persistent conn)", func() string { return latencies(rtt(addr(echo), 20000)) })
	measure("new conn + 1 rtt", func() string { return latencies(connectRTT(addr(echo), 3000)) })
	if *pid != 0 {
		fmt.Printf("proxy peak RSS %s\n", peakRSS(*pid))
	}
}

// parallel returns a func that runs f in n goroutines and waits for them.
func parallel(n int, f func()) func() {
	return func() {
		var wg sync.WaitGroup
		for range n {
			wg.Go(f)
		}
		wg.Wait()
	}
}

func throughput(bytes int64, f func()) string {
	start := time.Now()
	f()
	secs := time.Since(start).Seconds()
	return fmt.Sprintf("%7.2f Gbit/s", float64(bytes)*8/secs/1e9)
}

func upload(addr string, n int64) {
	c := mustDial(addr)
	defer c.Close()
	if err := writeN(c, n); err != nil {
		log.Fatalf("upload: %v", err)
	}
	c.(*net.TCPConn).CloseWrite()
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		log.Fatalf("upload ack: %v", err)
	}
}

func download(addr string, n int64) {
	c := mustDial(addr)
	defer c.Close()
	binary.Write(c, binary.BigEndian, uint64(n))
	got, err := io.CopyBuffer(io.Discard, c, make([]byte, bufSize))
	if err != nil || got != n {
		log.Fatalf("download: got %d of %d bytes: %v", got, n, err)
	}
}

func rtt(addr string, n int) []time.Duration {
	c := mustDial(addr)
	defer c.Close()
	msg, reply := make([]byte, 64), make([]byte, 64)
	out := make([]time.Duration, n)
	for i := range n {
		start := time.Now()
		c.Write(msg)
		if _, err := io.ReadFull(c, reply); err != nil {
			log.Fatalf("rtt: %v", err)
		}
		out[i] = time.Since(start)
	}
	return out
}

func connectRTT(addr string, n int) []time.Duration {
	msg, reply := make([]byte, 64), make([]byte, 64)
	out := make([]time.Duration, n)
	for i := range n {
		start := time.Now()
		c := mustDial(addr)
		c.Write(msg)
		if _, err := io.ReadFull(c, reply); err != nil {
			log.Fatalf("connect rtt: %v", err)
		}
		out[i] = time.Since(start)
		c.Close()
	}
	return out
}

func latencies(d []time.Duration) string {
	slices.Sort(d)
	p := func(q float64) float64 { return float64(d[int(q*float64(len(d)-1))].Microseconds()) }
	return fmt.Sprintf("p50 %4.0fµs  p99 %5.0fµs", p(0.50), p(0.99))
}

func mustDial(addr string) net.Conn {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatalf("dial %s: %v", addr, err)
	}
	return c
}

// cpuSeconds returns user+system CPU time of pid from /proc/<pid>/stat.
func cpuSeconds(pid int) float64 {
	if pid == 0 {
		return 0
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		log.Fatalf("read proxy stat: %v", err)
	}
	// Fields after the ")" closing the command name; utime and stime are
	// fields 14 and 15 overall, i.e. index 11 and 12 here. Ticks are 1/100 s.
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	var utime, stime float64
	fmt.Sscan(f[11], &utime)
	fmt.Sscan(f[12], &stime)
	return (utime + stime) / 100
}

func peakRSS(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for line := range strings.Lines(string(b)) {
		if strings.HasPrefix(line, "VmHWM:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	return "?"
}
