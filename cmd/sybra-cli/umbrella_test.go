package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
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
	}
	if strings.Contains(stdout, "WARNING: planner exhausted") {
		t.Fatalf("stdout leaked human text in JSON mode:\n%s", stdout)
	}
	if got := decoded["degraded"]; got != true {
		t.Fatalf("decoded degraded = %v, want true", got)
	}
}

// TestCmdUmbrella_SendsTheModelToTheServer keeps --model meaningful against a
// running instance. Dropping it would expand with the server's default and
// still print success, leaving no sign the flag was ignored.
func TestCmdUmbrella_SendsTheModelToTheServer(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"umbrellaUrl":"https://github.com/o/r/issues/1","created":1}`))
	}))
	t.Cleanup(srv.Close)

	api := &apiClient{baseURL: srv.URL, token: "t", http: srv.Client()}
	code, _ := captureStdout(t, func() int {
		return cmdUmbrella(config.DefaultConfig(), api, nil, nil,
			[]string{"--model", "claude-opus-5", "https://github.com/o/r/issues/1"}, true)
	})
	if code != 0 {
		t.Fatalf("umbrella exit = %d, want 0", code)
	}
	if !strings.Contains(string(body), "claude-opus-5") {
		t.Errorf("request body %s does not carry the requested model", body)
	}
}
