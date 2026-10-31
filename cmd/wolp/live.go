package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// heartbeat is how often /events sends an update even when nothing changed,
// so countdowns and "for 3m" durations keep moving.
const heartbeat = 2 * time.Second

// minGap merges bursts of changes (e.g. many connections at once) into one update.
const minGap = 250 * time.Millisecond

// hub tells /events subscribers that some host's status changed.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{subs: map[chan struct{}]struct{}{}} }

// notify wakes every subscriber without blocking. Each subscriber channel
// holds at most one pending signal, so a burst of changes coalesces.
func (b *hub) notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (b *hub) subscribe() (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[c] = struct{}{}
	b.mu.Unlock()
	return c, func() {
		b.mu.Lock()
		delete(b.subs, c)
		b.mu.Unlock()
	}
}

// changed tells the status page that this host's status changed.
func (h *Host) changed() {
	if h.notify != nil {
		h.notify()
	}
}

// serveEvents streams the rendered host cards as Server-Sent Events: once on
// connect, after every change and on each heartbeat. The page swaps them in.
func serveEvents(w http.ResponseWriter, r *http.Request, b *hub, render func() ([]byte, error)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	changes, unsubscribe := b.subscribe()
	defer unsubscribe()

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	fmt.Fprint(w, "retry: 2000\n\n") // reconnect delay for the browser
	for {
		html, err := render()
		if err != nil {
			log.Printf("status events: %v", err)
			return
		}
		// Each line of a multi-line message needs its own "data:" prefix.
		var msg bytes.Buffer
		for line := range strings.Lines(string(html)) {
			fmt.Fprintf(&msg, "data: %s", strings.TrimSuffix(line, "\n")+"\n")
		}
		msg.WriteString("\n")
		if _, err := w.Write(msg.Bytes()); err != nil {
			return
		}
		flusher.Flush()

		select {
		case <-r.Context().Done():
			return
		case <-changes:
			// Let a burst finish so it becomes one update.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(minGap):
			}
		case <-tick.C:
		}
	}
}
