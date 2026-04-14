package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractTaskID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "found after -p",
			args: []string{"claude", "-p", "work on task a1b2c3d4 now"},
			want: "a1b2c3d4",
		},
		{
			name: "no -p flag",
			args: []string{"claude", "--output-format", "stream-json"},
			want: "",
		},
		{
			name: "-p flag at end",
			args: []string{"claude", "-p"},
			want: "",
		},
		{
			name: "no hex ID in prompt",
			args: []string{"claude", "-p", "do some work please"},
			want: "",
		},
		{
			name: "empty args",
			args: []string{},
			want: "",
		},
		{
			name: "ID too short",
			args: []string{"claude", "-p", "task a1b2c3 here"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractTaskID(tt.args)
			if got != tt.want {
				t.Errorf("extractTaskID(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestPopScenario_EnvVar(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_SCENARIO", "fail_exit")
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", "")

	got := popScenario()
	if got != "fail_exit" {
		t.Errorf("popScenario() = %q, want %q", got, "fail_exit")
	}
}

func TestPopScenario_Default(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_SCENARIO", "")
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", "")

	got := popScenario()
	if got != "success" {
		t.Errorf("popScenario() = %q, want %q", got, "success")
	}
}

func TestPopScenario_File(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "scenarios.txt")
	if err := os.WriteFile(f, []byte("triage\nimplement\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", f)
	t.Setenv("FAKE_CLAUDE_SCENARIO", "")

	got := popScenario()
	if got != "triage" {
		t.Errorf("first pop = %q, want %q", got, "triage")
	}

	// Second pop should return the next scenario.
	got2 := popScenario()
	if got2 != "implement" {
		t.Errorf("second pop = %q, want %q", got2, "implement")
	}
}

func TestPopScenario_FileMissingFallsBack(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", "/nonexistent/path/scenarios.txt")
	t.Setenv("FAKE_CLAUDE_SCENARIO", "evaluate")

	got := popScenario()
	if got != "evaluate" {
		t.Errorf("popScenario() = %q, want %q", got, "evaluate")
	}
}

func TestEnvInt(t *testing.T) {
	tests := []struct {
		name string
		val  string
		def  int
		want int
	}{
		{"unset returns default", "", 42, 42},
		{"valid integer", "17", 42, 17},
		{"zero allowed", "0", 42, 0},
		{"whitespace trimmed", "  99 ", 42, 99},
		{"negative falls back", "-5", 42, 42},
		{"non-numeric falls back", "abc", 42, 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := "FAKE_CLAUDE_TEST_ENVINT"
			if tt.val == "" {
				os.Unsetenv(key)
			} else {
				t.Setenv(key, tt.val)
			}
			got := envInt(key, tt.def)
			if got != tt.want {
				t.Errorf("envInt(%q, %d) = %d, want %d", tt.val, tt.def, got, tt.want)
			}
		})
	}
}

// captureStdout swaps os.Stdout with a pipe, runs fn, and returns everything
// that was written during fn's execution. Used to verify the NDJSON output
// of the perf scenarios.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// parseNDJSON splits an NDJSON blob into decoded event maps, skipping blank
// lines so trailing newlines don't inflate counts.
func parseNDJSON(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func TestRunPerfStream_DefaultCount(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_EVENT_COUNT", "")
	os.Unsetenv("FAKE_CLAUDE_EVENT_COUNT")
	t.Setenv("FAKE_CLAUDE_EVENT_INTERVAL_MS", "0")

	out := captureStdout(t, runPerfStream)
	events := parseNDJSON(t, out)
	// 1 system + 100 assistant + 1 result = 102
	if len(events) != 102 {
		t.Fatalf("event count = %d, want 102", len(events))
	}
	if events[0]["type"] != "system" {
		t.Errorf("first event type = %v, want system", events[0]["type"])
	}
	if events[1]["type"] != "assistant" {
		t.Errorf("second event type = %v, want assistant", events[1]["type"])
	}
	if events[len(events)-1]["type"] != "result" {
		t.Errorf("last event type = %v, want result", events[len(events)-1]["type"])
	}
}

func TestRunPerfStream_CustomCount(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_EVENT_COUNT", "5")
	t.Setenv("FAKE_CLAUDE_EVENT_INTERVAL_MS", "0")

	out := captureStdout(t, runPerfStream)
	events := parseNDJSON(t, out)
	if len(events) != 7 { // 1 system + 5 assistant + 1 result
		t.Fatalf("event count = %d, want 7", len(events))
	}
}

func TestRunPerfBurst_EmitsWithoutSleep(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_EVENT_COUNT", "1000")

	start := time.Now()
	out := captureStdout(t, runPerfBurst)
	elapsed := time.Since(start)

	events := parseNDJSON(t, out)
	if len(events) != 1002 { // 1 system + 1000 assistant + 1 result
		t.Fatalf("event count = %d, want 1002", len(events))
	}
	// The legacy emit helper sleeps 10ms per event. Emitting 1000 events that
	// way would take ≥10s. Burst must complete well under 1s even on a slow CI
	// box — if this fails, perf_burst is accidentally paced.
	if elapsed > time.Second {
		t.Errorf("burst took %s, expected <1s (emitRaw must not sleep)", elapsed)
	}
}

func TestRunPerfLong_RespectsDuration(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_DURATION_MS", "200")
	t.Setenv("FAKE_CLAUDE_EVENT_INTERVAL_MS", "20")

	start := time.Now()
	out := captureStdout(t, runPerfLong)
	elapsed := time.Since(start)

	events := parseNDJSON(t, out)
	if len(events) < 3 {
		t.Fatalf("event count = %d, want ≥3 (system + assistants + result)", len(events))
	}
	if events[0]["type"] != "system" {
		t.Errorf("first event type = %v, want system", events[0]["type"])
	}
	if events[len(events)-1]["type"] != "result" {
		t.Errorf("last event type = %v, want result", events[len(events)-1]["type"])
	}
	// Duration is 200ms; allow up to 1s slack for scheduler jitter.
	if elapsed < 200*time.Millisecond {
		t.Errorf("elapsed %s < 200ms minimum", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed %s exceeded 1s slack", elapsed)
	}
}
