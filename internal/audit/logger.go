package audit

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	dir     string
	mu      sync.Mutex
	current *os.File
	today   string
}

func NewLogger(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Logger{dir: dir}, nil
}

func (l *Logger) Log(e Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}

	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.file(e.Timestamp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

// LogEvent records a structured audit event, silently no-oping when al is
// nil. A logging failure is reported to fallback rather than returned, since
// callers on the dispatch path (agent start, chat rollback, ...) treat audit
// logging as best-effort and must not fail the operation it's recording.
func LogEvent(al *Logger, fallback *slog.Logger, eventType, taskID, agentID string, data map[string]any) {
	if al == nil {
		return
	}
	if err := al.Log(Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		fallback.Error("audit.log", "type", eventType, "err", err)
	}
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current != nil {
		return l.current.Close()
	}
	return nil
}

func (l *Logger) file(ts time.Time) (*os.File, error) {
	day := ts.Format(time.DateOnly)
	if l.current != nil && l.today == day {
		return l.current, nil
	}

	if l.current != nil {
		_ = l.current.Close()
	}

	path := filepath.Join(l.dir, day+".ndjson")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	l.current = f
	l.today = day
	return f, nil
}
