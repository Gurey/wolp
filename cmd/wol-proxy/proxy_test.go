package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Verbose() {
		log.SetOutput(io.Discard)
	}
	os.Exit(m.Run())
}

// testHost returns a Host whose wake func only counts calls.
func testHost(t *testing.T, hc HostConfig) (*Host, *atomic.Int32) {
	t.Helper()
	if hc.Name == "" {
		hc.Name = "test"
	}
	if hc.MAC == "" {
		hc.MAC = "02:00:00:00:00:01"
	}
	if hc.Agent != "" && hc.AgentTokenFile == "" {
		hc.AgentTokenFile = filepath.Join(t.TempDir(), "agent.token")
		if err := os.WriteFile(hc.AgentTokenFile, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := newHost(hc)
	if err != nil {
		t.Fatal(err)
	}
	var wakes atomic.Int32
	h.wake = func() error { wakes.Add(1); return nil }
	return h, &wakes
}

// echoServer accepts connections on ln and echoes until EOF.
func echoServer(t *testing.T, ln net.Listener) {
	t.Helper()
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
}

func listen(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startProxy serves h on a free local port, forwarding to target.
func startProxy(t *testing.T, h *Host, target string) string {
	t.Helper()
	ln := listen(t, "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	go h.serve(ln, target)
	return ln.Addr().String()
}

// freeAddr returns a local address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln := listen(t, "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// roundTrip sends msg through addr, half-closes, and returns everything read back.
func roundTrip(t *testing.T, addr string, msg []byte) []byte {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// waitIdle waits for the host's open connection count to drop to zero.
func waitIdle(t *testing.T, h *Host) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		open := h.open
		h.mu.Unlock()
		if open == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("connections still open")
}

func TestMagicPacket(t *testing.T) {
	mac, _ := net.ParseMAC("10:7b:44:16:46:27")
	p := magicPacket(mac)
	if len(p) != 102 {
		t.Fatalf("len = %d, want 102", len(p))
	}
	if !bytes.Equal(p[:6], bytes.Repeat([]byte{0xff}, 6)) {
		t.Errorf("header = %x", p[:6])
	}
	for i := range 16 {
		if got := p[6+6*i : 12+6*i]; !bytes.Equal(got, mac) {
			t.Fatalf("repeat %d = %x, want %x", i, got, mac)
		}
	}
}

func TestForwardsWhenHostIsUp(t *testing.T) {
	backend := listen(t, "127.0.0.1:0")
	echoServer(t, backend)
	h, wakes := testHost(t, HostConfig{WakeTimeoutSecs: 5, WolResendSecs: 10})
	before := time.Now()

	msg := bytes.Repeat([]byte("data"), 100_000) // big enough to need several reads
	if got := roundTrip(t, startProxy(t, h, backend.Addr().String()), msg); !bytes.Equal(got, msg) {
		t.Fatalf("got %d bytes back, want %d", len(got), len(msg))
	}
	waitIdle(t, h)

	if n := wakes.Load(); n != 0 {
		t.Errorf("sent %d wake packets to a host that was up", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.lastActive.After(before) {
		t.Error("closing the connection didn't update lastActive")
	}
}

func TestWakesAndHoldsClientUntilHostIsUp(t *testing.T) {
	target := freeAddr(t)
	// Long resend interval: however many times the dial fails, only one packet.
	h, wakes := testHost(t, HostConfig{WakeTimeoutSecs: 10, WolResendSecs: 60})
	proxy := startProxy(t, h, target)

	// The "host" comes up 1.5 s after the client connects.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		echoServer(t, listen(t, target))
	}()

	start := time.Now()
	if got := roundTrip(t, proxy, []byte("hello")); string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
	if waited := time.Since(start); waited < time.Second {
		t.Errorf("answered after %s, before the host was up", waited)
	}
	if n := wakes.Load(); n != 1 {
		t.Errorf("sent %d wake packets, want 1", n)
	}
}

func TestGivesUpAfterWakeTimeout(t *testing.T) {
	h, wakes := testHost(t, HostConfig{WakeTimeoutSecs: 1, WolResendSecs: 60})
	proxy := startProxy(t, h, freeAddr(t))

	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	c.Write([]byte("hello"))

	// The proxy closes the connection; with "hello" still unread on its side
	// that arrives as a reset rather than a clean EOF. Either is fine.
	start := time.Now()
	got, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %q from a host that never came up", got)
	}
	if took := time.Since(start); took < time.Second || took > 5*time.Second {
		t.Errorf("gave up after %s, want about the 1 s wake timeout", took)
	}
	if wakes.Load() == 0 {
		t.Error("never sent a wake packet")
	}
	waitIdle(t, h)
}

func TestWakeIsRateLimited(t *testing.T) {
	h, wakes := testHost(t, HostConfig{WolResendSecs: 10})
	for range 5 {
		h.maybeWake("test")
	}
	if n := wakes.Load(); n != 1 {
		t.Fatalf("5 calls within the interval sent %d packets, want 1", n)
	}

	h.mu.Lock()
	h.lastPacket = time.Now().Add(-11 * time.Second)
	h.mu.Unlock()
	h.maybeWake("test")
	if n := wakes.Load(); n != 2 {
		t.Fatalf("after the interval: %d packets, want 2", n)
	}
}
