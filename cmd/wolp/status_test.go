package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func eventTexts(s HostStatus) string {
	var texts []string
	for _, e := range s.Events {
		texts = append(texts, e.Text)
	}
	return strings.Join(texts, " | ")
}

func TestSnapshotState(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		set  func(h *Host)
		want string
	}{
		{"not probed yet", func(h *Host) {}, "unknown"},
		{"up", func(h *Host) { h.setUpLocked(true, now, false) }, "awake"},
		{"down", func(h *Host) { h.setUpLocked(false, now, false) }, "asleep"},
		{"agent suspending", func(h *Host) { h.setUpLocked(true, now, false); h.st.agentSleeping = true }, "going to sleep"},
		{"client waiting", func(h *Host) { h.setUpLocked(false, now, false); h.st.waiting = 1 }, "waking"},
		{"wake in progress", func(h *Host) { h.setUpLocked(false, now, false); h.st.wakeStart = now }, "waking"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := testHost(t, HostConfig{})
			tt.set(h)
			if got := h.snapshot(now).State; got != tt.want {
				t.Errorf("state = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSnapshotSleepCountdown(t *testing.T) {
	h, _ := testHost(t, HostConfig{Agent: "http://unused", IdleTimeoutSecs: 900})
	now := time.Now()
	h.setUpLocked(true, now.Add(-time.Hour), false)
	h.lastActive = now.Add(-300 * time.Second)

	s := h.snapshot(now)
	if s.SleepInSecs != 600 || s.IdleSecs != 300 {
		t.Errorf("sleep in %d s, idle %d s; want 600 and 300", s.SleepInSecs, s.IdleSecs)
	}

	h.open = 1
	if s := h.snapshot(now); s.SleepInSecs != -1 || s.IdleSecs != 0 {
		t.Errorf("with a connection open: sleep in %d, idle %d; want no countdown", s.SleepInSecs, s.IdleSecs)
	}
}

func TestAwakePercent(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	t0 := time.Now()
	h.setUpLocked(true, t0, false)                  // up for 1 h
	h.setUpLocked(false, t0.Add(time.Hour), false)  // asleep for 2 h
	h.setUpLocked(true, t0.Add(3*time.Hour), false) // up again for 1 h
	if got := h.snapshot(t0.Add(4 * time.Hour)).AwakePercent; got != 50 {
		t.Errorf("awake %.1f%%, want 50%%", got)
	}
}

func TestProbeWithAgent(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("got %s with auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"busy":["ssh session 3 (no tty)"],"sleeping":false}`))
	}))
	h, _ := testHost(t, HostConfig{Agent: agent.URL})
	now := time.Now()

	h.probe(now)
	s := h.snapshot(now)
	if s.State != "awake" || len(s.Busy) != 1 {
		t.Fatalf("state %q, busy %v; want awake with one reason", s.State, s.Busy)
	}

	agent.Close() // the host went to sleep
	h.probe(now.Add(10 * time.Second))
	s = h.snapshot(now.Add(10 * time.Second))
	if s.State != "asleep" || len(s.Busy) != 0 {
		t.Errorf("state %q, busy %v; want asleep with no reasons", s.State, s.Busy)
	}
	if !strings.Contains(eventTexts(s), "asleep") {
		t.Errorf("no 'asleep' event: %s", eventTexts(s))
	}
}

func TestProbeWithoutAgent(t *testing.T) {
	backend := listen(t, "127.0.0.1:0")
	echoServer(t, backend)
	target := backend.Addr().(*net.TCPAddr).Port

	h, _ := testHost(t, HostConfig{Address: "127.0.0.1", Ports: []PortConfig{{Listen: "unused", Target: target}}})
	h.probe(time.Now())
	if s := h.snapshot(time.Now()); s.State != "awake" {
		t.Errorf("state %q, want awake", s.State)
	}
}

func TestWakeIsRecorded(t *testing.T) {
	target := freeAddr(t)
	h, _ := testHost(t, HostConfig{WakeTimeoutSecs: 10, WolResendSecs: 60})
	proxy := startProxy(t, h, target)
	go func() {
		time.Sleep(1200 * time.Millisecond)
		echoServer(t, listen(t, target))
	}()

	// Two clients wait for the same wake.
	done := make(chan string, 2)
	for _, msg := range []string{"one", "two"} {
		go func() { done <- string(roundTrip(t, proxy, []byte(msg))) }()
	}
	time.Sleep(300 * time.Millisecond)
	if s := h.snapshot(time.Now()); s.State != "waking" || s.Waiting != 2 {
		t.Errorf("while waking: state %q, %d waiting; want waking, 2", s.State, s.Waiting)
	}
	<-done
	<-done
	waitIdle(t, h)

	s := h.snapshot(time.Now())
	if s.Wakes != 1 || s.LastWakeSecs < 1 || s.LastWake == nil {
		t.Errorf("wakes %d, last took %.1fs; want 1 wake of over 1 s", s.Wakes, s.LastWakeSecs)
	}
	if s.State != "awake" || s.Waiting != 0 {
		t.Errorf("after: state %q, %d waiting; want awake, 0", s.State, s.Waiting)
	}
	if s.Conns != 2 || s.BytesUp != 6 || s.BytesDown != 6 {
		t.Errorf("conns %d, up %d, down %d; want 2, 6, 6", s.Conns, s.BytesUp, s.BytesDown)
	}
	events := eventTexts(s)
	if strings.Count(events, "sent wake-on-lan") != 1 || strings.Count(events, "awake after") != 1 {
		t.Errorf("want one wake and one awake event, got: %s", events)
	}
}

func TestEventsAreCapped(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	for i := range maxEvents + 10 {
		h.eventLocked(time.Now(), "event %d", i)
	}
	s := h.snapshot(time.Now())
	if len(s.Events) != maxEvents || s.Events[0].Text != "event 59" {
		t.Errorf("%d events, newest %q; want %d, newest first", len(s.Events), s.Events[0].Text, maxEvents)
	}
}

func TestStatusHandler(t *testing.T) {
	h, _ := testHost(t, HostConfig{Name: "server", Address: "192.168.50.61", Agent: "http://unused", IdleTimeoutSecs: 900,
		Ports: []PortConfig{{Listen: "0.0.0.0:8000", Target: 8000}}})
	h.setUpLocked(true, time.Now(), false)
	h.st.busy = []string{"GPU process running (pid <42>)"}
	h.eventLocked(time.Now(), "awake after 15.8s")
	srv := httptest.NewServer(statusHandler([]*Host{h}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"server", "awake", "0.0.0.0:8000 → 8000", "awake after 15.8s", "pid &lt;42&gt;"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("page is missing %q", want)
		}
	}

	resp, err = http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Hosts []HostStatus }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(body.Hosts) != 1 || body.Hosts[0].State != "awake" || body.Hosts[0].SleepInSecs <= 0 {
		t.Errorf("api: %+v", body.Hosts)
	}

	if resp, _ := http.Get(srv.URL + "/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/nope: %d, want 404", resp.StatusCode)
	}
}
