package limits

import (
	"bufio"
	"encoding/json"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const scannerBuffer = 4 * 1024 * 1024

type codexLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type            string          `json:"type"`
		Info            codexUsageInfo  `json:"info"`
		RateLimits      *codexRateLimit `json:"rate_limits"`
		RateLimitsAlias *codexRateLimit `json:"rateLimits"`
	} `json:"payload"`
}

type codexUsageInfo struct {
	TotalTokenUsage codexTokenUsage `json:"total_token_usage"`
	LastTokenUsage  codexTokenUsage `json:"last_token_usage"`
}

type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

type codexRateLimit struct {
	LimitID              string           `json:"limit_id"`
	LimitName            *string          `json:"limit_name"`
	Primary              *codexLimitCycle `json:"primary"`
	Secondary            *codexLimitCycle `json:"secondary"`
	PlanType             string           `json:"plan_type"`
	RateLimitReachedType *string          `json:"rate_limit_reached_type"`
}

type codexLimitCycle struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

func ParseCodexLine(line []byte, source, id, sessionID string) (Snapshot, UsageEvent, bool) {
	var raw codexLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return Snapshot{}, UsageEvent{}, false
	}
	capturedAt := raw.Timestamp
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	}
	rl := raw.Payload.RateLimits
	if rl == nil {
		rl = raw.Payload.RateLimitsAlias
	}
	var snapshot Snapshot
	if rl != nil {
		snapshot = Snapshot{
			Provider:             ProviderCodex,
			PlanType:             rl.PlanType,
			LimitID:              rl.LimitID,
			Source:               source,
			Confidence:           ConfidenceExact,
			CapturedAt:           capturedAt,
			Primary:              codexCycle(rl.Primary),
			Secondary:            codexCycle(rl.Secondary),
			RateLimitReachedType: ptrString(rl.RateLimitReachedType),
		}
		if rl.LimitName != nil {
			snapshot.LimitName = *rl.LimitName
		}
	}
	usage := raw.Payload.Info.LastTokenUsage
	var event UsageEvent
	if usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
		event = UsageEvent{
			ID:                   id,
			Provider:             ProviderCodex,
			Source:               source,
			SessionID:            sessionID,
			InputTokens:          usage.InputTokens,
			OutputTokens:         usage.OutputTokens,
			CacheReadInputTokens: usage.CachedInputTokens,
			ReasoningTokens:      usage.ReasoningOutputTokens,
			TotalTokens:          usage.TotalTokens,
			Timestamp:            capturedAt,
		}
	}
	return snapshot, event, rl != nil || event.ID != ""
}

func SnapshotFromCodexRaw(line []byte, source string) (Snapshot, bool) {
	s, _, ok := ParseCodexLine(line, source, "", "")
	return s, ok && s.Provider != ""
}

func codexCycle(c *codexLimitCycle) *CycleSnapshot {
	if c == nil {
		return nil
	}
	out := &CycleSnapshot{
		UsedPercent:   c.UsedPercent,
		WindowMinutes: c.WindowMinutes,
	}
	if c.ResetsAt > 0 {
		out.ResetsAt = time.Unix(c.ResetsAt, 0).UTC()
	}
	return out
}

type claudeLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	Message   *struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func ParseClaudeLine(line []byte, source, id string) (UsageEvent, bool) {
	var raw claudeLine
	if err := json.Unmarshal(line, &raw); err != nil || raw.Message == nil || raw.Message.Usage == nil {
		return UsageEvent{}, false
	}
	ts := raw.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return UsageEvent{
		ID:                       id,
		Provider:                 ProviderClaude,
		Source:                   source,
		SessionID:                raw.SessionID,
		Model:                    raw.Message.Model,
		InputTokens:              raw.Message.Usage.InputTokens,
		OutputTokens:             raw.Message.Usage.OutputTokens,
		CacheCreationInputTokens: raw.Message.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     raw.Message.Usage.CacheReadInputTokens,
		Timestamp:                ts,
	}, true
}

type copilotLine struct {
	Type  string `json:"type"`
	Usage *struct {
		InputTokens      int `json:"inputTokens"`
		OutputTokens     int `json:"outputTokens"`
		CacheReadTokens  int `json:"cacheReadTokens"`
		CacheWriteTokens int `json:"cacheWriteTokens"`
		ReasoningTokens  int `json:"reasoningTokens"`
	} `json:"usage"`
	Timestamp time.Time `json:"timestamp"`
}

func ParseCopilotLine(line []byte, source, id, sessionID string) (UsageEvent, bool) {
	var raw copilotLine
	if err := json.Unmarshal(line, &raw); err != nil || raw.Usage == nil {
		return UsageEvent{}, false
	}
	ts := raw.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return UsageEvent{
		ID:                       id,
		Provider:                 ProviderCopilot,
		Source:                   source,
		SessionID:                sessionID,
		InputTokens:              raw.Usage.InputTokens,
		OutputTokens:             raw.Usage.OutputTokens,
		CacheReadInputTokens:     raw.Usage.CacheReadTokens,
		CacheCreationInputTokens: raw.Usage.CacheWriteTokens,
		ReasoningTokens:          raw.Usage.ReasoningTokens,
		TotalTokens:              raw.Usage.InputTokens + raw.Usage.OutputTokens + raw.Usage.CacheReadTokens + raw.Usage.CacheWriteTokens,
		Timestamp:                ts,
	}, true
}

// BackfillLocalSessionFiles imports usage from local provider session files
// newer than cutoff. It is best-effort: unreadable provider directories are
// skipped so one missing CLI never disables Sybra stats.
func (s *Store) BackfillLocalSessionFiles(cutoff time.Time) error {
	home, ok := userHomeDir()
	if !ok {
		return nil
	}
	batch := newSessionImport()
	if err := batch.backfillCodex(filepath.Join(home, ".codex", "sessions"), cutoff); err != nil {
		return err
	}
	if err := batch.backfillClaude(filepath.Join(home, ".claude", "projects"), cutoff); err != nil {
		return err
	}
	if err := batch.backfillCopilot(filepath.Join(home, ".copilot", "session-state"), cutoff); err != nil {
		return err
	}
	return s.Import(batch.events, batch.snapshotsList())
}

type sessionImport struct {
	events    []UsageEvent
	eventIDs  map[string]struct{}
	snapshots map[string]Snapshot
}

func newSessionImport() *sessionImport {
	return &sessionImport{
		eventIDs:  map[string]struct{}{},
		snapshots: map[string]Snapshot{},
	}
}

func (b *sessionImport) addEvent(event UsageEvent) {
	if event.ID == "" || event.Provider == "" {
		return
	}
	if _, ok := b.eventIDs[event.ID]; ok {
		return
	}
	b.eventIDs[event.ID] = struct{}{}
	b.events = append(b.events, event)
}

func (b *sessionImport) addSnapshot(snapshot Snapshot) {
	if snapshot.Provider == "" {
		return
	}
	prev, ok := b.snapshots[snapshot.Provider]
	if ok && snapshot.CapturedAt.Before(prev.CapturedAt) {
		return
	}
	b.snapshots[snapshot.Provider] = snapshot
}

func (b *sessionImport) snapshotsList() []Snapshot {
	if len(b.snapshots) == 0 {
		return nil
	}
	out := make([]Snapshot, 0, len(b.snapshots))
	for provider := range b.snapshots {
		out = append(out, b.snapshots[provider])
	}
	return out
}

func (b *sessionImport) backfillCodex(root string, cutoff time.Time) error {
	return walkJSONL(root, cutoff, func(path string, offset int64, line []byte) error {
		sessionID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "rollout-"), ".jsonl")
		snapshot, event, ok := ParseCodexLine(line, SourceSessionFiles, eventID(ProviderCodex, path, offset), sessionID)
		if !ok {
			return nil
		}
		if snapshot.Provider != "" {
			b.addSnapshot(snapshot)
		}
		if event.ID != "" {
			b.addEvent(event)
		}
		return nil
	})
}

func (b *sessionImport) backfillClaude(root string, cutoff time.Time) error {
	return walkJSONL(root, cutoff, func(path string, offset int64, line []byte) error {
		event, ok := ParseClaudeLine(line, SourceSessionFiles, eventID(ProviderClaude, path, offset))
		if !ok {
			return nil
		}
		b.addEvent(event)
		return nil
	})
}

func (b *sessionImport) backfillCopilot(root string, cutoff time.Time) error {
	return walkJSONL(root, cutoff, func(path string, offset int64, line []byte) error {
		sessionID := filepath.Base(filepath.Dir(path))
		event, ok := ParseCopilotLine(line, SourceSessionFiles, eventID(ProviderCopilot, path, offset), sessionID)
		if !ok {
			return nil
		}
		b.addEvent(event)
		return nil
	})
}

func walkJSONL(root string, cutoff time.Time, fn func(path string, offset int64, line []byte) error) error {
	if _, ok := sessionRootInfo(root); !ok {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if walkEntryUnreadable(err) || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		modTime, ok := entryModTime(d)
		if !ok || (!cutoff.IsZero() && modTime.Before(cutoff)) {
			return nil
		}
		return scanLines(path, fn)
	})
}

func scanLines(path string, fn func(path string, offset int64, line []byte) error) error {
	f, ok := openSessionFile(path)
	if !ok {
		return nil
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	var offset int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			startOffset := offset
			offset += int64(len(line))
			trimmed := strings.TrimRight(string(line), "\r\n")
			if len(trimmed) <= scannerBuffer {
				if err := fn(path, startOffset, []byte(trimmed)); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func eventID(provider, path string, offset int64) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(path))
	return provider + ":" + strconv.FormatUint(hash.Sum64(), 16) + ":" + strconv.FormatInt(offset, 10)
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func userHomeDir() (home string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return home, true
}

func sessionRootInfo(root string) (info os.FileInfo, ok bool) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, false
	}
	return info, true
}

func walkEntryUnreadable(err error) bool {
	return err != nil
}

func entryModTime(d os.DirEntry) (modTime time.Time, ok bool) {
	info, err := d.Info()
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func openSessionFile(path string) (file *os.File, ok bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	return file, true
}
