package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// busyReasons returns why the host shouldn't sleep right now, or nil if idle.
func busyReasons(cfg Config) []string {
	var reasons []string
	reasons = append(reasons, sshSessions(cfg.SSHMaxIdleMins)...)
	reasons = append(reasons, gpuProcesses(cfg.Service)...)
	reasons = append(reasons, watchedConnections(cfg.WatchPorts)...)
	return reasons
}

// sshSessions reports live SSH sessions. A session with a terminal counts
// until it has had no input for maxIdleMins; one without a terminal (scp,
// rsync, a remote command) counts as long as it's connected.
func sshSessions(maxIdleMins int) []string {
	out, err := exec.Command("loginctl", "list-sessions", "--json=short").Output()
	if err != nil {
		return []string{fmt.Sprintf("can't list sessions: %v", err)}
	}
	var sessions []struct {
		Session string `json:"session"`
		Class   string `json:"class"`
	}
	if err := json.Unmarshal(out, &sessions); err != nil {
		return []string{fmt.Sprintf("can't parse sessions: %v", err)}
	}

	var reasons []string
	for _, s := range sessions {
		if s.Class != "user" {
			continue
		}
		props := sessionProps(s.Session)
		// "closing" means the SSH connection is gone but processes it started
		// (e.g. under nohup) are still running; that isn't someone using it.
		if props["Service"] != "sshd" || props["State"] == "closing" {
			continue
		}
		tty := props["TTY"]
		if tty == "" {
			reasons = append(reasons, fmt.Sprintf("ssh session %s (no tty)", s.Session))
			continue
		}
		fi, err := os.Stat("/dev/" + tty)
		if err != nil {
			continue
		}
		atime := fi.Sys().(*syscall.Stat_t).Atim
		idle := time.Since(time.Unix(atime.Sec, atime.Nsec))
		if idle < time.Duration(maxIdleMins)*time.Minute {
			reasons = append(reasons, fmt.Sprintf("ssh session %s (%s, idle %s)", s.Session, tty, idle.Round(time.Minute)))
		}
	}
	return reasons
}

func sessionProps(id string) map[string]string {
	out, _ := exec.Command("loginctl", "show-session", id, "-p", "Service", "-p", "TTY", "-p", "State").Output()
	props := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props
}

// gpuProcesses reports GPU compute processes other than the service's own,
// which hold the GPU whenever the service is up.
func gpuProcesses(service string) []string {
	var own []string
	if service != "" {
		b, _ := os.ReadFile("/sys/fs/cgroup/system.slice/" + service + "/cgroup.procs")
		own = strings.Fields(string(b))
	}
	var reasons []string
	for _, pid := range gpuPIDs() {
		if !slices.Contains(own, pid) {
			reasons = append(reasons, "GPU process running (pid "+pid+")")
		}
	}
	return reasons
}

func gpuPIDs() []string {
	out, err := exec.Command("nvidia-smi", "--query-compute-apps=pid", "--format=csv,noheader").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// waitForGPUIdle polls until no GPU process remains or the timeout passes,
// and returns the pids still running.
func waitForGPUIdle(timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		pids := gpuPIDs()
		if len(pids) == 0 || time.Now().After(deadline) {
			return pids
		}
		time.Sleep(time.Second)
	}
}

// watchedConnections reports established TCP connections to the watched ports.
func watchedConnections(ports []int) []string {
	counts := map[int]int{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
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
		f.Close()
	}
	var reasons []string
	for _, port := range ports {
		if n := counts[port]; n > 0 {
			reasons = append(reasons, fmt.Sprintf("%d connection(s) on port %d", n, port))
		}
	}
	return reasons
}
