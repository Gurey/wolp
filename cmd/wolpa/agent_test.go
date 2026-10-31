package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
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

// fakeSystem records the commands the agent runs and lets tests script
// what the system reports.
type fakeSystem struct {
	mu        sync.Mutex
	commands  []string
	busy      []string
	active    bool          // whether the service is running
	gpuPIDs   []string      // still on the GPU after waiting
	failOn    string        // a command that should fail
	gpuWaited time.Duration // timeout the agent waited for the GPU with
}

func newTestAgent(sys *fakeSystem, cfg Config) *agent {
	if cfg.SuspendCommand == nil {
		cfg.SuspendCommand = []string{"systemctl", "suspend"}
	}
	return &agent{
		cfg:      cfg,
		token:    []byte("secret"),
		busy:     func() []string { return sys.busy },
		isActive: func(string) bool { return sys.active },
		waitGPU: func(timeout time.Duration) []string {
			sys.gpuWaited = timeout
			return sys.gpuPIDs
		},
		run: func(name string, args ...string) error {
			cmd := strings.Join(append([]string{name}, args...), " ")
			sys.mu.Lock()
			sys.commands = append(sys.commands, cmd)
			sys.mu.Unlock()
			if cmd == sys.failOn {
				return errors.New("failed")
			}
			return nil
		},
	}
}

func (s *fakeSystem) ran() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

func request(t *testing.T, a *agent, method, path, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestAuth(t *testing.T) {
	sys := &fakeSystem{}
	a := newTestAgent(sys, Config{})
	for _, token := range []string{"", "wrong", "secretX", "Secret"} {
		for _, path := range []string{"/status", "/sleep"} {
			method := map[string]string{"/status": "GET", "/sleep": "POST"}[path]
			if code, _ := request(t, a, method, path, token); code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: %d, want 401", method, path, token, code)
			}
		}
	}
	if len(sys.ran()) != 0 {
		t.Errorf("unauthorized requests ran %v", sys.ran())
	}
}

func TestStatus(t *testing.T) {
	sys := &fakeSystem{busy: []string{"ssh session 3 (no tty)"}}
	code, body := request(t, newTestAgent(sys, Config{}), "GET", "/status", "secret")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if busy, _ := body["busy"].([]any); len(busy) != 1 || busy[0] != "ssh session 3 (no tty)" {
		t.Errorf("busy = %v", body["busy"])
	}
}

func TestSleepRefusedWhenBusy(t *testing.T) {
	sys := &fakeSystem{busy: []string{"GPU process running (pid 42)"}, active: true}
	code, body := request(t, newTestAgent(sys, Config{Service: "inference.service"}), "POST", "/sleep", "secret")
	if code != http.StatusConflict {
		t.Fatalf("status %d, want 409", code)
	}
	if busy, _ := body["busy"].([]any); len(busy) != 1 {
		t.Errorf("busy = %v", body["busy"])
	}
	if len(sys.ran()) != 0 {
		t.Errorf("ran %v while busy", sys.ran())
	}
}

func TestSleepAccepted(t *testing.T) {
	sys := &fakeSystem{active: true}
	a := newTestAgent(sys, Config{Service: "inference.service"})
	if code, _ := request(t, a, "POST", "/sleep", "secret"); code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", code)
	}
	// The sequence runs in the background; wait for it to finish.
	deadline := time.Now().Add(5 * time.Second)
	for a.sleeping.Load() || len(sys.ran()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("sequence didn't finish; ran %v", sys.ran())
		}
		time.Sleep(5 * time.Millisecond)
	}
	want := []string{"systemctl stop inference.service", "systemctl suspend"}
	if got := sys.ran(); !slices.Equal(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

func TestGoToSleep(t *testing.T) {
	tests := []struct {
		name string
		sys  fakeSystem
		cfg  Config
		want []string
	}{
		{
			name: "stops the service, then suspends",
			sys:  fakeSystem{active: true},
			cfg:  Config{Service: "inference.service"},
			want: []string{"systemctl stop inference.service", "systemctl suspend"},
		},
		{
			name: "service not running: just suspends",
			sys:  fakeSystem{active: false},
			cfg:  Config{Service: "inference.service"},
			want: []string{"systemctl suspend"},
		},
		{
			name: "no service configured",
			cfg:  Config{},
			want: []string{"systemctl suspend"},
		},
		{
			name: "GPU still busy: restarts the service, doesn't suspend",
			sys:  fakeSystem{active: true, gpuPIDs: []string{"4242"}},
			cfg:  Config{Service: "inference.service"},
			want: []string{"systemctl stop inference.service", "systemctl start inference.service"},
		},
		{
			name: "stopping fails: doesn't suspend",
			sys:  fakeSystem{active: true, failOn: "systemctl stop inference.service"},
			cfg:  Config{Service: "inference.service"},
			want: []string{"systemctl stop inference.service"},
		},
		{
			name: "suspend fails: restarts the service",
			sys:  fakeSystem{active: true, failOn: "systemctl suspend"},
			cfg:  Config{Service: "inference.service"},
			want: []string{"systemctl stop inference.service", "systemctl suspend", "systemctl start inference.service"},
		},
		{
			name: "custom suspend command",
			cfg:  Config{SuspendCommand: []string{"systemctl", "hibernate"}},
			want: []string{"systemctl hibernate"},
		},
		{
			name: "dry run runs nothing",
			sys:  fakeSystem{active: true},
			cfg:  Config{Service: "inference.service", DryRun: true},
			want: nil,
		},
	}
	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAgent(&tt.sys, tt.cfg)
			a.sleeping.Store(true)
			a.goToSleep()
			if got := tt.sys.ran(); !slices.Equal(got, tt.want) {
				t.Errorf("ran %v, want %v", got, tt.want)
			}
			if a.sleeping.Load() {
				t.Error("sleeping flag still set; later requests would be ignored")
			}
		})
	}
}

func TestGoToSleepWaitsForGPU(t *testing.T) {
	sys := &fakeSystem{active: true}
	newTestAgent(sys, Config{Service: "inference.service"}).goToSleep()
	if sys.gpuWaited != gpuWait {
		t.Errorf("waited %s for the GPU, want %s", sys.gpuWaited, gpuWait)
	}
}
