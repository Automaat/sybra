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
	if a.Provider != "claude" && a.Provider != "codex" {
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

	if a.Provider == "codex" {
		name = "codex"
		args = []string{"exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"}
		// headless=true: --sandbox workspace-write requires approval prompts
		// which auto-reject in headless mode (no TTY/UI). Always bypass.
		args = append(args, codexSandboxArgs(cfg.RequirePermissions, true)...)
		if a.Model != "" {
			args = append(args, "--model", a.Model)
		}
		if a.sessionCWD != "" {
			args = append(args, "-C", a.sessionCWD)
		}
		prompt := rewriteSkillInvocations(cfg.Prompt, discoverCodexSkills())
		args = append(args, prompt)
		command = "codex " + strings.Join(args, " ")
		return
	}

	name = "claude"
	args = []string{"-p", cfg.Prompt, "--output-format", "stream-json", "--verbose"}
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	if len(cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	} else if !cfg.RequirePermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
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
	command = "claude " + strings.Join(args, " ")
	return
}
