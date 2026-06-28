package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseClaudeLine_ConvoRawIsIndependentOfScannerBuffer locks in the fix
// for a JSON marshal crash observed in production:
//
//	json: error calling MarshalJSON for type json.RawMessage:
//	invalid character '{' after top-level value
//
// Root cause: ConvoEvent.Raw aliased bufio.Scanner's internal buffer,
// which is reused when the scanner refills it. A ConvoEvent stored in
// the agent's buffer would later contain whatever line the scanner
// was reading next — often two concatenated JSON objects — and fail
// validation when the frontend tried to marshal the event.
//
// The test forces the scanner to refill its buffer (by using a small
// buffer relative to the total input) so that the first line's memory
// is overwritten by subsequent reads. ParseClaudeLine must copy the raw
// bytes so ClaudeEvent.Raw stays valid after the scanner advances.
func TestParseClaudeLine_ConvoRawIsIndependentOfScannerBuffer(t *testing.T) {
	// Each line is large enough that the scanner's tiny buffer (below)
	// must shift+refill between lines, evicting the previous line's
	// bytes from the memory region that scanner.Bytes() returned.
	mkLine := func(id string, fill int) string {
		return `{"type":"assistant","session_id":"` + id +
			`","payload":"` + strings.Repeat("x", fill) + `"}`
	}
	line1 := mkLine("s-one", 256)
	line2 := mkLine("s-two", 256)
	line3 := mkLine("s-three", 256)

	input := line1 + "\n" + line2 + "\n" + line3 + "\n"

	scanner := bufio.NewScanner(strings.NewReader(input))
	// Deliberately smaller than one line to force repeated refills.
	scanner.Buffer(make([]byte, 0, 64), 4096)

	var events []ClaudeEvent
	for scanner.Scan() {
		ev, err := ParseClaudeLine(scanner.Bytes())
		if err != nil {
			t.Fatalf("ParseClaudeLine: %v", err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}

	// Each Raw must still marshal cleanly through encoding/json and
	// equal the original line — this is the exact path that blew up
	// in production when Raw aliased the scanner buffer.
	for i, want := range []string{line1, line2, line3} {
		got, err := json.Marshal(events[i].Raw)
		if err != nil {
			t.Fatalf("marshal event[%d].Raw: %v", i, err)
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Errorf("event[%d].Raw = %s\nwant %s", i, got, want)
		}
	}
}

func TestStartConvoProcessReplacesStalePipeOnRetry(t *testing.T) {
	dir := t.TempDir()
	fakeClaude := filepath.Join(dir, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, _ := newTestManager(t)
	a := &Agent{ID: "retry-convo", Mode: "interactive"}
	cfg := RunConfig{Prompt: "hello"}

	cmd, stdout, _, err := m.startConvoProcess(context.Background(), a, cfg)
	if err != nil {
		t.Fatalf("first startConvoProcess: %v", err)
	}
	if _, err := io.ReadAll(stdout); err != nil {
		t.Fatalf("read first stdout: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if !a.convo.hasStdinPipe() {
		t.Fatal("stale stdin pipe was not retained for retry setup")
	}

	cmd, stdout, _, err = m.startConvoProcess(context.Background(), a, cfg)
	if err != nil {
		t.Fatalf("retry startConvoProcess: %v", err)
	}
	if _, err := io.ReadAll(stdout); err != nil {
		t.Fatalf("read retry stdout: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("retry wait: %v", err)
	}
	a.convo.closeStdinPipe()
}
