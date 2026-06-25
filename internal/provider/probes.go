package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 10 * time.Second

// minCodexVersion is the lowest codex CLI version that ships the default model
// (gpt-5.5). Older CLIs reject `--model gpt-5.5` at exec time, so the probe
// surfaces a warning instead of letting every default run fail silently.
const minCodexVersion = "0.142.2"

var codexVersionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)*`)

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
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			return Status{Provider: "claude", Healthy: false, Reason: "probe_error", Detail: err.Error(), LastCheck: time.Now()}, err
		}
		// Fall through: claude may have printed JSON on stdout even with non-zero exit.
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
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			return Status{Provider: "codex", Healthy: false, Reason: "probe_error", Detail: err.Error(), LastCheck: time.Now()}, err
		}
	}
	st, perr := parseCodexLoginStatus(raw)
	if perr == nil && st.Healthy {
		// Reuse cctx so the login and version probes share one probeTimeout
		// budget rather than allowing ProbeCodex to run for up to 2× of it.
		if v := probeCodexVersion(cctx); v != "" && !codexVersionAtLeast(v, minCodexVersion) {
			warning := fmt.Sprintf("codex %s is older than %s; the default model gpt-5.5 requires %s+ — upgrade codex or pin an older model", v, minCodexVersion, minCodexVersion)
			if st.Detail != "" {
				st.Detail += " — " + warning
			} else {
				st.Detail = warning
			}
		}
	}
	return st, perr
}

// copilotTokenEnvVars are the env vars Copilot checks for a headless auth
// token, in precedence order (see `copilot login --help`).
var copilotTokenEnvVars = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// ProbeCopilot checks GitHub Copilot CLI liveness and reports its auth method.
//
// Copilot exposes no non-interactive auth-status subcommand (unlike `claude
// auth status` / `codex login status`), and on desktop the OAuth token lives in
// the system credential store, which is not cheaply inspectable. So the probe
// confirms the binary runs (`copilot --version`) and surfaces the auth method:
// when a token env var is present it is authoritative; otherwise auth goes
// through the credential store and cannot be verified here without spending a
// premium request. A genuinely logged-out CLI is caught at run time via the
// passive stderr signal (isLoggedOutStderr) which flips the provider unhealthy.
func ProbeCopilot(ctx context.Context) (Status, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "copilot", "--version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if isLoggedOutStderr(stderr.String()) {
			return Status{Provider: "copilot", Healthy: false, Reason: "logged_out", LastCheck: time.Now()}, nil
		}
		return Status{Provider: "copilot", Healthy: false, Reason: "probe_error", Detail: err.Error(), LastCheck: time.Now()}, err
	}
	st := Status{Provider: "copilot", Healthy: true, Reason: "ok", LastCheck: time.Now()}
	if env := copilotTokenEnvVar(); env != "" {
		st.Detail = "token: " + env
	} else {
		st.Detail = "auth via credential store (not verified)"
	}
	return st, nil
}

// copilotTokenEnvVar returns the name of the first set, non-empty Copilot auth
// token env var, or "" if none is set.
func copilotTokenEnvVar() string {
	for _, name := range copilotTokenEnvVars {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return name
		}
	}
	return ""
}

// probeCodexVersion runs `codex --version` and returns the dotted version string
// (e.g. "0.142.2"), or "" if it cannot be determined.
func probeCodexVersion(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "codex", "--version").Output()
	if err != nil {
		return ""
	}
	return codexVersionRe.FindString(string(out))
}

// codexVersionAtLeast reports whether have >= want using dotted numeric
// comparison. An unparseable `have` fails open (returns true) so a future
// version-format change never blocks an otherwise-working CLI.
func codexVersionAtLeast(have, want string) bool {
	hp := splitVersion(have)
	if hp == nil {
		return true
	}
	wp := splitVersion(want)
	for i := 0; i < len(hp) || i < len(wp); i++ {
		var h, w int
		if i < len(hp) {
			h = hp[i]
		}
		if i < len(wp) {
			w = wp[i]
		}
		if h != w {
			return h > w
		}
	}
	return true
}

// splitVersion parses a dotted version into numeric components, tolerating a
// trailing non-numeric suffix per component (e.g. "2-beta" -> 2). Returns nil
// if any component lacks a leading digit.
func splitVersion(v string) []int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			return nil
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
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
		return st, errors.New("claude auth status: empty response")
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
		return st, errors.New("codex login status: empty response")
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
		strings.Contains(lower, "not authenticated") ||
		strings.Contains(lower, "please run claude auth login") ||
		strings.Contains(lower, "please run: codex login") ||
		strings.Contains(lower, "please run codex login") ||
		strings.Contains(lower, "please run: copilot login") ||
		strings.Contains(lower, "run `copilot login`") ||
		strings.Contains(lower, "run 'copilot login'")
}
