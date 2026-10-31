package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUserSessions(t *testing.T) {
	out := `[{"session":"64","uid":1000,"user":"gurey","seat":null,"leader":41928,"class":"manager","tty":null,"idle":false,"since":null},
	{"session":"76","uid":1000,"user":"gurey","seat":null,"leader":42831,"class":"user","tty":null,"idle":false,"since":null},
	{"session":"c1","uid":120,"user":"gdm","seat":"seat0","leader":900,"class":"greeter","tty":"tty1","idle":false,"since":null}]`
	ids, err := userSessions([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"76"}) {
		t.Errorf("got %v, want only the user-class session [76]", ids)
	}

	if _, err := userSessions([]byte("not json")); err == nil {
		t.Error("no error for invalid JSON")
	}
}

func TestParseProps(t *testing.T) {
	props := parseProps("Service=sshd\nTTY=pts/0\nState=active\nBroken line\n")
	want := map[string]string{"Service": "sshd", "TTY": "pts/0", "State": "active"}
	if len(props) != len(want) {
		t.Fatalf("got %v, want %v", props, want)
	}
	for k, v := range want {
		if props[k] != v {
			t.Errorf("%s = %q, want %q", k, props[k], v)
		}
	}
}

func TestSSHReason(t *testing.T) {
	const maxIdle = 2 * time.Hour
	idleFor := func(d time.Duration) func(string) (time.Duration, bool) {
		return func(string) (time.Duration, bool) { return d, true }
	}
	unknown := func(string) (time.Duration, bool) { return 0, false }

	tests := []struct {
		name     string
		props    map[string]string
		ttyIdle  func(string) (time.Duration, bool)
		wantBusy bool
		want     string
	}{
		{"active terminal", map[string]string{"Service": "sshd", "TTY": "pts/0", "State": "active"}, idleFor(5 * time.Minute), true, "ssh session 7 (pts/0, idle 5m0s)"},
		{"terminal idle too long", map[string]string{"Service": "sshd", "TTY": "pts/0", "State": "active"}, idleFor(3 * time.Hour), false, ""},
		{"terminal idle unknown", map[string]string{"Service": "sshd", "TTY": "pts/0", "State": "active"}, unknown, false, ""},
		{"no terminal (scp, rsync)", map[string]string{"Service": "sshd", "TTY": "", "State": "active"}, unknown, true, "ssh session 7 (no tty)"},
		{"disconnected, processes left", map[string]string{"Service": "sshd", "TTY": "", "State": "closing"}, unknown, false, ""},
		{"not ssh (local login)", map[string]string{"Service": "login", "TTY": "tty1", "State": "active"}, idleFor(time.Minute), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, busy := sshReason("7", tt.props, maxIdle, tt.ttyIdle)
			if busy != tt.wantBusy || got != tt.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, busy, tt.want, tt.wantBusy)
			}
		})
	}
}

// Lines captured from the server's /proc/net/tcp: a listener on 8000
// (st 0A), an established connection to 8000 (st 01), plus SSH on 22.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   3: 00000000:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 243976 1 0000000000000000 100 0 0 10 0
   4: 3D32A8C0:1F40 A932A8C0:CDF8 01 00000000:00000000 00:00000000 00000000   999        0 238536 1 0000000000000000 20 0 0 10 -1
   5: 3D32A8C0:0016 A932A8C0:CE10 01 00000000:00000000 02:000A7F00 00000000     0        0 245000 2 0000000000000000 20 4 30 10 -1
   6: 3D32A8C0:1F40 4B32A8C0:A1B2 06 00000000:00000000 03:00001770 00000000     0        0 0 3 0000000000000000
`

// tcp6 lists addresses as 32 hex digits; the port parsing is the same.
const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00003D32A8C0:1F40 0000000000000000FFFF0000A932A8C0:CDF9 01 00000000:00000000 00:00000000 00000000   999        0 1 1 0000000000000000 20 0 0 10 -1
`

func TestCountEstablished(t *testing.T) {
	counts := map[int]int{}
	ports := []int{8000, 11434}
	countEstablished(strings.NewReader(procNetTCP), ports, counts)
	countEstablished(strings.NewReader(procNetTCP6), ports, counts)

	// The listener (0A) and the TIME_WAIT (06) don't count; SSH isn't watched.
	if counts[8000] != 2 || counts[11434] != 0 || counts[22] != 0 {
		t.Errorf("counts = %v, want 2 on 8000 and nothing else", counts)
	}
	got := connectionReasons(ports, counts)
	if !slices.Equal(got, []string{"2 connection(s) on port 8000"}) {
		t.Errorf("reasons = %q", got)
	}
}

func TestCountEstablishedIgnoresJunk(t *testing.T) {
	counts := map[int]int{}
	countEstablished(strings.NewReader("header\n\nshort line\n 1: nocolon 0 01\n"), []int{8000}, counts)
	if len(counts) != 0 {
		t.Errorf("counts = %v from malformed input", counts)
	}
}
