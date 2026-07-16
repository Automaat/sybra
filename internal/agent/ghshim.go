package agent

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GhShimReason is what the gh shim prints to stderr, and the agent reads, when
// it refuses to submit a PR approval.
const GhShimReason = "Blocked by Sybra: agents have no PR-approval authority. " +
	"Submit a comment review (gh pr review --comment) or request changes " +
	"(gh pr review --request-changes) instead; approval is a human decision."

const ghShimScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "review" ]; then
	for arg in "$@"; do
		case "$arg" in
		--approve|-a|--approve=*)
			printf '%%s\n' '%[1]s' >&2
			exit 1
			;;
		esac
	done
fi
if [ "$1" = "api" ]; then
	for arg in "$@"; do
		case "$arg" in
		*[Ee][Vv][Ee][Nn][Tt]*[Aa][Pp][Pp][Rr][Oo][Vv][Ee]*)
			printf '%%s\n' '%[1]s' >&2
			exit 1
			;;
		esac
	done
fi
exec '%[2]s' "$@"
`

// writeGhShim materializes a `gh` wrapper in dir, for callers to prepend to an
// agent's PATH.
//
// This is the deterministic floor under the review-agent prompts: the prompts
// tell agents never to approve (semantic ceiling), and this refuses the call
// even if that instruction drifts or is dropped — a prompt is not a permission
// boundary.
//
// It matches on real argv, after the shell has already resolved quoting,
// command substitution, heredocs and aliases, so it needs no shell parsing of
// its own: a review body arrives as exactly one argv element and can never be
// mistaken for a flag, and `gh pr review $(gh pr view -q .number) --approve`
// arrives as a plain `--approve`. Matching the same intent by parsing the
// command string at the PreToolUse hook was tried first and leaked both ways —
// it missed a trailing `;` and false-denied bodies that merely mentioned `-a` —
// which is why this guards the point of execution instead.
//
// Living on PATH rather than in a provider hook, it covers every provider
// (claude, codex, copilot) and any grandchild process, which no single
// provider's hook contract can offer.
//
// Returns ("", nil) when no real gh is installed: there is nothing to guard and
// nothing to exec, and shimming a missing binary would break `gh` probes that
// already tolerate its absence.
func lookRealGh() string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	return path
}

func writeGhShim(dir string) (string, error) {
	found := lookRealGh()
	if found == "" {
		return "", nil
	}
	realGh, err := filepath.Abs(found)
	if err != nil {
		return "", fmt.Errorf("resolve gh path: %w", err)
	}
	if strings.ContainsAny(realGh, "'\n") {
		return "", fmt.Errorf("gh path %q is not shell-safe", realGh)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create gh shim dir: %w", err)
	}
	path := filepath.Join(dir, "gh")
	script := fmt.Sprintf(ghShimScript, GhShimReason, realGh)
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		return "", fmt.Errorf("write gh shim: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return "", fmt.Errorf("chmod gh shim: %w", err)
	}
	return dir, nil
}

// resolveGhShimDir materializes the shim once per manager and returns the dir
// to prepend to agent PATHs, or "" when the guard is unavailable. It logs and
// degrades rather than failing construction: the prompt still forbids approval,
// and taking the whole fleet down over a shim write is the wrong trade.
func resolveGhShimDir(dir string, logger *slog.Logger) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	resolved, err := writeGhShim(dir)
	if err != nil {
		logger.Error("agent.gh-shim.failed", "dir", dir, "err", err)
		return ""
	}
	if resolved == "" {
		logger.Info("agent.gh-shim.skipped", "reason", "no gh on PATH")
		return ""
	}
	logger.Info("agent.gh-shim.ready", "dir", resolved)
	return resolved
}

func (m *Manager) injectGhShim(cfg *RunConfig) {
	if m.ghShimDir == "" {
		m.logger.Warn("agent.gh-shim.unguarded",
			"task_id", cfg.TaskID,
			"reason", "no gh shim; prompt is the only ceiling on PR approval")
		return
	}
	cfg.ExtraEnv = prependPATH(cfg.ExtraEnv, m.ghShimDir)
}

func prependPATH(env []string, dir string) []string {
	current := os.Getenv("PATH")
	for _, kv := range env {
		if after, ok := strings.CutPrefix(kv, "PATH="); ok {
			current = after
		}
	}
	return append(stripEnvKeys(env, "PATH"), "PATH="+dir+string(os.PathListSeparator)+current)
}
