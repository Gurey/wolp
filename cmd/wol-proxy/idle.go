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

// idleCheckInterval is how often a host's idle time is checked. While a host
// is asleep each check is one failed request to its agent, which is cheap.
const idleCheckInterval = 10 * time.Second

var agentClient = &http.Client{Timeout: 3 * time.Second}

func (h *Host) connOpened() {
	h.mu.Lock()
	h.open++
	h.mu.Unlock()
}

func (h *Host) connClosed() {
	h.mu.Lock()
	h.open--
	h.lastActive = time.Now()
	h.mu.Unlock()
}

// idleLoop asks the agent to sleep once the host has had no open connections
// for the idle timeout. Both "busy" and "going to sleep" restart the
// countdown, so a host that stays busy is asked again a full timeout later.
func (h *Host) idleLoop() {
	timeout := time.Duration(h.cfg.IdleTimeoutSecs) * time.Second
	for range time.Tick(idleCheckInterval) {
		h.mu.Lock()
		idle := h.open == 0 && time.Since(h.lastActive) >= timeout
		h.mu.Unlock()
		if !idle {
			continue
		}

		state, restart := h.requestSleep()
		h.mu.Lock()
		if restart {
			h.lastActive = time.Now()
		}
		changed := state != h.agentState
		h.agentState = state
		h.mu.Unlock()
		if changed {
			log.Printf("%s: agent: %s", h.cfg.Name, state)
		}
	}
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
