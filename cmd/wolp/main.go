// wolp (Wake-on-LAN proxy) forwards TCP connections to hosts that may be asleep. When a
// host doesn't answer, it sends Wake-on-LAN and keeps the client waiting
// until the host's port accepts connections, then copies bytes both ways.
// Hosts with an agent are asked to sleep once they've been idle long enough.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Optional address for the status page, e.g. "0.0.0.0:8090".
	StatusListen string       `toml:"status_listen"`
	Hosts        []HostConfig `toml:"hosts"`
}

type HostConfig struct {
	Name            string `toml:"name"`
	MAC             string `toml:"mac"`
	Address         string `toml:"address"`
	Broadcast       string `toml:"broadcast"`
	WakeTimeoutSecs int    `toml:"wake_timeout_secs"`
	WolResendSecs   int    `toml:"wol_resend_secs"`
	// Optional: the host's wolpa agent, asked to sleep after IdleTimeoutSecs
	// with no open connections.
	Agent           string       `toml:"agent"`
	AgentTokenFile  string       `toml:"agent_token_file"`
	IdleTimeoutSecs int          `toml:"idle_timeout_secs"`
	Ports           []PortConfig `toml:"ports"`
}

type PortConfig struct {
	Listen string `toml:"listen"`
	Target int    `toml:"target"`
}

// Host holds the runtime state for one configured machine.
type Host struct {
	cfg HostConfig
	mac net.HardwareAddr

	agentToken string
	wake       func() error // sends the magic packet; replaced in tests

	mu         sync.Mutex
	lastPacket time.Time
	open       int       // client connections currently open, including waiting ones
	lastActive time.Time // when the last connection closed, or a busy/sleep answer
	agentState string    // last logged agent answer, so changes are logged once
	st         stats     // for the status page; see status.go
	notify     func()    // tells the status page something changed; may be nil
}

const dialTimeout = time.Second

// Set at build time with -ldflags "-X main.version=... -X main.build=...
// -X main.commit=...". Releases built by GitHub Actions set all three.
var (
	version = "dev"
	build   = "" // the GitHub Actions run number
	commit  = ""
)

func versionString() string {
	s := version
	if build != "" {
		s += " build " + build
	}
	if commit != "" {
		s += " (" + commit + ")"
	}
	return s
}

func main() {
	configPath := flag.String("config", "wolp.toml", "path to config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("wolp", versionString())
		return
	}
	log.Printf("wolp %s starting", versionString())

	var cfg Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		log.Fatalf("read config: %v", err)
	}
	if len(cfg.Hosts) == 0 {
		log.Fatal("config has no hosts")
	}

	var hosts []*Host
	for _, hc := range cfg.Hosts {
		if hc.AgentTokenFile != "" {
			hc.AgentTokenFile = relativeTo(*configPath, hc.AgentTokenFile)
		}
		h, err := newHost(hc)
		if err != nil {
			log.Fatalf("host %s: %v", hc.Name, err)
		}
		hosts = append(hosts, h)
	}
	// Before the hosts start, so their change notifications are wired up.
	var status http.Handler
	if cfg.StatusListen != "" {
		status = statusHandler(hosts)
	}

	for _, h := range hosts {
		go h.monitor()
		for _, p := range h.cfg.Ports {
			ln, err := net.Listen("tcp", p.Listen)
			if err != nil {
				log.Fatalf("host %s: listen %s: %v", h.cfg.Name, p.Listen, err)
			}
			target := net.JoinHostPort(h.cfg.Address, fmt.Sprint(p.Target))
			log.Printf("%s: %s -> %s", h.cfg.Name, p.Listen, target)
			go h.serve(ln, target)
		}
	}
	if status != nil {
		log.Printf("status page on %s", cfg.StatusListen)
		log.Fatal(http.ListenAndServe(cfg.StatusListen, status))
	}
	select {}
}

func newHost(hc HostConfig) (*Host, error) {
	mac, err := net.ParseMAC(hc.MAC)
	if err != nil {
		return nil, fmt.Errorf("bad mac: %w", err)
	}
	h := &Host{cfg: hc, mac: mac, lastActive: time.Now()}
	h.wake = func() error { return sendMagicPacket(mac, hc.Broadcast) }
	if hc.Agent != "" {
		if h.agentToken, err = readToken(hc.AgentTokenFile); err != nil {
			return nil, err
		}
	}
	return h, nil
}

func (h *Host) serve(ln net.Listener, target string) {
	for {
		client, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// E.g. out of file descriptors; pause instead of spinning.
			log.Printf("%s: accept: %v", h.cfg.Name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go h.handle(client.(*net.TCPConn), target)
	}
}

func (h *Host) handle(client *net.TCPConn, target string) {
	defer client.Close()
	h.connOpened()
	defer h.connClosed()
	peer := client.RemoteAddr()
	start := time.Now()

	upstream, woke, err := h.connect(target, peer)
	if err != nil {
		h.mu.Lock()
		h.st.wakeStart = time.Time{}
		h.eventLocked(time.Now(), "gave up waiting after %s (%v)", time.Since(start).Round(time.Second), err)
		h.mu.Unlock()
		return
	}
	defer upstream.Close()
	if woke {
		h.wokeUp(time.Now())
	}

	up, down := pipe(client, upstream)
	log.Printf("%s: %v: closed after %s, %d bytes up, %d bytes down",
		h.cfg.Name, peer, time.Since(start).Round(time.Millisecond), up, down)
	h.mu.Lock()
	h.st.conns++
	h.st.bytesUp += up
	h.st.bytesDn += down
	h.mu.Unlock()
}

// wokeUp records a finished wake. Several connections may wait for the same
// wake; only the first one to get through records it.
func (h *Host) wokeUp(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setUpLocked(true, now, false)
	if h.st.wakeStart.IsZero() {
		return
	}
	h.st.wakes++
	h.st.lastWake = now
	h.st.lastWakeIn = now.Sub(h.st.wakeStart)
	h.st.wakeStart = time.Time{}
	h.eventLocked(now, "awake after %.1fs", h.st.lastWakeIn.Seconds())
}

// connect dials the target, waking the host and retrying until it answers
// or the wake timeout runs out. woke reports whether the first dial failed.
func (h *Host) connect(target string, peer net.Addr) (conn *net.TCPConn, woke bool, err error) {
	deadline := time.Now().Add(time.Duration(h.cfg.WakeTimeoutSecs) * time.Second)
	defer func() {
		if woke {
			h.mu.Lock()
			h.st.waiting--
			h.mu.Unlock()
			h.changed()
		}
	}()
	for {
		c, err := net.DialTimeout("tcp", target, dialTimeout)
		if err == nil {
			return c.(*net.TCPConn), woke, nil
		}
		if time.Now().After(deadline) {
			return nil, woke, err
		}
		if !woke {
			woke = true
			h.mu.Lock()
			h.st.waiting++
			h.mu.Unlock()
			h.changed()
		}
		h.maybeWake(peer.String())
		// A failed dial can return instantly (e.g. connection refused while
		// the service starts), so don't spin.
		time.Sleep(500 * time.Millisecond)
	}
}

// maybeWake sends a magic packet unless one went out recently. Every waiting
// connection calls this, so the resend interval doubles as the rate limit.
// by names who the host is woken for, for the event log.
func (h *Host) maybeWake(by string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if now.Sub(h.lastPacket) < time.Duration(h.cfg.WolResendSecs)*time.Second {
		return
	}
	h.lastPacket = now
	if err := h.wake(); err != nil {
		h.eventLocked(now, "wake-on-lan failed: %v", err)
		return
	}
	if h.st.wakeStart.IsZero() {
		h.st.wakeStart = now
		h.eventLocked(now, "not answering, sent wake-on-lan (for %s)", by)
	} else {
		log.Printf("%s: resent wake-on-lan to %s", h.cfg.Name, h.mac)
	}
}

// magicPacket is 6 bytes of 0xff followed by the MAC address 16 times.
func magicPacket(mac net.HardwareAddr) []byte {
	return append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(mac, 16)...)
}

func sendMagicPacket(mac net.HardwareAddr, broadcast string) error {
	conn, err := net.Dial("udp", net.JoinHostPort(broadcast, "9"))
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(magicPacket(mac))
	return err
}

// pipe copies in both directions until both sides are done. On Linux,
// io.Copy between two TCPConns uses splice, so data never enters user space.
func pipe(client, upstream *net.TCPConn) (up, down int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		up, _ = io.Copy(upstream, client)
		upstream.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		down, _ = io.Copy(client, upstream)
		client.CloseWrite()
	}()
	wg.Wait()
	return up, down
}

// relativeTo resolves a relative path in the config file against the config
// file's directory, so it doesn't depend on where the binary was started.
func relativeTo(configPath, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(configPath), path)
}
