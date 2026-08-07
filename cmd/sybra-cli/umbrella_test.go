package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUmbrella_JSONStdoutStaysClean(t *testing.T) {
	t.Parallel()
	var (
		code   int
		stdout string
	)
	stderr := captureStderr(t, func() {
		code, stdout = captureStdout(t, func() int {
			return reportUmbrella(true, "https://github.com/o/r/issues/100", 2, 1, true)
		})
	})
	if code != 0 {
		t.Fatalf("reportUmbrella exit = %d, want 0", code)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("stderr = %q, want empty for JSON mode", stderr)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\n%s", err, stdout)
		panic("unreachable")
	}
	if strings.Contains(stdout, "WARNING: planner exhausted") {
		t.Fatalf("stdout leaked human text in JSON mode:\n%s", stdout)
	}
	if got := decoded["degraded"]; got != true {
		t.Fatalf("decoded degraded = %v, want true", got)
	}
}
