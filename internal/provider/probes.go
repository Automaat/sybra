package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const probeTimeout = 10 * time.Second

// ProbeClaude runs `claude auth status --json` and maps the result to a Status.
// A non-zero exit combined with "not logged in" stderr is treated as a logged-out
// state rather than an error.
func ProbeClaude(ctx context.Context) (Status, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", "auth", "status", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if isLoggedOutStderr(stderr.String()) {
			return Status{Provider: "claude", Healthy: false, Reason: "logged_out", LastCheck: time.Now()}, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Fall through: claude may have printed JSON on stdout even with non-zero exit.
		} else {
			return Status{Provider: "claude", Healthy: false, Reason: "probe_error", Detail: err.Error(), LastCheck: time.Now()}, err
		}
	}
	return parseClaudeAuthStatus(stdout.Bytes())
}

// ProbeCodex runs `codex login status` and maps the text output to a Status.
func ProbeCodex(ctx context.Context) (Status, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "codex", "login", "status")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	raw := stdout.Bytes()
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = stderr.Bytes()
	}
	if err != nil {
		if isLoggedOutStderr(stderr.String()) || isLoggedOutStderr(stdout.String()) {
			return Status{Provider: "codex", Healthy: false, Reason: "logged_out", LastCheck: time.Now()}, nil
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return Status{Provider: "codex", Healthy: false, Reason: "probe_error", Detail: err.Error(), LastCheck: time.Now()}, err
		}
	}
	return parseCodexLoginStatus(raw)
}

type claudeAuthStatusJSON struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	Email            string `json:"email"`
	SubscriptionType string `json:"subscriptionType"`
}

func parseClaudeAuthStatus(raw []byte) (Status, error) {
	st := Status{Provider: "claude", LastCheck: time.Now()}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		st.Reason = "probe_error"
		st.Detail = "empty response"
		return st, fmt.Errorf("claude auth status: empty response")
	}
	var payload claudeAuthStatusJSON
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		st.Reason = "probe_error"
		st.Detail = err.Error()
		return st, fmt.Errorf("claude auth status: parse json: %w", err)
	}
	if !payload.LoggedIn {
		st.Reason = "logged_out"
		return st, nil
	}
	st.Healthy = true
	st.Reason = "ok"
	if payload.SubscriptionType != "" {
		st.Detail = "subscription: " + payload.SubscriptionType
	}
	return st, nil
}

func parseCodexLoginStatus(raw []byte) (Status, error) {
	st := Status{Provider: "codex", LastCheck: time.Now()}
	text := strings.ToLower(strings.TrimSpace(string(raw)))
	if text == "" {
		st.Reason = "probe_error"
		st.Detail = "empty response"
		return st, fmt.Errorf("codex login status: empty response")
	}
	if strings.Contains(text, "not logged in") || strings.Contains(text, "please run: codex login") || strings.Contains(text, "please run codex login") {
		st.Reason = "logged_out"
		return st, nil
	}
	if strings.Contains(text, "logged in") {
		st.Healthy = true
		st.Reason = "ok"
		st.Detail = strings.TrimSpace(string(raw))
		return st, nil
	}
	st.Reason = "probe_error"
	st.Detail = strings.TrimSpace(string(raw))
	return st, fmt.Errorf("codex login status: unrecognized output %q", st.Detail)
}

func isLoggedOutStderr(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "please run claude auth login") ||
		strings.Contains(lower, "please run: codex login") ||
		strings.Contains(lower, "please run codex login")
}
