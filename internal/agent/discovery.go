package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Automaat/synapse/internal/fsutil"
)

const (
	tailReadBytes     = 8192
	scannerInitialBuf = 64 * 1024
	scannerMaxBuf     = 256 * 1024
)

type claudeSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

var nonAlphanumDash = regexp.MustCompile(`[^a-zA-Z0-9-]`)

func (m *Manager) DiscoverAgents() []*Agent {
	claudeSessions := readClaudeSessions()
	codexSessions := readCodexSessions()
	m.refreshTracked()
	discovered := m.discoverNew(claudeSessions)
	discovered = append(discovered, m.discoverNewCodex(codexSessions)...)
	return discovered
}

// refreshTracked updates state of already-tracked external agents.
// I/O (process checks, session file reads) happens outside the mutex to
// avoid blocking concurrent callers behind disk reads.
func (m *Manager) refreshTracked() {
	type snap struct {
		a         *Agent
		pid       int
		cwd       string
		sessionID string
		provider  string
		filePath  string
	}

	m.mu.RLock()
	snaps := make([]snap, 0, len(m.agents))
	for _, a := range m.agents {
		if !a.External {
			continue
		}
		snaps = append(snaps, snap{
			a:         a,
			pid:       a.PID,
			cwd:       a.sessionCWD,
			sessionID: a.GetSessionID(),
			provider:  a.Provider,
			filePath:  a.GetSessionFilePath(),
		})
	}
	m.mu.RUnlock()

	for _, s := range snaps {
		var next State
		if s.provider == "codex" {
			next = refreshCodexAgent(s.pid, s.filePath)
		} else {
			if !processAlive(s.pid) {
				next = StateStopped
			} else {
				next = inferState(s.cwd, s.sessionID)
			}
		}
		s.a.SetState(next)
	}
}

func refreshCodexAgent(pid int, filePath string) State {
	if pid != 0 && processAlive(pid) {
		if filePath != "" {
			return inferCodexState(filePath)
		}
		return StateRunning
	}
	if pid != 0 {
		// Had a PID, process is now dead.
		return StateStopped
	}
	// No PID: use file mod time as proxy.
	if filePath != "" {
		return inferCodexState(filePath)
	}
	return StateIdle
}

// discoverNew registers and returns external agents not yet tracked.
func (m *Manager) discoverNew(sessions []claudeSession) []*Agent {
	m.mu.RLock()
	trackedPIDs := make(map[int]bool)
	for _, a := range m.agents {
		if a.cmd != nil && a.cmd.Process != nil {
			trackedPIDs[a.cmd.Process.Pid] = true
		}
		if a.PID != 0 {
			trackedPIDs[a.PID] = true
		}
	}
	m.mu.RUnlock()

	var discovered []*Agent
	for _, s := range sessions {
		if trackedPIDs[s.PID] {
			continue
		}
		if !processAlive(s.PID) {
			continue
		}

		a := &Agent{
			ID:         fmt.Sprintf("ext-%d", s.PID),
			Mode:       sessionKind(s.Kind),
			State:      inferState(s.CWD, s.SessionID),
			External:   true,
			PID:        s.PID,
			SessionID:  s.SessionID,
			StartedAt:  time.UnixMilli(s.StartedAt).UTC(),
			Name:       s.Name,
			Project:    projectName(s.CWD),
			sessionCWD: s.CWD,
		}

		m.mu.Lock()
		if _, exists := m.agents[a.ID]; !exists {
			m.agents[a.ID] = a
		}
		m.mu.Unlock()

		discovered = append(discovered, a)
	}
	return discovered
}

const staleThreshold = 10 * time.Second

type sessionState struct {
	msgType    string
	hasToolUse bool
	stale      bool
}

func inferState(cwd, sessionID string) State {
	ss := readSessionState(cwd, sessionID)

	switch {
	case ss.msgType == "system" || ss.msgType == "":
		return StateIdle
	case ss.msgType == "assistant" && ss.hasToolUse && ss.stale:
		return StatePaused
	case ss.stale:
		return StateIdle
	default:
		return StateRunning
	}
}

func readSessionState(cwd, sessionID string) sessionState {
	if cwd == "" || sessionID == "" {
		return sessionState{}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return sessionState{}
	}

	projectKey := nonAlphanumDash.ReplaceAllString(cwd, "-")
	path := filepath.Join(home, ".claude", "projects", projectKey, sessionID+".jsonl")

	return readLastJSONL(path)
}

func readLastJSONL(path string) sessionState {
	f, err := os.Open(path)
	if err != nil {
		return sessionState{}
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return sessionState{}
	}

	stale := time.Since(info.ModTime()) > staleThreshold

	// Read last 8KB — enough for the last JSONL entry
	offset := max(info.Size()-tailReadBytes, 0)
	if _, err := f.Seek(offset, 0); err != nil {
		return sessionState{}
	}

	var lastLine string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scannerInitialBuf), scannerMaxBuf)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) != "" {
			lastLine = line
		}
	}

	if lastLine == "" {
		return sessionState{}
	}

	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lastLine), &msg); err != nil {
		return sessionState{}
	}

	hasToolUse := false
	for _, c := range msg.Message.Content {
		if c.Type == "tool_use" {
			hasToolUse = true
			break
		}
	}

	return sessionState{
		msgType:    msg.Type,
		hasToolUse: hasToolUse,
		stale:      stale,
	}
}

func readClaudeSessionByPID(pidStr string) claudeSession {
	home, err := os.UserHomeDir()
	if err != nil {
		return claudeSession{}
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "sessions", pidStr+".json"))
	if err != nil {
		return claudeSession{}
	}
	var s claudeSession
	if err := json.Unmarshal(data, &s); err != nil {
		return claudeSession{}
	}
	return s
}

func readClaudeSessions() []claudeSession {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	dir := filepath.Join(home, ".claude", "sessions")
	paths, err := fsutil.ListFiles(dir, ".json")
	if err != nil {
		return nil
	}

	var sessions []claudeSession
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		var s claudeSession
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if s.PID == 0 {
			continue
		}

		sessions = append(sessions, s)
	}
	return sessions
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func projectName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

func sessionKind(kind string) string {
	if kind == "headless" {
		return "headless"
	}
	return "interactive"
}

// codexSessionMaxAge is the cutoff for Codex session discovery.
// Sessions older than this are almost certainly dead.
const codexSessionMaxAge = 24 * time.Hour

type codexSession struct {
	SessionID  string
	CWD        string
	Originator string // "codex_exec" or "codex_tui"
	StartedAt  time.Time
	FilePath   string
	Branch     string
}

func readCodexSessions() []codexSession {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return readCodexSessionsFromDir(filepath.Join(home, ".codex", "sessions"))
}

func readCodexSessionsFromDir(sessDir string) []codexSession {
	root, err := os.OpenRoot(sessDir)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()

	type candidate struct {
		path    string
		relPath string
	}
	var candidates []candidate
	_ = filepath.WalkDir(sessDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if time.Since(info.ModTime()) > codexSessionMaxAge {
			return nil
		}
		relPath, err := filepath.Rel(sessDir, path)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{path: path, relPath: relPath})
		return nil
	})

	var sessions []codexSession
	for _, c := range candidates {
		f, err := root.Open(c.relPath)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		if !scanner.Scan() {
			_ = f.Close()
			continue
		}
		line := make([]byte, len(scanner.Bytes()))
		copy(line, scanner.Bytes())
		_ = f.Close()

		var meta struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Payload   struct {
				ID         string `json:"id"`
				CWD        string `json:"cwd"`
				Originator string `json:"originator"`
				Git        struct {
					Branch string `json:"branch"`
				} `json:"git"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &meta); err != nil {
			continue
		}
		if meta.Type != "session_meta" {
			continue
		}
		sessions = append(sessions, codexSession{
			SessionID:  meta.Payload.ID,
			CWD:        meta.Payload.CWD,
			Originator: meta.Payload.Originator,
			StartedAt:  meta.Timestamp,
			FilePath:   c.path,
			Branch:     meta.Payload.Git.Branch,
		})
	}
	return sessions
}

type codexEventState struct {
	eventType   string
	payloadType string
	stale       bool
}

func readLastCodexEvent(filePath string) codexEventState {
	f, err := os.Open(filePath)
	if err != nil {
		return codexEventState{}
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return codexEventState{}
	}

	stale := time.Since(info.ModTime()) > staleThreshold

	offset := max(info.Size()-tailReadBytes, 0)
	if _, err := f.Seek(offset, 0); err != nil {
		return codexEventState{}
	}

	var lastLine string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scannerInitialBuf), scannerMaxBuf)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) != "" {
			lastLine = line
		}
	}

	if lastLine == "" {
		return codexEventState{stale: stale}
	}

	var ev struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lastLine), &ev); err != nil {
		return codexEventState{stale: stale}
	}

	return codexEventState{
		eventType:   ev.Type,
		payloadType: ev.Payload.Type,
		stale:       stale,
	}
}

func inferCodexState(filePath string) State {
	ev := readLastCodexEvent(filePath)

	switch ev.eventType {
	case "event_msg":
		switch ev.payloadType {
		case "task_complete", "turn_aborted":
			return StateStopped
		case "task_started":
			if !ev.stale {
				return StateRunning
			}
			return StateIdle
		case "agent_message":
			if !ev.stale {
				return StateRunning
			}
			return StateIdle
		default:
			// token_count and anything else → idle
			return StateIdle
		}
	case "response_item":
		if !ev.stale {
			return StateRunning
		}
		return StatePaused
	}
	// session_meta only, empty, or unknown event type
	return StateIdle
}

// findCodexPIDs returns a CWD→PID map for running codex processes (macOS only).
func findCodexPIDs() map[string]int {
	out, err := exec.Command("pgrep", "-f", "codex").Output()
	if err != nil {
		return nil
	}

	result := make(map[string]int)
	for pidStr := range strings.FieldsSeq(string(out)) {
		lsofOut, err := exec.Command("lsof", "-a", "-d", "cwd", "-Fn", "-p", pidStr).Output()
		if err != nil {
			continue
		}
		cwd := parseLsofCWD(string(lsofOut))
		if cwd == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil || pid == 0 {
			continue
		}
		result[cwd] = pid
	}
	return result
}

func parseLsofCWD(output string) string {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if line == "fcwd" && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "n") {
			return lines[i+1][1:]
		}
	}
	return ""
}

func (m *Manager) discoverNewCodex(sessions []codexSession) []*Agent {
	m.mu.RLock()
	trackedPaths := make(map[string]bool)
	trackedSessionIDs := make(map[string]bool)
	for _, a := range m.agents {
		if fp := a.GetSessionFilePath(); fp != "" {
			trackedPaths[fp] = true
		}
		if sid := a.GetSessionID(); sid != "" {
			trackedSessionIDs[sid] = true
		}
	}
	m.mu.RUnlock()

	pidMap := findCodexPIDs()

	var discovered []*Agent
	for _, s := range sessions {
		if trackedPaths[s.FilePath] || trackedSessionIDs[s.SessionID] {
			continue
		}

		state := inferCodexState(s.FilePath)
		pid := pidMap[s.CWD]

		// Skip sessions that are dead with no associated process.
		if state == StateStopped && pid == 0 {
			continue
		}

		mode := "headless"
		if s.Originator == "codex_tui" {
			mode = "interactive"
		}

		shortID := s.SessionID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		a := &Agent{
			ID:              fmt.Sprintf("ext-codex-%s", shortID),
			Mode:            mode,
			State:           state,
			External:        true,
			PID:             pid,
			SessionID:       s.SessionID,
			StartedAt:       s.StartedAt,
			Project:         projectName(s.CWD),
			Provider:        "codex",
			sessionCWD:      s.CWD,
			sessionFilePath: s.FilePath,
		}

		m.mu.Lock()
		if _, exists := m.agents[a.ID]; !exists {
			m.agents[a.ID] = a
		}
		m.mu.Unlock()

		discovered = append(discovered, a)
	}
	return discovered
}

// resolveCodexSessionFile returns the JSONL path for a given Codex session ID.
// It walks ~/.codex/sessions/ looking for rollout-{sessionID}.jsonl.
func resolveCodexSessionFile(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return resolveCodexSessionFileInDir(filepath.Join(home, ".codex", "sessions"), sessionID)
}

func resolveCodexSessionFileInDir(sessDir, sessionID string) string {
	target := "rollout-" + sessionID + ".jsonl"
	var result string
	_ = filepath.WalkDir(sessDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(path) == target {
			result = path
			return filepath.SkipAll
		}
		return nil
	})
	return result
}
