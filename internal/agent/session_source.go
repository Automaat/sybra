package agent

import "time"

// RawSession is the provider-agnostic discovery record produced by a SessionSource.
// State and Terminated are pre-computed so [filterSessions] requires no I/O.
type RawSession struct {
	Provider   string
	SessionID  string
	PID        int
	CWD        string
	FilePath   string // codex: JSONL path; claude: empty (state read via CWD+SessionID)
	Mode       string // "headless" | "interactive"
	Name       string
	StartedAt  time.Time
	State      State
	Terminated bool // definitively dead/stopped; excluded by filterSessions
}

// SessionSource reads raw sessions from the filesystem.
type SessionSource interface {
	List() []RawSession
}

// claudeSessionSource reads from ~/.claude/sessions/.
type claudeSessionSource struct{}

func (claudeSessionSource) List() []RawSession {
	sessions := readClaudeSessions()
	result := make([]RawSession, 0, len(sessions))
	for _, s := range sessions {
		alive := processAlive(s.PID)
		var state State
		if alive {
			state = inferState(s.CWD, s.SessionID)
		}
		result = append(result, RawSession{
			Provider:   "claude",
			SessionID:  s.SessionID,
			PID:        s.PID,
			CWD:        s.CWD,
			Mode:       sessionKind(s.Kind),
			Name:       s.Name,
			StartedAt:  time.UnixMilli(s.StartedAt).UTC(),
			State:      state,
			Terminated: !alive,
		})
	}
	return result
}

// codexSessionSource reads from ~/.codex/sessions/.
type codexSessionSource struct{}

func (codexSessionSource) List() []RawSession {
	sessions := readCodexSessions()
	pidMap := findCodexPIDs()

	result := make([]RawSession, 0, len(sessions))
	for _, s := range sessions {
		pid := pidMap[s.CWD]
		state := inferCodexState(s.FilePath)

		mode := "headless"
		if s.Originator == "codex_tui" {
			mode = "interactive"
		}

		result = append(result, RawSession{
			Provider:   "codex",
			SessionID:  s.SessionID,
			PID:        pid,
			CWD:        s.CWD,
			FilePath:   s.FilePath,
			Mode:       mode,
			StartedAt:  s.StartedAt,
			State:      state,
			Terminated: state == StateStopped && pid == 0,
		})
	}
	return result
}
