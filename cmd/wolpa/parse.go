package main

// Parsing and decision helpers for the busy checks. They take plain input
// rather than reading the system, so they build and are tested on any OS.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// userSessions returns the ids of "user" class sessions in the output of
// `loginctl list-sessions --json=short`.
func userSessions(out []byte) ([]string, error) {
	var sessions []struct {
		Session string `json:"session"`
		Class   string `json:"class"`
	}
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range sessions {
		if s.Class == "user" {
			ids = append(ids, s.Session)
		}
	}
	return ids, nil
}

// parseProps parses `loginctl show-session -p ...` output (key=value lines).
func parseProps(out string) map[string]string {
	props := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props
}

// sshReason decides whether a session keeps the host busy. A session with a
// terminal counts until it has had no input for maxIdle; one without a
// terminal (scp, rsync, a remote command) counts as long as it's connected.
// ttyIdle returns how long a terminal has had no input, or false if unknown.
func sshReason(id string, props map[string]string, maxIdle time.Duration, ttyIdle func(tty string) (time.Duration, bool)) (string, bool) {
	// "closing" means the SSH connection is gone but processes it started
	// (e.g. under nohup) are still running; that isn't someone using it.
	if props["Service"] != "sshd" || props["State"] == "closing" {
		return "", false
	}
	tty := props["TTY"]
	if tty == "" {
		return fmt.Sprintf("ssh session %s (no tty)", id), true
	}
	idle, ok := ttyIdle(tty)
	if !ok || idle >= maxIdle {
		return "", false
	}
	return fmt.Sprintf("ssh session %s (%s, idle %s)", id, tty, idle.Round(time.Minute)), true
}

// countEstablished adds the established connections per watched local port
// in a /proc/net/tcp or /proc/net/tcp6 file to counts.
func countEstablished(r io.Reader, ports []int, counts map[int]int) {
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		// sl local_address rem_address st ...; st 01 is ESTABLISHED.
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != "01" {
			continue
		}
		_, portHex, _ := strings.Cut(fields[1], ":")
		port, err := strconv.ParseInt(portHex, 16, 32)
		if err == nil && slices.Contains(ports, int(port)) {
			counts[int(port)]++
		}
	}
}

// connectionReasons turns per-port counts into busy reasons, in port order.
func connectionReasons(ports []int, counts map[int]int) []string {
	var reasons []string
	for _, port := range ports {
		if n := counts[port]; n > 0 {
			reasons = append(reasons, fmt.Sprintf("%d connection(s) on port %d", n, port))
		}
	}
	return reasons
}
