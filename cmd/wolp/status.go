package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// maxEvents is how many recent events each host keeps for the status page.
const maxEvents = 50

type event struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// stats is the bookkeeping behind the status page. It lives in Host and is
// guarded by Host.mu.
type stats struct {
	waiting    int       // connections waiting for the host to wake
	wakeStart  time.Time // first magic packet of the wake in progress; zero if none
	wakes      int
	lastWake   time.Time
	lastWakeIn time.Duration

	// From the periodic probe: is the host up, and since when.
	up, upKnown   bool
	stateSince    time.Time
	firstProbe    time.Time
	awakeTotal    time.Duration // time up, not counting the current stretch
	busy          []string      // the agent's current busy reasons
	agentSleeping bool          // the agent is running its suspend sequence

	conns            int
	bytesUp, bytesDn int64
	events           []event
}

// eventLocked logs a host event and keeps it for the status page.
// h.mu must be held.
func (h *Host) eventLocked(now time.Time, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	log.Printf("%s: %s", h.cfg.Name, text)
	h.st.events = append(h.st.events, event{At: now, Text: text})
	if len(h.st.events) > maxEvents {
		h.st.events = h.st.events[len(h.st.events)-maxEvents:]
	}
	h.changed()
}

// setUpLocked records the probe result, tracking how long the host is up.
// h.mu must be held.
func (h *Host) setUpLocked(up bool, now time.Time, announce bool) {
	if h.st.upKnown && up == h.st.up {
		return
	}
	if !h.st.upKnown {
		h.st.firstProbe = now
	} else if h.st.up {
		h.st.awakeTotal += now.Sub(h.st.stateSince)
	}
	if h.st.upKnown && announce {
		h.eventLocked(now, "%s", map[bool]string{true: "awake", false: "asleep"}[up])
	}
	h.st.up, h.st.upKnown, h.st.stateSince = up, true, now
	if !up {
		h.st.busy, h.st.agentSleeping = nil, false
	}
	h.changed()
}

// probe checks whether the host is up. With an agent it asks GET /status,
// which also returns the busy reasons and doesn't count as traffic on the
// forwarded ports; without one it tries a TCP connection to the first port.
func (h *Host) probe(now time.Time) {
	up, busy, sleeping := false, []string(nil), false
	if h.cfg.Agent != "" {
		req, _ := http.NewRequest("GET", strings.TrimSuffix(h.cfg.Agent, "/")+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+h.agentToken)
		if resp, err := agentClient.Do(req); err == nil {
			var body struct {
				Busy     []string `json:"busy"`
				Sleeping bool     `json:"sleeping"`
			}
			up = resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil
			busy, sleeping = body.Busy, body.Sleeping
			resp.Body.Close()
		}
	} else if len(h.cfg.Ports) > 0 {
		target := net.JoinHostPort(h.cfg.Address, fmt.Sprint(h.cfg.Ports[0].Target))
		if c, err := net.DialTimeout("tcp", target, dialTimeout); err == nil {
			c.Close()
			up = true
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	// While a wake is in progress, the wake itself reports the host coming up.
	h.setUpLocked(up, now, h.st.wakeStart.IsZero())
	if up && (!slices.Equal(busy, h.st.busy) || sleeping != h.st.agentSleeping) {
		h.st.busy, h.st.agentSleeping = busy, sleeping
		h.changed()
	}
}

// HostStatus is one host's entry on the status page and in /api/status.
type HostStatus struct {
	Name         string     `json:"name"`
	Address      string     `json:"address"`
	Ports        []string   `json:"ports"`
	State        string     `json:"state"` // awake, asleep, waking, going to sleep, unknown
	StateSecs    int        `json:"state_secs"`
	Open         int        `json:"open"`
	Waiting      int        `json:"waiting"`
	HasAgent     bool       `json:"has_agent"`
	IdleSecs     int        `json:"idle_secs"`
	SleepInSecs  int        `json:"sleep_in_secs"` // -1 if not counting down
	Busy         []string   `json:"busy"`
	AgentAnswer  string     `json:"agent_answer"`
	Wakes        int        `json:"wakes"`
	LastWake     *time.Time `json:"last_wake,omitempty"`
	LastWakeSecs float64    `json:"last_wake_secs"`
	AwakePercent float64    `json:"awake_percent"` // of the time since the first probe
	Conns        int        `json:"connections"`
	BytesUp      int64      `json:"bytes_up"`
	BytesDown    int64      `json:"bytes_down"`
	Events       []event    `json:"events"` // newest first
}

func (h *Host) snapshot(now time.Time) HostStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := &h.st

	s := HostStatus{
		Name:         h.cfg.Name,
		Address:      h.cfg.Address,
		Open:         h.open,
		Waiting:      st.waiting,
		HasAgent:     h.cfg.Agent != "",
		IdleSecs:     int(now.Sub(h.lastActive).Seconds()),
		SleepInSecs:  -1,
		Busy:         st.busy,
		AgentAnswer:  h.agentState,
		Wakes:        st.wakes,
		LastWakeSecs: st.lastWakeIn.Seconds(),
		Conns:        st.conns,
		BytesUp:      st.bytesUp,
		BytesDown:    st.bytesDn,
	}
	for _, p := range h.cfg.Ports {
		s.Ports = append(s.Ports, fmt.Sprintf("%s → %d", p.Listen, p.Target))
	}
	if !st.lastWake.IsZero() {
		t := st.lastWake
		s.LastWake = &t
	}
	if h.open > 0 {
		s.IdleSecs = 0
	}

	switch {
	case st.waiting > 0 || !st.wakeStart.IsZero():
		s.State = "waking"
	case !st.upKnown:
		s.State = "unknown"
	case st.up && st.agentSleeping:
		s.State = "going to sleep"
	case st.up:
		s.State = "awake"
	default:
		s.State = "asleep"
	}
	if st.upKnown {
		s.StateSecs = int(now.Sub(st.stateSince).Seconds())
		awake := st.awakeTotal
		if st.up {
			awake += now.Sub(st.stateSince)
		}
		if total := now.Sub(st.firstProbe); total > 0 {
			s.AwakePercent = 100 * awake.Seconds() / total.Seconds()
		}
	}
	if s.HasAgent && s.State == "awake" && h.open == 0 {
		left := time.Duration(h.cfg.IdleTimeoutSecs)*time.Second - now.Sub(h.lastActive)
		s.SleepInSecs = max(0, int(left.Seconds()))
	}
	for i := len(st.events) - 1; i >= 0; i-- {
		s.Events = append(s.Events, st.events[i])
	}
	return s
}

// statusHandler serves the HTML page at /, live updates at /events and JSON
// at /api/status. It hooks the hosts up to the live updates, so call it
// before the hosts start serving.
func statusHandler(hosts []*Host) http.Handler {
	b := newHub()
	for _, h := range hosts {
		h.notify = b.notify
	}
	pageData := func() map[string]any {
		now := time.Now()
		var out []HostStatus
		for _, h := range hosts {
			out = append(out, h.snapshot(now))
		}
		return map[string]any{"Hosts": out, "Now": now}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"hosts": pageData()["Hosts"]})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		serveEvents(w, r, b, func() ([]byte, error) {
			var buf bytes.Buffer
			err := page.ExecuteTemplate(&buf, "hosts", pageData())
			return buf.Bytes(), err
		})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, pageData()); err != nil {
			log.Printf("status page: %v", err)
		}
	})
	return mux
}

// human formats seconds as a short duration: 45s, 12m, 3h 5m, 2d 4h.
func human(secs int) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", secs)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"human": human,
	"bytes": bytesHuman,
	"ago":   func(now, t time.Time) string { return human(int(now.Sub(t).Seconds())) + " ago" },
	"class": func(state string) string { return strings.ReplaceAll(state, " ", "-") },
	"join":  strings.Join,
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>wolp</title>
<style>
  :root { color-scheme: light dark; --bg: #f6f7f9; --card: #fff; --text: #1d2330; --muted: #677085; --line: #e3e6ec;
          --awake: #1a7f4b; --asleep: #677085; --waking: #b25e00; --going: #6b4fbb; }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #14171c; --card: #1d2128; --text: #e6e9ef; --muted: #98a1b3; --line: #2c323c;
            --awake: #4cc38a; --asleep: #98a1b3; --waking: #f0a35e; --going: #a990ff; }
  }
  body { font: 15px/1.5 system-ui, sans-serif; background: var(--bg); color: var(--text); margin: 0; padding: 24px; }
  main { max-width: 860px; margin: 0 auto; }
  h1 { font-size: 20px; margin: 0 0 16px; }
  .card { background: var(--card); border: 1px solid var(--line); border-radius: 10px; padding: 18px 20px; margin-bottom: 16px; }
  .head { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
  .head h2 { font-size: 18px; margin: 0; }
  .muted { color: var(--muted); }
  .state { font-weight: 600; }
  .state::before { content: "●"; margin-right: 6px; }
  .awake { color: var(--awake); } .asleep, .unknown { color: var(--asleep); }
  .waking { color: var(--waking); } .going-to-sleep { color: var(--going); }
  dl { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: 12px 20px; margin: 16px 0; }
  dt { color: var(--muted); font-size: 13px; } dd { margin: 0; font-variant-numeric: tabular-nums; }
  ul.busy { margin: 4px 0 0; padding-left: 18px; }
  table { width: 100%; border-collapse: collapse; font-size: 14px; }
  td { padding: 4px 0; border-top: 1px solid var(--line); vertical-align: top; }
  td.when { color: var(--muted); white-space: nowrap; width: 1%; padding-right: 16px; font-variant-numeric: tabular-nums; }
  details summary { cursor: pointer; color: var(--muted); margin-top: 8px; }
  footer { color: var(--muted); font-size: 13px; text-align: center; }
  #live::before { content: "●"; margin-right: 4px; color: var(--asleep); }
  #live.on::before { color: var(--awake); }
</style>
</head>
<body><main>
<h1>wolp</h1>
<div id="hosts">{{template "hosts" .}}</div>
<footer><span id="live">connecting…</span> · counters since the proxy started · <a href="api/status">JSON</a></footer>
</main>
<script>
// Live updates: /events sends the rendered host cards; swap them in, keeping
// any event list the user opened or closed as it was.
const hosts = document.getElementById("hosts");
const live = document.getElementById("live");
const events = new EventSource("events");
events.onopen = () => { live.textContent = "live"; live.className = "on"; };
events.onerror = () => { live.textContent = "reconnecting…"; live.className = ""; };
events.onmessage = (e) => {
  const open = {};
  hosts.querySelectorAll("details[data-host]").forEach((d) => (open[d.dataset.host] = d.open));
  hosts.innerHTML = e.data;
  hosts.querySelectorAll("details[data-host]").forEach((d) => {
    if (d.dataset.host in open) d.open = open[d.dataset.host];
  });
};
</script>
</body>
</html>
{{define "hosts"}}{{range .Hosts}}
<section class="card">
  <div class="head">
    <h2>{{.Name}}</h2>
    <span class="state {{class .State}}">{{.State}}</span>
    {{if ne .State "unknown"}}<span class="muted">for {{human .StateSecs}}</span>{{end}}
    <span class="muted">· {{.Address}} · {{join .Ports ", "}}</span>
  </div>
  <dl>
    <div><dt>Connections</dt><dd>{{.Open}} open{{if .Waiting}}, {{.Waiting}} waiting for wake{{end}}</dd></div>
    {{if .HasAgent}}<div><dt>Sleeps in</dt><dd>{{if ge .SleepInSecs 0}}{{human .SleepInSecs}}{{else}}—{{end}}</dd></div>{{end}}
    <div><dt>Idle for</dt><dd>{{if .Open}}—{{else}}{{human .IdleSecs}}{{end}}</dd></div>
    <div><dt>Wakes</dt><dd>{{.Wakes}}{{if .LastWake}} · last took {{printf "%.1f" .LastWakeSecs}}s, {{ago $.Now .LastWake}}{{end}}</dd></div>
    <div><dt>Awake</dt><dd>{{printf "%.0f" .AwakePercent}}% of the time</dd></div>
    <div><dt>Forwarded</dt><dd>{{.Conns}} connections · ↑ {{bytes .BytesUp}} ↓ {{bytes .BytesDown}}</dd></div>
    {{if .HasAgent}}<div><dt>Agent's last answer</dt><dd>{{or .AgentAnswer "—"}}</dd></div>{{end}}
    {{if .Busy}}<div><dt>Busy right now</dt><dd><ul class="busy">{{range .Busy}}<li>{{.}}</li>{{end}}</ul></dd></div>{{end}}
  </dl>
  {{if .Events}}
  <details data-host="{{.Name}}" {{if lt (len .Events) 8}}open{{end}}>
    <summary>Recent events ({{len .Events}})</summary>
    <table>{{range .Events}}<tr><td class="when" title="{{.At.Format "2006-01-02 15:04:05"}}">{{.At.Format "Jan 2 15:04:05"}}</td><td>{{.Text}}</td></tr>{{end}}</table>
  </details>
  {{end}}
</section>
{{end}}{{end}}
`))
