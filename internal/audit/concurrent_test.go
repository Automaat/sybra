package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestConcurrentAppend_LineIntegrity verifies that simultaneous writes from
// multiple independent Logger instances (each with their own file handle,
// simulating separate processes) produce an NDJSON file where every line is
// valid JSON. This is the structural check that replaces reliance on PIPE_BUF
// atomicity reasoning for the hook subprocess + app concurrent-write scenario.
func TestConcurrentAppend_LineIntegrity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const goroutines = 8
	const eventsEach = 50

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Each goroutine creates its own Logger (separate file handle) to
			// simulate a separate hook subprocess writing concurrently with the app.
			l, err := NewLogger(dir)
			if err != nil {
				t.Errorf("g%d: NewLogger: %v", n, err)
				return
			}
			defer func() { _ = l.Close() }()
			for j := range eventsEach {
				if err := l.Log(Event{
					Type:   EventCodexSessionStart,
					TaskID: fmt.Sprintf("task-%d-%03d", n, j),
					Data:   map[string]any{"seq": n*eventsEach + j},
				}); err != nil {
					t.Errorf("g%d: Log %d: %v", n, j, err)
				}
			}
		}(i)
	}
	wg.Wait()

	day := time.Now().UTC().Format(time.DateOnly)
	raw, err := os.ReadFile(filepath.Join(dir, day+".ndjson"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	corrupt := 0
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Errorf("line %d is not valid JSON (concurrent-write interleave?): %v\nraw: %s", i, err, line)
			corrupt++
		}
	}
	if corrupt > 0 {
		t.Fatalf("%d of %d lines are corrupted — audit.Logger needs an interprocess write lock (flock)", corrupt, len(lines))
	}
	t.Logf("wrote %d lines from %d goroutines × %d events each — all valid JSON", len(lines), goroutines, eventsEach)
}
