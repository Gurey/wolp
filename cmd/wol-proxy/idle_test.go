package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAgent answers POST /sleep with status and body, and counts requests.
func fakeAgent(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/sleep" {
			t.Errorf("got %s %s, want POST /sleep", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want the token from the file", got)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func idleHost(t *testing.T, agentURL string) *Host {
	h, _ := testHost(t, HostConfig{Agent: agentURL, IdleTimeoutSecs: 900})
	return h
}

func TestCheckIdleWaitsForTimeout(t *testing.T) {
	srv, calls := fakeAgent(t, http.StatusAccepted, `{"sleeping":true}`)
	h := idleHost(t, srv.URL)
	now := time.Now()
	h.lastActive = now.Add(-899 * time.Second)

	h.checkIdle(now)
	if calls.Load() != 0 {
		t.Error("asked the agent to sleep before the idle timeout")
	}
}

func TestCheckIdleWaitsForOpenConnections(t *testing.T) {
	srv, calls := fakeAgent(t, http.StatusAccepted, `{"sleeping":true}`)
	h := idleHost(t, srv.URL)
	now := time.Now()
	h.lastActive = now.Add(-time.Hour)
	h.open = 1 // e.g. a long file copy

	h.checkIdle(now)
	if calls.Load() != 0 {
		t.Error("asked the agent to sleep while a connection was open")
	}
}

func TestCheckIdleAsksAgentToSleep(t *testing.T) {
	srv, calls := fakeAgent(t, http.StatusAccepted, `{"sleeping":true}`)
	h := idleHost(t, srv.URL)
	now := time.Now()
	h.lastActive = now.Add(-900 * time.Second)

	h.checkIdle(now)
	if calls.Load() != 1 {
		t.Fatalf("agent called %d times, want 1", calls.Load())
	}
	if !h.lastActive.Equal(now) {
		t.Error("countdown not restarted after the agent agreed to sleep")
	}
	if h.agentState != "going to sleep" {
		t.Errorf("agentState = %q", h.agentState)
	}
}

func TestCheckIdleBusyRestartsCountdown(t *testing.T) {
	srv, calls := fakeAgent(t, http.StatusConflict, `{"busy":["ssh session 3 (no tty)","GPU process running (pid 42)"]}`)
	h := idleHost(t, srv.URL)
	now := time.Now()
	h.lastActive = now.Add(-time.Hour)

	h.checkIdle(now)
	if !h.lastActive.Equal(now) {
		t.Error("countdown not restarted after the agent said busy")
	}
	want := "busy: ssh session 3 (no tty); GPU process running (pid 42)"
	if h.agentState != want {
		t.Errorf("agentState = %q, want %q", h.agentState, want)
	}

	// Asked again only a full timeout later.
	h.checkIdle(now.Add(899 * time.Second))
	if calls.Load() != 1 {
		t.Errorf("asked again %d times before the next timeout", calls.Load()-1)
	}
}

func TestCheckIdleUnreachableAgentKeepsCountdown(t *testing.T) {
	srv, _ := fakeAgent(t, http.StatusAccepted, "")
	url := srv.URL
	srv.Close() // nothing listening: like a host that's already asleep
	h := idleHost(t, url)
	now := time.Now()
	last := now.Add(-time.Hour)
	h.lastActive = last

	h.checkIdle(now)
	if !h.lastActive.Equal(last) {
		t.Error("countdown restarted although the agent was unreachable")
	}
	if !strings.HasPrefix(h.agentState, "unreachable") {
		t.Errorf("agentState = %q", h.agentState)
	}
}

func TestCheckIdleWithoutAgentConfigFails(t *testing.T) {
	_, err := newHost(HostConfig{MAC: "02:00:00:00:00:01", Agent: "http://x", AgentTokenFile: "/does/not/exist"})
	if err == nil {
		t.Fatal("newHost accepted a missing token file")
	}
}
