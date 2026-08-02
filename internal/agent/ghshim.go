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
// it refuses to submit a PR review event.
//
// Only APPROVE is blocked: approval authority is human-only because it can
// satisfy a required-reviewer gate on the operator's account. REQUEST_CHANGES
// and COMMENT are constructive feedback, not authority, so the shim lets those
// through to the real gh binary.
const GhShimReason = "Blocked by Sybra: APPROVE is a human decision. " +
	"Use --request-changes/--comment (or gh api -f event=REQUEST_CHANGES/COMMENT) to submit feedback, " +
	"or gh api without an event to leave the review pending for a human to approve."

const ghShimScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "review" ]; then
	sawapprove=0
	sawverdict=0
	sawunknown=0
	skipval=0
	for arg in "$@"; do
		# The previous token was a value-taking flag in its separate form, so
		# this token is its value (a body, file path or repo) — never a flag.
		# Classifying it would misread a body that starts with '-' (e.g. a
		# markdown bullet or "-1, ...") as an unknown short-flag bundle.
		if [ "$skipval" = 1 ]; then
			skipval=0
			continue
		fi
		case "$arg" in
		--approve | --approve=*)
			sawapprove=1
			;;
		--comment | --comment=* | --request-changes | --request-changes=*)
			sawverdict=1
			;;
		--body | --body-file | --repo)
			skipval=1
			;;
		--body=* | --body-file=* | --repo=* | --help)
			;;
		-*)
			rest=${arg#-}
			case "$rest" in
			*[!abcrFR]*)
				sawunknown=1
				;;
			*)
				case "$rest" in *a*) sawapprove=1 ;; esac
				case "$rest" in *c*) sawverdict=1 ;; esac
				case "$rest" in *r*) sawverdict=1 ;; esac
				# A short bundle ending in a value-taking flag (-b/-F/-R) takes
				# the next argv token as its value; skip classifying it.
				case "$rest" in *[bFR]) skipval=1 ;; esac
				;;
			esac
			;;
		esac
	done
	# Block APPROVE, and anything we cannot positively identify as
	# REQUEST_CHANGES/COMMENT (unrecognized flags, or no verdict flag at all —
	# the latter drops into gh's interactive prompt, which a headless agent
	# cannot answer).
	{ [ "$sawunknown" = 1 ] || [ "$sawapprove" = 1 ] || [ "$sawverdict" = 0 ]; } \
		&& printf '%%s\n' '%[1]s' >&2 && exit 1
fi
if [ "$1" = "api" ]; then
	sawhidden=0
	sawreviews=0
	for arg in "$@"; do
		case "$arg" in
		[Ee][Vv][Ee][Nn][Tt]=[Aa][Pp][Pp][Rr][Oo][Vv][Ee])
			printf '%%s\n' '%[1]s' >&2
			exit 1
			;;
		esac
		case "$arg" in
		# The review mutation can carry its event in a GraphQL variable, so argv
		# never sees the submitted event beside EVENT. Agents have a sanctioned
		# REST path for pending drafts, so block GraphQL review mutations.
		*[Aa][Dd][Dd][Pp][Uu][Ll][Ll][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Ee][Vv][Ii][Ee][Ww]* | *[Ss][Uu][Bb][Mm][Ii][Tt][Pp][Uu][Ll][Ll][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Ee][Vv][Ii][Ee][Ww]*)
			printf '%%s\n' '%[1]s' >&2
			exit 1
			;;
		esac
		# --input (or --input=path) swaps the whole request body for a file/stdin
		# stream gh never parses, so an event field could ride along with nothing
		# in argv to catch it. event=@path and, for graphql, query=@path are the
		# same hole one field wide: gh api scopes a file value to exactly the
		# named field, so body=@path or comments[][body]=@path can only ever
		# become that field's string content — it cannot inject a sibling "event"
		# key — but event=@path hides the submission verb itself, and query=@path
		# hides a mutation that might embed one.
		case "$arg" in
		--input | --input=* | [Ee][Vv][Ee][Nn][Tt]=@* | [Qq][Uu][Ee][Rr][Yy]=@*)
			sawhidden=1
			;;
		esac
		# graphql counts as review-capable: addPullRequestReview reaches the same
		# place as POST /pulls/N/reviews.
		case "$arg" in
		[Gg][Rr][Aa][Pp][Hh][Qq][Ll] | *[Pp][Uu][Ll][Ll][Ss]/*/[Rr][Ee][Vv][Ii][Ee][Ww][Ss]* | */[Rr][Ee][Vv][Ii][Ee][Ww][Ss])
			sawreviews=1
			;;
		esac
	done
	# Refuse a review payload we cannot inspect rather than assume it is benign.
	[ "$sawhidden$sawreviews" = "11" ] && printf '%%s\n' '%[1]s' >&2 && exit 1
fi
if [ -n '%[3]s' ] && [ -x '%[3]s' ]; then
	token="$('%[3]s' github-app-token 2>/dev/null || true)"
elif command -v sybra-cli >/dev/null 2>&1; then
	token="$(sybra-cli github-app-token 2>/dev/null || true)"
else
	token=
fi
[ -z "$token" ] || {
	export GH_TOKEN="$token"
	export GITHUB_TOKEN="$token"
}
exec '%[2]s' "$@"
`

const gitCredentialShimTemplate = `#!/bin/sh
case "$1" in
get)
	protocol=
	host=
	while IFS= read -r line; do
		[ -n "$line" ] || break
		case "$line" in
		protocol=*) protocol=${line#protocol=} ;;
		host=*) host=${line#host=} ;;
		esac
	done
	case "$protocol:$host" in
	https:github.com)
		if [ -n '%[1]s' ] && [ -x '%[1]s' ]; then
			token="$('%[1]s' github-app-token 2>/dev/null || true)"
		elif command -v sybra-cli >/dev/null 2>&1; then
			token="$(sybra-cli github-app-token 2>/dev/null || true)"
		else
			token=
		fi
	[ -z "$token" ] || {
			printf 'username=x-access-token\n'
			printf '` + "pass" + `word=%%s\n' "$token"
		}
		;;
	esac
	;;
store | erase)
	;;
esac
exit 0
`

// writeGhShim materializes a `gh` wrapper in dir, for callers to prepend to an
// agent's PATH.
//
// This is the deterministic floor under the review-agent prompts: prompts
// carry the approve/request-changes/comment distinction (semantic ceiling),
// and this refuses direct APPROVE submissions even if that instruction drifts
// or is dropped — a prompt is not a permission boundary. REQUEST_CHANGES and
// COMMENT are feedback, not authority, so both this shim and the prompts let
// agents submit those directly.
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
func lookRealGh() string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	return path
}

func lookRealSybraCLI() string {
	if home, err := os.UserHomeDir(); err == nil {
		if abs := executableAbsolute(filepath.Join(home, ".local", "bin", "sybra-cli")); abs != "" {
			return abs
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		if abs := executableAbsolute(filepath.Join(dir, "sybra-cli")); abs != "" {
			return abs
		}
	}
	return ""
}

func executableAbsolute(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return abs
}

func writeGhShim(dir string) (string, error) {
	found := lookRealGh()
	if !shellSingleQuoteSafe(GhShimReason) {
		return "", fmt.Errorf("gh shim reason is not shell-safe")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create gh shim dir: %w", err)
	}

	sybraCLI := lookRealSybraCLI()
	if strings.ContainsAny(sybraCLI, "'\n") {
		return "", fmt.Errorf("sybra-cli path %q is not shell-safe", sybraCLI)
	}

	credentialScript := fmt.Sprintf(gitCredentialShimTemplate, sybraCLI)
	if err := writeExecutableAtomic(filepath.Join(dir, "git-credential-sybra"), credentialScript); err != nil {
		return "", err
	}
	if found != "" {
		realGh, err := filepath.Abs(found)
		if err != nil {
			return "", fmt.Errorf("resolve gh path: %w", err)
		}
		if strings.ContainsAny(realGh, "'\n") {
			return "", fmt.Errorf("gh path %q is not shell-safe", realGh)
		}
		script := fmt.Sprintf(ghShimScript, GhShimReason, realGh, sybraCLI)
		if err := writeExecutableAtomic(filepath.Join(dir, "gh"), script); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func shellSingleQuoteSafe(s string) bool {
	return !strings.ContainsAny(s, "'\n")
}

// writeExecutableAtomic stages the script under a temp name and renames it into
// place, so an agent already running when the manager restarts can never exec a
// half-written or not-yet-executable shim.
func writeExecutableAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gh-shim-*")
	if err != nil {
		return fmt.Errorf("create gh shim temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write gh shim: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close gh shim: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod gh shim: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install gh shim: %w", err)
	}
	return nil
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
	cfg.ExtraEnv = injectGitCredentialHelperEnv(cfg.ExtraEnv)
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

func injectGitCredentialHelperEnv(env []string) []string {
	env = stripEnvKeyPrefixes(env, "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_")
	env = stripEnvKeys(env, "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS")
	return append(env,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0=sybra",
		"GIT_CONFIG_KEY_1=credential.https://github.com.useHttpPath",
		"GIT_CONFIG_VALUE_1=false",
	)
}

func stripEnvKeyPrefixes(env []string, prefixes ...string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		drop := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
