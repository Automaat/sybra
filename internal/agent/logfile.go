package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// ParseLogFile reads an NDJSON agent log and returns up to maxEvents
// StreamEvents (the last N if the file exceeds the cap).
// Malformed lines are silently skipped.
func ParseLogFile(path string, maxEvents int) ([]StreamEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []StreamEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		var ev StreamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type == "" {
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events, nil
}

// ParseConvoLogFile reads an NDJSON log written by an interactive agent
// (Claude stream-json wire format) and returns up to maxEvents ConvoEvents.
//
// Interactive agents persist raw Anthropic-envelope lines
// (`{"type":"assistant","message":{"content":[...]}}`). Unmarshaling those
// directly into StreamEvent (flat Content string) silently drops the
// message body — the rendered UI shows labeled bubbles with empty text.
// This function unwraps the envelope via ParseClaudeLine +
// claudeEventToConvoEvent so history replay shows the same structure live
// agents do.
//
// Malformed lines are logged at debug level and skipped. `logger` may be
// nil (falls back to slog.Default) for callers that do not carry one.
func ParseConvoLogFile(path string, maxEvents int, logger *slog.Logger) ([]ConvoEvent, error) {
	if logger == nil {
		logger = slog.Default()
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open convo log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var (
		events  []ConvoEvent
		skipped int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ce, parseErr := ParseClaudeLine(line)
		if parseErr != nil {
			skipped++
			logger.Debug("convo.log.parse-skip", "path", path, "err", parseErr)
			continue
		}
		if ce.Type == "" {
			skipped++
			continue
		}
		events = append(events, claudeEventToConvoEvent(ce))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan convo log: %w", err)
	}
	if skipped > 0 {
		logger.Info("convo.log.parsed",
			"path", path, "events", len(events), "skipped", skipped)
	}
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events, nil
}

// FindLogFile locates the NDJSON log for agentID inside logsDir/agents/ by
// globbing "{agentID}-*.ndjson". Returns the first match or an error.
func FindLogFile(logsDir, agentID string) (string, error) {
	pattern := filepath.Join(logsDir, "agents", agentID+"-*.ndjson")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no log file for agent %s", agentID)
	}
	return matches[0], nil
}
