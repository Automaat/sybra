//go:build darwin

package agent

// This file wraps provider CLI subprocesses in an OS-level sandbox-exec
// seatbelt profile so a write outside the agent's worktree/sandbox-home/tmp
// is denied by the kernel (see #1578, #1576). It is unrelated to
// internal/sandbox, which provisions Docker/k3d containers for an
// application *under test* by a task, not the agent process itself.

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

//go:embed agent_sandbox.sb
var agentSandboxProfile []byte

var (
	sandboxExecPathOnce sync.Once
	sandboxExecPath     string

	sandboxProfileOnce sync.Once
	sandboxProfilePath string
	errSandboxProfile  error
)

// sandboxExecAvailable reports whether sandbox-exec is on PATH. Every
// supported macOS release ships it at /usr/bin/sandbox-exec, but a stripped
// CI image or a future OS removal must be detected rather than assumed.
func sandboxExecAvailable() bool {
	sandboxExecPathOnce.Do(func() {
		if p, err := exec.LookPath("sandbox-exec"); err == nil {
			sandboxExecPath = p
		}
	})
	return sandboxExecPath != ""
}

func sandboxWrapperName() string { return "sandbox-exec" }

// sandboxUsesGitObjectOverlay reports whether provider Git objects need a
// task-private staging directory. Seatbelt can grant the shared, content-
// addressed objects directory directly, so Darwin must write there. Unlike
// Linux's bwrap wrapper, sandbox-exec has no post-process object/ref sync; an
// overlay here would let Git publish the real branch ref to a disposable
// object and corrupt it when the next run resets the sandbox home.
func sandboxUsesGitObjectOverlay() bool { return false }

// materializeSandboxProfile writes the embedded seatbelt profile to a stable
// temp file once per process and returns its path, for both the -f flag and
// operator-facing log/error messages. The profile content is fixed at build
// time, so re-writing it more than once per process would only waste I/O.
func materializeSandboxProfile() (string, error) {
	sandboxProfileOnce.Do(func() {
		f, err := os.CreateTemp("", "sybra-agent-sandbox-*.sb")
		if err != nil {
			errSandboxProfile = fmt.Errorf("write sandbox profile: %w", err)
			return
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Write(agentSandboxProfile); err != nil {
			errSandboxProfile = fmt.Errorf("write sandbox profile: %w", err)
			return
		}
		sandboxProfilePath = f.Name()
	})
	return sandboxProfilePath, errSandboxProfile
}

// canonicalizeRoot resolves root to an absolute, symlink-free path suitable
// for templating into a seatbelt -D value. sandbox-exec matches subpaths
// literally against the resolved filesystem, so an un-resolved /tmp (a
// symlink to /private/tmp on darwin) would deny every legitimate write
// under it in enforce mode, and evaluating symlinks/".." here (rather than
// trusting the caller) is what makes a crafted or stale root incapable of
// escaping the intended allowlist.
func canonicalizeRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("sandbox: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root %q: %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root %q: %w", root, err)
	}
	return resolved, nil
}

// sandboxTmpAlias returns the Seatbelt pattern for Claude Code's provider-owned
// cwd markers below macOS's stable /tmp target. Granting the whole target would
// also let one task modify arbitrary files belonging to another task or host
// process, so the alias must remain narrowly scoped.
func sandboxTmpAlias(canonTmp string) string {
	if strings.TrimSpace(canonTmp) == "" {
		return ""
	}
	const alias = "/tmp"
	resolved, err := canonicalizeRoot(alias)
	if err != nil {
		return ""
	}
	switch {
	case canonTmp == resolved:
		return "^" + regexp.QuoteMeta(resolved) + `/claude-[^/]+-cwd(/.*)?$`
	case strings.HasPrefix(canonTmp, "/private/var/folders/") && resolved == "/private/tmp":
		return "^" + regexp.QuoteMeta(resolved) + `/claude-[^/]+-cwd(/.*)?$`
	default:
		return ""
	}
}

// wrapInvocation transposes name/args into a sandbox-exec invocation when
// cfg carries an enforce-mode sandbox spec (set by
// Manager.injectProcessSandbox). report/off specs are never wrapped here:
// report mode's spec is validated and logged at dispatch time but
// intentionally never reaches this function with mode=="enforce", so a
// profile or SBPL defect can only affect enforce-mode runs, never the
// default (report) posture — see the rollback note on DefaultSandboxMode. A
// nil cfg (tests constructing a command directly) is never wrapped.
func sandboxRootOr(root, fallback string) string {
	if strings.TrimSpace(root) == "" {
		return fallback
	}
	return root
}

// unusedWritableRootSentinel stands in for a writable root that this run
// legitimately has none of — WORKTREE on a RunConfig.ReadOnlyDir run, where
// injectReadOnlyProcessSandbox deliberately passes an empty worktree so Dir is
// never writable.
//
// The profile references every param unconditionally, and sandbox-exec rejects
// an empty pattern with "sandbox-exec: empty subpath pattern" and refuses to
// launch. That surfaced as the agent producing no output at all — a 36-byte
// stderr and a workflow that blamed missing sidecars — rather than as a
// sandbox error, so it must be impossible to reach rather than merely fixed at
// the one site that hit it.
//
// A reserved, never-written path keeps the allow rule inert. Distinct from
// unusedReadOnlyDirSentinel so an unused *allow* root can never alias an
// unused *deny* root.
const unusedWritableRootSentinel = "/private/var/empty/sybra-sandbox-unused-writable"

// unusedReadOnlyDirSentinel is READONLY_DIR's value on every run that isn't
// RunConfig.ReadOnlyDir: the profile's READONLY_DIR deny rule is
// unconditional, so it always needs a value, and this one is a fixed,
// reserved macOS path no writable root is ever configured under — a deny on
// its subpath can never shadow a legitimate write.
const unusedReadOnlyDirSentinel = "/private/var/empty/sybra-sandbox-readonly-unused"

// stateDenyAt returns the i-th durable-config path, or "" past the end. SBPL
// takes fixed parameters and cannot iterate, so the slots are filled
// positionally and unused ones fall back to the sentinel — a path nothing
// writes, so an unused slot denies nothing.
func stateDenyAt(paths []string, i int) string {
	if i < len(paths) {
		return paths[i]
	}
	return ""
}

// sbplQuote renders p as an SBPL string literal. A path containing a quote or
// backslash would otherwise terminate the literal early and change which
// rules the profile expresses, so both are escaped rather than assumed absent.
func sbplQuote(p string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
}

// buildReadProfile materializes a profile that denies reads outside roots,
// returning base unchanged when roots is empty. SBPL parameters are
// fixed-arity and cannot iterate, so a variable-length allowlist has to be
// generated into the profile text and written per run rather than passed
// as -D values like the write roots are.
//
// Seatbelt resolves the most specific applicable rule rather than the last
// declared one, so appending after the embedded base leaves the base's
// write rules untouched.
func buildReadProfile(base string, roots []string, dir string) (string, error) {
	if len(roots) == 0 {
		return base, nil
	}
	var b strings.Builder
	b.Write(agentSandboxProfile)
	b.WriteString("\n;; Deny-by-default reads (#2781), generated per run.\n")
	b.WriteString("(deny file-read*)\n")
	// Provider runtimes inspect inherited pipes before startup. Claude's Bun
	// launcher calls fstat(2) on stderr while deciding whether to enable
	// colours. Seatbelt cannot target inherited descriptors here, so this
	// intentionally permits metadata probes for paths outside roots; content
	// reads remain restricted to the explicit allowlist below.
	b.WriteString("(allow file-read-metadata)\n(allow file-read*\n")
	// Seatbelt checks metadata access on every ancestor while traversing to an
	// allowed subpath. Grant the ancestors as exact literals only; without
	// this, even /bin/sh aborts while trying to traverse "/", while using a
	// subpath here would accidentally reopen the entire filesystem.
	seenAncestors := make(map[string]struct{})
	for _, r := range roots {
		for ancestor := filepath.Dir(r); ; ancestor = filepath.Dir(ancestor) {
			if _, ok := seenAncestors[ancestor]; !ok {
				seenAncestors[ancestor] = struct{}{}
				b.WriteString("  (literal " + sbplQuote(ancestor) + ")\n")
			}
			if ancestor == filepath.Dir(ancestor) {
				break
			}
		}
	}
	for _, r := range roots {
		// subpath covers directories; literal covers the plain files in the
		// allowlist (~/.claude.json, ~/.gitconfig), which subpath does not
		// match on its own.
		b.WriteString("  (subpath " + sbplQuote(r) + ")\n")
		b.WriteString("  (literal " + sbplQuote(r) + ")\n")
	}
	b.WriteString(")\n")
	// Fixed name inside the per-task sandbox home rather than a fresh
	// os.CreateTemp file. This runs on every enforce spawn, so a long-lived
	// daemon would otherwise leak one profile per spawn into the system temp
	// dir forever. One file per task, removed with the sandbox home.
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("read sandbox profile: no sandbox home to write into")
	}
	path := filepath.Join(dir, "agent-sandbox-read.sb")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("write read sandbox profile: %w", err)
	}
	return path, nil
}

func wrapInvocation(name string, args []string, cfg *RunConfig) (wrappedName string, wrappedArgs []string) {
	if cfg == nil || cfg.sandbox.mode != "enforce" {
		return name, args
	}
	home := cfg.sandbox.sandboxHome
	params := [][2]string{
		{"WORKTREE", cfg.sandbox.worktree},
		{"SANDBOX_HOME", home},
		{"SIDECAR_DIR", cfg.sandbox.sidecarDir},
		{"TMP", cfg.sandbox.tmp},
		{"TMP_ALIAS_PATTERN", cfg.sandbox.tmpAlias},
		{"SHARED_CACHE", cfg.sandbox.sharedCache},
		{"CLAUDE_STATE", sandboxRootOr(cfg.sandbox.claudeState, home)},
		{"CODEX_STATE", sandboxRootOr(cfg.sandbox.codexState, home)},
		{"COPILOT_STATE", sandboxRootOr(cfg.sandbox.copilotState, home)},
		{"OPENCODE_STATE", sandboxRootOr(cfg.sandbox.opencodeState, home)},
		{"TOOL_CACHE", sandboxRootOr(cfg.sandbox.toolCache, home)},
		{"APP_SUPPORT", cfg.sandbox.appSupport},
		{"CLAUDE_SCRATCH", cfg.sandbox.claudeScratch},
		{"GIT_ADMIN_DIR", cfg.sandbox.gitAdminDir},
		{"GIT_LOOSE_OBJECT_PATTERN", cfg.sandbox.gitLooseObjectPattern},
		{"GIT_LOOSE_OBJECT_FANOUT_PATTERN", cfg.sandbox.gitLooseObjectFanoutPattern},
		{"GIT_BRANCH_REF_FILE", cfg.sandbox.gitBranchRefFile},
		{"GIT_BRANCH_REF_LOCK_FILE", cfg.sandbox.gitBranchRefLockFile},
		{"GIT_BRANCH_LOG_FILE", cfg.sandbox.gitBranchLogFile},
		{"GIT_STASH_REF_FILE", cfg.sandbox.gitStashRefFile},
		{"GIT_STASH_REF_LOCK_FILE", cfg.sandbox.gitStashRefLockFile},
		{"GIT_STASH_LOG_FILE", cfg.sandbox.gitStashLogFile},
		{"GIT_STASH_LOG_LOCK_FILE", cfg.sandbox.gitStashLogLockFile},
		{"GIT_PACKED_REFS_LOCK_FILE", cfg.sandbox.gitPackedRefsLockFile},
		{"GIT_REMOTE_REF_DIR", cfg.sandbox.gitRemoteRefDir},
		{"GIT_REMOTE_LOG_DIR", cfg.sandbox.gitRemoteLogDir},
		{"GIT_REMOTE_LOG_LOCK_PATTERN", cfg.sandbox.gitRemoteLogLockPattern},
		{"GIT_TAG_REF_DIR", cfg.sandbox.gitTagRefDir},
		{"GIT_TAG_LOG_DIR", cfg.sandbox.gitTagLogDir},
		{"GIT_TAG_LOG_LOCK_PATTERN", cfg.sandbox.gitTagLogLockPattern},
		{"GIT_NOTES_REF_DIR", cfg.sandbox.gitNotesRefDir},
		{"GIT_NOTES_LOG_DIR", cfg.sandbox.gitNotesLogDir},
		{"GIT_NOTES_LOG_LOCK_PATTERN", cfg.sandbox.gitNotesLogLockPattern},
		{"GIT_SHALLOW_FILE", cfg.sandbox.gitShallowFile},
		{"GIT_SHALLOW_LOCK_FILE", cfg.sandbox.gitShallowLockFile},
		{"READONLY_DIR", sandboxRootOr(cfg.sandbox.readOnlyDir, unusedReadOnlyDirSentinel)},
		{"STATE_DENY_1", sandboxRootOr(stateDenyAt(cfg.sandbox.stateDenied, 0), unusedReadOnlyDirSentinel)},
		{"STATE_DENY_2", sandboxRootOr(stateDenyAt(cfg.sandbox.stateDenied, 1), unusedReadOnlyDirSentinel)},
		{"STATE_DENY_3", sandboxRootOr(stateDenyAt(cfg.sandbox.stateDenied, 2), unusedReadOnlyDirSentinel)},
	}
	wrapped := make([]string, 0, len(args)+2*len(params)+3)
	wrapped = append(wrapped, "-f", cfg.sandbox.profilePath)
	for _, p := range params {
		// Every param is templated into an unconditional subpath or literal
		// rule, and seatbelt rejects an empty pattern outright — see
		// unusedWritableRootSentinel. Substituting here is the single
		// chokepoint that keeps a legitimately-absent root from producing a
		// malformed profile.
		wrapped = append(wrapped, "-D", p[0]+"="+sandboxRootOr(p[1], unusedWritableRootSentinel))
	}
	wrapped = append(wrapped, name)
	wrapped = append(wrapped, args...)
	return sandboxExecPath, wrapped
}
