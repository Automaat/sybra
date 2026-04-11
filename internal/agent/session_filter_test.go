package agent

import (
	"testing"
	"time"
)

func TestFilterSessions(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		sessions []RawSession
		opts     FilterOpts
		wantIDs  []string // expected SessionIDs in result; nil means empty
	}{
		{
			name: "pass-through: untracked live session",
			sessions: []RawSession{
				{Provider: "claude", SessionID: "sess-1", PID: 1001, Mode: "interactive", StartedAt: now},
			},
			opts:    FilterOpts{},
			wantIDs: []string{"sess-1"},
		},
		{
			name: "stale session: Terminated=true is excluded",
			sessions: []RawSession{
				{Provider: "claude", SessionID: "dead-1", PID: 2001, Terminated: true},
			},
			opts:    FilterOpts{},
			wantIDs: nil,
		},
		{
			name: "zombie process: PID already tracked",
			sessions: []RawSession{
				{Provider: "claude", SessionID: "zombie-1", PID: 3001},
			},
			opts:    FilterOpts{TrackedPIDs: map[int]bool{3001: true}},
			wantIDs: nil,
		},
		{
			name: "codex: tracked by file path",
			sessions: []RawSession{
				{Provider: "codex", SessionID: "cx-1", FilePath: "/tmp/session.jsonl"},
			},
			opts:    FilterOpts{TrackedPaths: map[string]bool{"/tmp/session.jsonl": true}},
			wantIDs: nil,
		},
		{
			name: "codex: tracked by session ID",
			sessions: []RawSession{
				{Provider: "codex", SessionID: "cx-2", FilePath: "/tmp/other.jsonl"},
			},
			opts:    FilterOpts{TrackedSessionIDs: map[string]bool{"cx-2": true}},
			wantIDs: nil,
		},
		{
			name: "multiple providers: mixed tracking",
			sessions: []RawSession{
				{Provider: "claude", SessionID: "cl-alive", PID: 4001},
				{Provider: "claude", SessionID: "cl-tracked", PID: 4002},
				{Provider: "codex", SessionID: "cx-alive", FilePath: "/a.jsonl"},
				{Provider: "codex", SessionID: "cx-tracked", FilePath: "/b.jsonl"},
			},
			opts: FilterOpts{
				TrackedPIDs:       map[int]bool{4002: true},
				TrackedPaths:      map[string]bool{},
				TrackedSessionIDs: map[string]bool{"cx-tracked": true},
			},
			wantIDs: []string{"cl-alive", "cx-alive"},
		},
		{
			name: "mode mismatches: filter does not exclude based on mode",
			sessions: []RawSession{
				{Provider: "claude", SessionID: "m-headless", PID: 5001, Mode: "headless"},
				{Provider: "claude", SessionID: "m-interactive", PID: 5002, Mode: "interactive"},
				{Provider: "codex", SessionID: "m-unknown", PID: 5003, Mode: ""},
			},
			opts:    FilterOpts{},
			wantIDs: []string{"m-headless", "m-interactive", "m-unknown"},
		},
		{
			name: "codex: dead with no process is excluded",
			sessions: []RawSession{
				{Provider: "codex", SessionID: "cx-dead", FilePath: "/dead.jsonl", State: StateStopped, PID: 0, Terminated: true},
			},
			opts:    FilterOpts{},
			wantIDs: nil,
		},
		{
			name: "codex: stopped state but has process remains",
			sessions: []RawSession{
				{Provider: "codex", SessionID: "cx-live", FilePath: "/live.jsonl", State: StateStopped, PID: 6001, Terminated: false},
			},
			opts:    FilterOpts{},
			wantIDs: []string{"cx-live"},
		},
		{
			name: "PID zero is not matched against tracked PIDs",
			sessions: []RawSession{
				{Provider: "codex", SessionID: "cx-nopid", FilePath: "/nopid.jsonl", PID: 0},
			},
			opts:    FilterOpts{TrackedPIDs: map[int]bool{0: true}},
			wantIDs: []string{"cx-nopid"},
		},
		{
			name:     "empty session list returns nil",
			sessions: nil,
			opts:     FilterOpts{},
			wantIDs:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterSessions(tt.sessions, tt.opts)
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("got %d sessions, want %d", len(got), len(tt.wantIDs))
			}
			for i, s := range got {
				if s.SessionID != tt.wantIDs[i] {
					t.Errorf("[%d] SessionID = %q, want %q", i, s.SessionID, tt.wantIDs[i])
				}
			}
		})
	}
}
