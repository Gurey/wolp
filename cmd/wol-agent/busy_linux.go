package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
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

// sshSessions reports live SSH sessions; see sshReason for the rules.
func sshSessions(maxIdleMins int) []string {
	out, err := exec.Command("loginctl", "list-sessions", "--json=short").Output()
	if err != nil {
		return []string{fmt.Sprintf("can't list sessions: %v", err)}
	}
	ids, err := userSessions(out)
	if err != nil {
		return []string{fmt.Sprintf("can't parse sessions: %v", err)}
	}
	var reasons []string
	for _, id := range ids {
		out, _ := exec.Command("loginctl", "show-session", id, "-p", "Service", "-p", "TTY", "-p", "State").Output()
		if reason, busy := sshReason(id, parseProps(string(out)), time.Duration(maxIdleMins)*time.Minute, ttyIdle); busy {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

// ttyIdle returns how long since the terminal last had input (its atime).
func ttyIdle(tty string) (time.Duration, bool) {
	fi, err := os.Stat("/dev/" + tty)
	if err != nil {
		return 0, false
	}
	atime := fi.Sys().(*syscall.Stat_t).Atim
	return time.Since(time.Unix(atime.Sec, atime.Nsec)), true
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
		if f, err := os.Open(path); err == nil {
			countEstablished(f, ports, counts)
			f.Close()
		}
	}
	return connectionReasons(ports, counts)
}
