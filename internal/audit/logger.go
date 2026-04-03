package audit

import (
	"encoding/json"
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
