package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseReader reads Server-Sent Events messages from a response body.
type sseReader struct{ r *bufio.Reader }

// next returns the data of the next message, skipping messages without data
// (like the initial "retry:" one).
func (s sseReader) next(t *testing.T) string {
	t.Helper()
	var data []string
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading events: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if len(data) > 0 {
				return strings.Join(data, "\n")
			}
			continue
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, d)
		}
	}
}

func subscribeEvents(t *testing.T, hosts []*Host) sseReader {
	t.Helper()
	srv := httptest.NewServer(statusHandler(hosts))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); srv.Close() })
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	return sseReader{bufio.NewReader(resp.Body)}
}

func TestEventsSendsCardsOnConnect(t *testing.T) {
	h, _ := testHost(t, HostConfig{Name: "server"})
	first := subscribeEvents(t, []*Host{h}).next(t)
	if !strings.Contains(first, "<h2>server</h2>") {
		t.Errorf("first message isn't the rendered card:\n%s", first)
	}
	if strings.Contains(first, "<html") {
		t.Error("message contains the whole page, want only the host cards")
	}
}

func TestEventsPushesChangesImmediately(t *testing.T) {
	h, _ := testHost(t, HostConfig{Name: "server"})
	events := subscribeEvents(t, []*Host{h})
	events.next(t)

	start := time.Now()
	h.mu.Lock()
	h.eventLocked(time.Now(), "not answering, sent wake-on-lan")
	h.mu.Unlock()

	msg := events.next(t)
	if !strings.Contains(msg, "not answering, sent wake-on-lan") {
		t.Errorf("update doesn't show the new event:\n%s", msg)
	}
	if took := time.Since(start); took >= heartbeat {
		t.Errorf("update took %s, want it before the %s heartbeat", took, heartbeat)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	h, _ := testHost(t, HostConfig{Name: "server"})
	events := subscribeEvents(t, []*Host{h})
	events.next(t)
	start := time.Now()
	events.next(t) // nothing changed: only the heartbeat sends this
	if took := time.Since(start); took < heartbeat/2 || took > 2*heartbeat {
		t.Errorf("second message after %s, want about %s", took, heartbeat)
	}
}

func TestHubCoalesces(t *testing.T) {
	b := newHub()
	changes, unsubscribe := b.subscribe()
	for range 10 {
		b.notify() // must not block although nobody reads yet
	}
	if n := len(changes); n != 1 {
		t.Errorf("%d pending signals after 10 changes, want 1", n)
	}
	unsubscribe()
	b.notify()
	if len(b.subs) != 0 {
		t.Error("subscriber not removed")
	}
}

func TestPageSubscribesToEvents(t *testing.T) {
	h, _ := testHost(t, HostConfig{Name: "server"})
	h.mu.Lock()
	h.eventLocked(time.Now(), "awake")
	h.mu.Unlock()
	srv := httptest.NewServer(statusHandler([]*Host{h}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`new EventSource("events")`, `<div id="hosts">`, `data-host="server"`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(string(page), `http-equiv="refresh"`) {
		t.Error("page still reloads itself")
	}
}
