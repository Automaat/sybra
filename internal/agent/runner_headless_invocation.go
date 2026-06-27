package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// buildHeadlessInvocation builds the subprocess invocation for a headless
// agent. The returned env slice contains "KEY=VALUE" entries the caller must
// merge into cmd.Env (Bash tool timeout for claude is delivered this way —
// claude has no CLI flag for it).
func buildHeadlessInvocation(a *Agent, cfg RunConfig) (name string, args, env []string, command string, err error) {
	if a.Provider != "claude" && a.Provider != "codex" && a.Provider != "copilot" {
		err = fmt.Errorf("unsupported provider: %s", a.Provider)
		return
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			err = fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
			return
		}
	}
	if a.Model != "" && !safeArgRe.MatchString(a.Model) {
		err = fmt.Errorf("invalid model %q: must match %s", a.Model, safeArgRe)
		return
	}
	if cfg.FallbackModel != "" && !safeArgRe.MatchString(cfg.FallbackModel) {
		err = fmt.Errorf("invalid fallback model %q: must match %s", cfg.FallbackModel, safeArgRe)
		return
	}

	if a.Provider == "codex" {
		name = "codex"
		args = []string{"exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"}
		// headless=true: --sandbox workspace-write requires approval prompts
		// which auto-reject in headless mode (no TTY/UI). Always bypass.
		args = append(args, codexSandboxArgs(cfg.RequirePermissions, true)...)
		if a.Model != "" {
			args = append(args, "--model", a.Model)
		}
		args = append(args, codexReasoningArgs(a.ReasoningEffort)...)
		if a.sessionCWD != "" {
			args = append(args, "-C", a.sessionCWD)
		}
		if cfg.outputSchemaPath != "" {
			args = append(args, "--output-schema", cfg.outputSchemaPath)
		}
		// Lifecycle hooks (fail-open: omitted when sybra-cli unresolvable or taskID unsafe).
		args = append(args, buildCodexHookArgs(a.TaskID)...)
		prompt := rewriteSkillInvocations(cfg.Prompt, discoverCodexSkills())
		args = append(args, prompt)
		command = "codex " + strings.Join(args, " ")
		return
	}

	if a.Provider == "copilot" {
		name = "copilot"
		// --allow-all-tools is required for non-interactive mode; --no-ask-user
		// stops the agent blocking on questions (no TTY/UI to answer them).
		args = []string{"-p", cfg.Prompt, "--output-format", "json", "--allow-all-tools", "--no-ask-user"}
		if a.Model != "" {
			args = append(args, "--model", a.Model)
		}
		// Copilot only reports its session id on the terminal result event, so
		// a resume id is available only for an intentional stop/restart. Use
		// --session-id (unambiguous required-value flag) over --resume[=value].
		if sid := a.GetSessionID(); sid != "" {
			args = append(args, "--session-id", sid)
		}
		command = "copilot " + strings.Join(args, " ")
		return
	}

	name = "claude"
	args = []string{"-p", cfg.Prompt, "--output-format", "stream-json", "--verbose"}
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	args = append(args, claudePermissionArgs(cfg.AllowedTools, cfg.RequirePermissions, cfg.HeadlessPermissionMode)...)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if cfg.FallbackModel != "" {
		args = append(args, "--fallback-model", cfg.FallbackModel)
	}
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
	command = "claude " + strings.Join(args, " ")
	return
}

// claudePermissionArgs returns the permission-related CLI flags for a claude headless run.
//
// Precedence:
//  1. len(allowed)>0 → --allowedTools <list> (explicit tool allowlist wins)
//  2. requirePerms → nil (approval-hook mode; no bypass or auto flag)
//  3. mode=="auto" → --permission-mode auto (auto-mode classifier)
//  4. else → --dangerously-skip-permissions (legacy bypass, default)
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
