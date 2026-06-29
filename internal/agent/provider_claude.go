package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	providerpkg "github.com/Automaat/sybra/internal/provider"
)

type claudeProvider struct {
	baseProvider
}

func init() {
	registerAgentProvider(claudeProvider{})
}

func (claudeProvider) Name() string { return "claude" }

func (claudeProvider) NormalizeModel(model string) string {
	// [1m] is a Claude-Code-only context marker. Fable 5 ships a 1M context
	// window by default, so CC 2.1.173 strips the redundant suffix; Sybra
	// exposes no 1M variants, so the marker is always redundant here.
	// Stripping before safeArgRe keeps the validator strict — it intentionally
	// rejects '[' and ']'. Scoped to the Claude path; Codex strings untouched.
	model = stripContextSuffix(model)
	if strings.TrimSpace(model) == "" {
		return "sonnet"
	}
	return model
}

func (claudeProvider) BuildCommand(cfg RunConfig, model string) string {
	return buildClaudeCommand(model, cfg.ReasoningEffort, cfg.AllowedTools, cfg.RequirePermissions, cfg.HeadlessPermissionMode)
}

func (claudeProvider) BuildHeadlessInvocation(a *Agent, cfg RunConfig) (headlessInvocation, error) {
	args := []string{"-p", cfg.Prompt, "--output-format", "stream-json", "--verbose"}
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	args = append(args, claudePermissionArgs(cfg.AllowedTools, cfg.RequirePermissions, cfg.HeadlessPermissionMode)...)
	args = append(args, effortArgs(a.ReasoningEffort)...)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if cfg.FallbackModel != "" {
		args = append(args, "--fallback-model", cfg.FallbackModel)
	}
	var env []string
	if cfg.BashTimeoutMs > 0 {
		// Claude has no `--bashTimeoutMs` CLI flag — the supported channel is
		// the BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS env vars. Set both
		// so the configured value is honored even when it exceeds claude's
		// built-in max (600000 ms).
		ms := strconv.Itoa(cfg.BashTimeoutMs)
		env = append(env,
			"BASH_DEFAULT_TIMEOUT_MS="+ms,
			"BASH_MAX_TIMEOUT_MS="+ms,
		)
	}
	if cfg.ForkSubagent {
		env = append(env, "CLAUDE_CODE_FORK_SUBAGENT=1")
	}
	if cfg.RetryWatchdog > 0 {
		// CLAUDE_CODE_RETRY_WATCHDOG replaces CLAUDE_CODE_MAX_RETRIES (now
		// capped at 15) for unattended/server headless runs. Delivered via env
		// because claude has no equivalent CLI flag.
		env = append(env, "CLAUDE_CODE_RETRY_WATCHDOG="+strconv.Itoa(cfg.RetryWatchdog))
	}
	return headlessInvocation{
		name:    "claude",
		args:    args,
		env:     env,
		command: "claude " + strings.Join(args, " "),
	}, nil
}

func (claudeProvider) ParseHeadlessLine(line []byte) (StreamEvent, error) {
	ce, err := ParseClaudeLine(line)
	if err != nil {
		return StreamEvent{}, err
	}
	return claudeEventToStreamEvent(ce), nil
}

func (claudeProvider) ClassifyError(sample providerpkg.ErrorSample) (providerpkg.Signal, string, time.Duration) {
	return providerpkg.ClassifyClaudeError(sample)
}

// buildClaudeCommand builds the display command string for a Claude agent.
func buildClaudeCommand(model, effort string, allowedTools []string, requirePerms bool, mode string) string {
	parts := []string{"claude"}
	parts = append(parts, claudePermissionArgs(allowedTools, requirePerms, mode)...)
	parts = append(parts, effortArgs(effort)...)
	if model != "" {
		parts = append(parts, "--model", model)
	}
	return strings.Join(parts, " ")
}

// effortArgs returns the reasoning-effort CLI flag shared by the claude and
// copilot CLIs (`--effort <level>`), or an empty slice when effort is empty so
// the model default is used. Codex uses a different surface — see codexReasoningArgs.
func effortArgs(effort string) []string {
	if effort == "" {
		return []string{}
	}
	return []string{"--effort", effort}
}

// claudePermissionArgs returns the permission-related CLI flags for a claude headless run.
//
// Precedence:
//  1. len(allowed)>0 -> --allowedTools <list> (explicit tool allowlist wins)
//  2. requirePerms -> nil (approval-hook mode; no bypass or auto flag)
//  3. mode=="auto" -> --permission-mode auto (auto-mode classifier)
//  4. else -> --dangerously-skip-permissions (legacy bypass, default)
func claudePermissionArgs(allowed []string, requirePerms bool, mode string) []string {
	if len(allowed) > 0 {
		return []string{"--allowedTools", strings.Join(allowed, ",")}
	}
	if requirePerms {
		return nil
	}
	if mode == "auto" {
		return []string{"--permission-mode", "auto"}
	}
	return []string{"--dangerously-skip-permissions"}
}

var oneMSuffixRe = regexp.MustCompile(`(?i)\[1m\]$`)

func stripContextSuffix(model string) string {
	return oneMSuffixRe.ReplaceAllString(strings.TrimSpace(model), "")
}
