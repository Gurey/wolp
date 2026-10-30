// wol-proxy forwards TCP connections to hosts that may be asleep. When a
// host doesn't answer, it sends Wake-on-LAN and keeps the client waiting
// until the host's port accepts connections, then copies bytes both ways.
// Hosts with an agent are asked to sleep once they've been idle long enough.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Hosts []HostConfig `toml:"hosts"`
}

type HostConfig struct {
	Name            string `toml:"name"`
	MAC             string `toml:"mac"`
	Address         string `toml:"address"`
	Broadcast       string `toml:"broadcast"`
	WakeTimeoutSecs int    `toml:"wake_timeout_secs"`
	WolResendSecs   int    `toml:"wol_resend_secs"`
	// Optional: the host's wol-agent, asked to sleep after IdleTimeoutSecs
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

	mu         sync.Mutex
	lastPacket time.Time
	open       int       // client connections currently open, including waiting ones
	lastActive time.Time // when the last connection closed, or a busy/sleep answer
	agentState string    // last logged agent answer, so changes are logged once
}

const dialTimeout = time.Second

func main() {
	configPath := flag.String("config", "config.toml", "path to config file")
	flag.Parse()

	var cfg Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		log.Fatalf("read config: %v", err)
	}
	if len(cfg.Hosts) == 0 {
		log.Fatal("config has no hosts")
	}

	for _, hc := range cfg.Hosts {
		mac, err := net.ParseMAC(hc.MAC)
		if err != nil {
			log.Fatalf("host %s: bad mac: %v", hc.Name, err)
		}
		h := &Host{cfg: hc, mac: mac, lastActive: time.Now()}
		if hc.Agent != "" {
			if h.agentToken, err = readToken(hc.AgentTokenFile); err != nil {
				log.Fatalf("host %s: %v", hc.Name, err)
			}
			go h.idleLoop()
		}
		for _, p := range hc.Ports {
			ln, err := net.Listen("tcp", p.Listen)
			if err != nil {
				log.Fatalf("host %s: listen %s: %v", hc.Name, p.Listen, err)
			}
			target := net.JoinHostPort(hc.Address, fmt.Sprint(p.Target))
			log.Printf("%s: %s -> %s", hc.Name, p.Listen, target)
			go h.serve(ln, target)
		}
	}
	select {}
}

func (h *Host) serve(ln net.Listener, target string) {
	for {
		client, err := ln.Accept()
		if err != nil {
			log.Printf("%s: accept: %v", h.cfg.Name, err)
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

	upstream, woke, err := h.connect(target)
	if err != nil {
		log.Printf("%s: %v: gave up after %s: %v", h.cfg.Name, peer, time.Since(start).Round(time.Millisecond), err)
		return
	}
	defer upstream.Close()
	if woke {
		log.Printf("%s: %v: host up after %s", h.cfg.Name, peer, time.Since(start).Round(time.Millisecond))
	}

	up, down := pipe(client, upstream)
	log.Printf("%s: %v: closed after %s, %d bytes up, %d bytes down",
		h.cfg.Name, peer, time.Since(start).Round(time.Millisecond), up, down)
}

// connect dials the target, waking the host and retrying until it answers
// or the wake timeout runs out. woke reports whether the first dial failed.
func (h *Host) connect(target string) (conn *net.TCPConn, woke bool, err error) {
	deadline := time.Now().Add(time.Duration(h.cfg.WakeTimeoutSecs) * time.Second)
	for {
		c, err := net.DialTimeout("tcp", target, dialTimeout)
		if err == nil {
			return c.(*net.TCPConn), woke, nil
		}
		if time.Now().After(deadline) {
			return nil, woke, err
		}
		woke = true
		h.maybeWake()
		// A failed dial can return instantly (e.g. connection refused while
		// the service starts), so don't spin.
		time.Sleep(500 * time.Millisecond)
	}
}

// maybeWake sends a magic packet unless one went out recently. Every waiting
// connection calls this, so the resend interval doubles as the rate limit.
func (h *Host) maybeWake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.lastPacket) < time.Duration(h.cfg.WolResendSecs)*time.Second {
		return
	}
	h.lastPacket = time.Now()
	if err := sendMagicPacket(h.mac, h.cfg.Broadcast); err != nil {
		log.Printf("%s: wake-on-lan: %v", h.cfg.Name, err)
		return
	}
	log.Printf("%s: sent wake-on-lan to %s", h.cfg.Name, h.mac)
}

func sendMagicPacket(mac net.HardwareAddr, broadcast string) error {
	packet := append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(mac, 16)...)
	conn, err := net.Dial("udp", net.JoinHostPort(broadcast, "9"))
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(packet)
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
