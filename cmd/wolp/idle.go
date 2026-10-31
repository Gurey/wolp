package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// monitorInterval is how often each host is probed and its idle time checked.
// While a host is asleep each probe is one failed request, which is cheap.
const monitorInterval = 10 * time.Second

var agentClient = &http.Client{Timeout: 3 * time.Second}

func (h *Host) connOpened() {
	h.mu.Lock()
	h.open++
	h.mu.Unlock()
	h.changed()
}

func (h *Host) connClosed() {
	h.mu.Lock()
	h.open--
	h.lastActive = time.Now()
	h.mu.Unlock()
	h.changed()
}

// monitor probes the host for the status page and, if it has an agent, asks
// it to sleep once it has been idle long enough. See probe and checkIdle.
func (h *Host) monitor() {
	h.probe(time.Now())
	for now := range time.Tick(monitorInterval) {
		h.probe(now)
		if h.cfg.Agent != "" {
			h.checkIdle(now)
		}
	}
}

// checkIdle asks the agent to sleep if the host has been idle long enough.
// Both "busy" and "going to sleep" restart the countdown, so a host that stays
// busy is asked again a full timeout later.
func (h *Host) checkIdle(now time.Time) {
	timeout := time.Duration(h.cfg.IdleTimeoutSecs) * time.Second
	h.mu.Lock()
	idle := h.open == 0 && now.Sub(h.lastActive) >= timeout
	asleep := h.st.upKnown && !h.st.up
	h.mu.Unlock()
	if asleep {
		return // the probe just found it asleep; nothing to ask
	}
	if !idle {
		return
	}

	state, restart := h.requestSleep()
	h.mu.Lock()
	if restart {
		h.lastActive = now
	}
	if state != h.agentState {
		if restart {
			h.eventLocked(now, "agent: %s", state)
		} else {
			log.Printf("%s: agent: %s", h.cfg.Name, state)
		}
	}
	h.agentState = state
	h.mu.Unlock()
}

// requestSleep calls the agent's POST /sleep. It returns a description for the
// log and whether to restart the idle countdown. An unreachable agent usually
// means the host is already asleep, so the countdown isn't restarted.
func (h *Host) requestSleep() (state string, restart bool) {
	req, _ := http.NewRequest("POST", strings.TrimSuffix(h.cfg.Agent, "/")+"/sleep", nil)
	req.Header.Set("Authorization", "Bearer "+h.agentToken)
	resp, err := agentClient.Do(req)
	if err != nil {
		return "unreachable (asleep?)", false
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		return "going to sleep", true
	case http.StatusConflict:
		var body struct {
			Busy []string `json:"busy"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		return "busy: " + strings.Join(body.Busy, "; "), true
	default:
		return fmt.Sprintf("unexpected response %s", resp.Status), true
	}
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read agent token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("agent token file %s is empty", path)
	}
	return token, nil
}
