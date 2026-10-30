// wol-agent runs on each host behind wol-proxy. It answers whether the host is
// busy and, when asked to sleep and it isn't, stops the configured service and
// suspends the machine. The proxy decides when a host is idle; the agent only
// vetoes based on activity the proxy can't see (SSH, GPU jobs, direct traffic).
package main

import (
	"crypto/subtle"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Listen    string `toml:"listen"`
	TokenFile string `toml:"token_file"`
	// DryRun logs the commands that would change state (stopping the service,
	// suspending) instead of running them. Read-only checks still run.
	DryRun bool `toml:"dry_run"`
	// Service is stopped before suspend, since a live CUDA process can wedge
	// the machine on resume. Its own GPU use doesn't count as busy. Optional.
	Service string `toml:"service"`
	// Established connections on these ports count as busy. The proxy only
	// asks to sleep once its own connections are closed, so these are direct.
	WatchPorts []int `toml:"watch_ports"`
	// An SSH session with no input for this long stops counting as busy.
	SSHMaxIdleMins int      `toml:"ssh_max_idle_mins"`
	SuspendCommand []string `toml:"suspend_command"`
}

type agent struct {
	cfg      Config
	token    []byte
	sleeping atomic.Bool
}

// gpuWait is how long to wait for GPU processes to exit after stopping the service.
const gpuWait = 30 * time.Second

func main() {
	configPath := flag.String("config", "agent.toml", "path to config file")
	flag.Parse()

	var cfg Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		log.Fatalf("read config: %v", err)
	}
	if len(cfg.SuspendCommand) == 0 {
		log.Fatal("config: suspend_command is required")
	}
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		log.Fatalf("read token: %v", err)
	}
	a := &agent{cfg: cfg, token: []byte(strings.TrimSpace(string(token)))}
	if len(a.token) == 0 {
		log.Fatal("token file is empty")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", a.auth(a.handleStatus))
	mux.HandleFunc("POST /sleep", a.auth(a.handleSleep))
	log.Printf("listening on %s (dry run: %v)", cfg.Listen, cfg.DryRun)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

func (a *agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), a.token) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *agent) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"busy":     busyReasons(a.cfg),
		"sleeping": a.sleeping.Load(),
	})
}

// handleSleep answers 409 with the reasons if the host is busy, otherwise 202
// and starts the suspend sequence in the background.
func (a *agent) handleSleep(w http.ResponseWriter, r *http.Request) {
	if reasons := busyReasons(a.cfg); len(reasons) > 0 {
		log.Printf("sleep refused, busy: %s", strings.Join(reasons, "; "))
		writeJSON(w, http.StatusConflict, map[string]any{"busy": reasons})
		return
	}
	if a.sleeping.CompareAndSwap(false, true) {
		log.Printf("sleep requested by %s", r.RemoteAddr)
		go a.goToSleep()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"sleeping": true})
}

func (a *agent) goToSleep() {
	defer a.sleeping.Store(false)

	stopped := false
	if a.cfg.Service != "" && exec.Command("systemctl", "is-active", "-q", a.cfg.Service).Run() == nil {
		if err := a.change("systemctl", "stop", a.cfg.Service); err != nil {
			log.Printf("stop %s: %v; not suspending", a.cfg.Service, err)
			return
		}
		stopped = true
	}

	if a.cfg.DryRun {
		log.Printf("dry run: would wait up to %s for GPU processes to exit", gpuWait)
	} else if pids := waitForGPUIdle(gpuWait); len(pids) > 0 {
		log.Printf("GPU still in use after stopping %s (pids %s); not suspending", a.cfg.Service, strings.Join(pids, ", "))
		if stopped {
			a.change("systemctl", "start", a.cfg.Service)
		}
		return
	}

	log.Print("suspending")
	if err := a.change(a.cfg.SuspendCommand[0], a.cfg.SuspendCommand[1:]...); err != nil {
		log.Printf("suspend: %v", err)
		if stopped {
			a.change("systemctl", "start", a.cfg.Service)
		}
	}
	// After resume, the systemd-sleep hook starts the service again.
}

// change runs a command that changes system state, or only logs it in dry-run mode.
func (a *agent) change(name string, args ...string) error {
	cmd := strings.Join(append([]string{name}, args...), " ")
	if a.cfg.DryRun {
		log.Printf("dry run: would run %q", cmd)
		return nil
	}
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		log.Printf("%q failed: %v: %s", cmd, err, strings.TrimSpace(string(out)))
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
