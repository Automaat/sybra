package artifact

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

type ProgressEntry struct {
	Ts      time.Time `json:"ts"`
	Kind    string    `json:"kind"`
	Role    string    `json:"role,omitempty"`
	Message string    `json:"message"`
}

const (
	ProgressKindProgress = "progress"
	ProgressKindDecision = "decision"
	ProgressKindBlocker  = "blocker"
	ProgressKindFailure  = "failure"
)

var validProgressKinds = map[string]struct{}{
	ProgressKindProgress: {},
	ProgressKindDecision: {},
	ProgressKindBlocker:  {},
	ProgressKindFailure:  {},
}

func ValidProgressKind(kind string) bool {
	_, ok := validProgressKinds[kind]
	return ok
}

func ProgressKinds() []string {
	return []string{ProgressKindProgress, ProgressKindDecision, ProgressKindBlocker, ProgressKindFailure}
}

func (s *Store) AppendProgress(taskID string, entry ProgressEntry) error {
	if !ValidProgressKind(entry.Kind) {
		return fmt.Errorf("artifact: invalid progress kind %q", entry.Kind)
	}
	if entry.Ts.IsZero() {
		entry.Ts = time.Now().UTC()
	}
	return s.Append(taskID, KindProgress, entry)
}

func (s *Store) ReadProgress(taskID string) ([]ProgressEntry, error) {
	data, _, err := s.Read(taskID, KindProgress.defaultName())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var entries []ProgressEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e ProgressEntry
		if err := json.Unmarshal(line, &e); err != nil {
			slog.Warn("artifact.progress.parse-err", "task_id", taskID, "err", err)
			continue
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("artifact: scan progress: %w", err)
	}
	slices.Reverse(entries)
	return entries, nil
}
