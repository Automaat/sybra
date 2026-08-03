//go:build darwin

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildReadProfile_EmptyRootsKeepsBaseProfile(t *testing.T) {
	got, err := buildReadProfile("/tmp/base.sb", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}
	// No deployment has read roots today; generating a read-denying profile for them would restrict reads nobody opted into.
	if got != "/tmp/base.sb" {
		t.Fatalf("profile = %q, want the untouched base path", got)
	}
}

func TestBuildReadProfile_DeniesReadsOutsideAllowlist(t *testing.T) {
	path, err := buildReadProfile("/tmp/base.sb", []string{"/usr", "/data/wt"}, t.TempDir())
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	profile := string(raw)

	if !strings.Contains(profile, "(deny file-read*)") {
		t.Errorf("profile does not deny reads by default:\n%s", profile)
	}
	for _, root := range []string{`(subpath "/usr")`, `(subpath "/data/wt")`} {
		if !strings.Contains(profile, root) {
			t.Errorf("profile missing read allow %s:\n%s", root, profile)
		}
	}
	// A plain file such as ~/.claude.json needs literal; subpath alone leaves the provider CLI unauthenticated.
	if !strings.Contains(profile, `(literal "/usr")`) {
		t.Errorf("profile has no literal rules, so file allowlist entries never match:\n%s", profile)
	}
	if !strings.Contains(profile, "(deny file-write*)") {
		t.Errorf("read block clobbered the base write rules:\n%s", profile)
	}
}

func TestSbplQuote_EscapesProfileTerminators(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain path", in: "/data/wt", want: `"/data/wt"`},
		{name: "embedded quote", in: `/data/a"b`, want: `"/data/a\"b"`},
		{name: "embedded backslash", in: `/data/a\b`, want: `"/data/a\\b"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// An unescaped quote closes the literal early, silently widening which paths the profile allows.
			if got := sbplQuote(tc.in); got != tc.want {
				t.Fatalf("sbplQuote(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildReadProfile_WritesIntoSandboxHome(t *testing.T) {
	home := t.TempDir()

	first, err := buildReadProfile("/tmp/base.sb", []string{"/usr"}, home)
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}
	second, err := buildReadProfile("/tmp/base.sb", []string{"/usr", "/etc"}, home)
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}

	// A fresh temp file per spawn would leak one profile per run forever in a long-lived daemon, so repeated builds must reuse one path under the per-task sandbox home.
	if first != second {
		t.Fatalf("profile path changed between runs: %q then %q", first, second)
	}
	if filepath.Dir(first) != home {
		t.Fatalf("profile written to %q, want it inside the sandbox home %q", first, home)
	}
}

func TestBuildReadProfile_RequiresSandboxHome(t *testing.T) {
	// Falling back to the system temp dir is what leaks; an empty home must fail the run closed instead.
	if _, err := buildReadProfile("/tmp/base.sb", []string{"/usr"}, ""); err == nil {
		t.Fatal("buildReadProfile accepted an empty sandbox home")
	}
}

// A RunConfig.ReadOnlyDir run legitimately has no writable worktree, so
// injectReadOnlyProcessSandbox passes an empty one. Templating that straight
// into the profile made sandbox-exec refuse to launch with
// "empty subpath pattern", which surfaced as an agent that produced no output
// at all rather than as a sandbox failure.
func TestWrapInvocation_NoEmptyProfileParams(t *testing.T) {
	tests := []struct {
		name string
		spec sandboxSpec
	}{
		{
			name: "read-only dir run has no worktree",
			spec: sandboxSpec{
				mode:        "enforce",
				worktree:    "", // what injectReadOnlyProcessSandbox passes
				sandboxHome: "/data/home",
				tmp:         "/tmp",
				sharedCache: "/data/cache",
				readOnlyDir: "/opt/src",
			},
		},
		{
			name: "every optional root absent",
			spec: sandboxSpec{mode: "enforce"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, args := wrapInvocation("claude", []string{"-p", "hi"}, &RunConfig{sandbox: tc.spec})
			for i := range len(args) - 1 {
				if args[i] != "-D" {
					continue
				}
				name, value, ok := strings.Cut(args[i+1], "=")
				if !ok {
					t.Fatalf("malformed -D arg %q", args[i+1])
				}
				if strings.TrimSpace(value) == "" {
					t.Errorf("param %s is empty; sandbox-exec rejects an empty subpath and refuses to launch", name)
				}
			}
		})
	}
}

// The substitute must stay inert: an unused writable root must not alias the
// sentinel used for unused deny rules.
func TestUnusedRootSentinelsAreDistinct(t *testing.T) {
	if unusedWritableRootSentinel == unusedReadOnlyDirSentinel {
		t.Fatal("an unused allow root aliases an unused deny root")
	}
}

// codex's in-process app-server creates a directory under the macOS per-user
// application-data root at startup. Without a write grant there it dies with
// "failed to initialize in-process app-server client: Operation not
// permitted" before producing any output, which surfaced downstream as a
// missing skill-conformance receipt rather than as a sandbox denial.
func TestWrapInvocation_GrantsAppSupportRoot(t *testing.T) {
	const appSupport = "/Users/u/Library/Application Support"
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    "/data/wt",
		sandboxHome: "/data/home",
		tmp:         "/tmp",
		sharedCache: "/data/cache",
		appSupport:  appSupport,
	}}

	_, args := wrapInvocation("codex", []string{"exec"}, cfg)

	var got string
	for i := range len(args) - 1 {
		if args[i] == "-D" {
			if name, value, ok := strings.Cut(args[i+1], "="); ok && name == "APP_SUPPORT" {
				got = value
			}
		}
	}
	if got != appSupport {
		t.Fatalf("APP_SUPPORT = %q, want %q", got, appSupport)
	}
}

// The embedded profile must actually reference the param, or the -D value is
// inert and the grant silently does nothing.
func TestSandboxProfile_ReferencesAppSupport(t *testing.T) {
	if !strings.Contains(string(agentSandboxProfile), `(subpath (param "APP_SUPPORT"))`) {
		t.Fatal("profile has no APP_SUPPORT write rule, so the resolved root grants nothing")
	}
}

// A linked worktree's actual gitdir (HEAD, index, logs/HEAD) lives outside
// WORKTREE, under the shared bare clone's worktrees/<branch>/ subdirectory,
// and a real commit also needs write on the shared object store plus the
// branch's own ref/lock/reflog and the shared remote/tag ref+log dirs.
// Without these grants git fetch/add/commit failed EPERM partway through.
func TestWrapInvocation_GrantsAllGitRoots(t *testing.T) {
	spec := sandboxSpec{
		mode:                  "enforce",
		worktree:              "/data/wt",
		sandboxHome:           "/data/home",
		tmp:                   "/tmp",
		sharedCache:           "/data/cache",
		gitAdminDir:           "/data/clones/repo.git/worktrees/task-branch",
		gitObjectDir:          "/data/clones/repo.git/objects",
		gitBranchRefFile:      "/data/clones/repo.git/refs/heads/fix/task-branch",
		gitBranchRefLockFile:  "/data/clones/repo.git/refs/heads/fix/task-branch.lock",
		gitBranchLogFile:      "/data/clones/repo.git/logs/refs/heads/fix/task-branch",
		gitBranchLogLockFile:  "/data/clones/repo.git/logs/refs/heads/fix/task-branch.lock",
		gitStashRefFile:       "/data/clones/repo.git/refs/stash",
		gitStashRefLockFile:   "/data/clones/repo.git/refs/stash.lock",
		gitStashLogFile:       "/data/clones/repo.git/logs/refs/stash",
		gitStashLogLockFile:   "/data/clones/repo.git/logs/refs/stash.lock",
		gitPackedRefsFile:     "/data/clones/repo.git/packed-refs",
		gitPackedRefsNewFile:  "/data/clones/repo.git/packed-refs.new",
		gitPackedRefsLockFile: "/data/clones/repo.git/packed-refs.lock",
		gitGCPidFile:          "/data/clones/repo.git/gc.pid",
		gitGCPidLockFile:      "/data/clones/repo.git/gc.pid.lock",
		gitShallowFile:        "/data/clones/repo.git/shallow",
		gitShallowLockFile:    "/data/clones/repo.git/shallow.lock",
		gitInfoDir:            "/data/clones/repo.git/info",
		gitInfoDenyAttributes: "/data/clones/repo.git/info/attributes",
		gitInfoDenyExclude:    "/data/clones/repo.git/info/exclude",
		gitRemoteRefDir:       "/data/clones/repo.git/refs/remotes",
		gitRemoteLogDir:       "/data/clones/repo.git/logs/refs/remotes",
		gitTagRefDir:          "/data/clones/repo.git/refs/tags",
		gitTagLogDir:          "/data/clones/repo.git/logs/refs/tags",
		gitNotesRefDir:        "/data/clones/repo.git/refs/notes",
		gitNotesLogDir:        "/data/clones/repo.git/logs/refs/notes",
	}
	cfg := &RunConfig{sandbox: spec}

	_, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)

	want := map[string]string{
		"GIT_ADMIN_DIR":             spec.gitAdminDir,
		"GIT_OBJECT_DIR":            spec.gitObjectDir,
		"GIT_BRANCH_REF_FILE":       spec.gitBranchRefFile,
		"GIT_BRANCH_REF_LOCK_FILE":  spec.gitBranchRefLockFile,
		"GIT_BRANCH_LOG_FILE":       spec.gitBranchLogFile,
		"GIT_BRANCH_LOG_LOCK_FILE":  spec.gitBranchLogLockFile,
		"GIT_STASH_REF_FILE":        spec.gitStashRefFile,
		"GIT_STASH_REF_LOCK_FILE":   spec.gitStashRefLockFile,
		"GIT_STASH_LOG_FILE":        spec.gitStashLogFile,
		"GIT_STASH_LOG_LOCK_FILE":   spec.gitStashLogLockFile,
		"GIT_PACKED_REFS_FILE":      spec.gitPackedRefsFile,
		"GIT_PACKED_REFS_NEW_FILE":  spec.gitPackedRefsNewFile,
		"GIT_PACKED_REFS_LOCK_FILE": spec.gitPackedRefsLockFile,
		"GIT_GC_PID_FILE":           spec.gitGCPidFile,
		"GIT_GC_PID_LOCK_FILE":      spec.gitGCPidLockFile,
		"GIT_SHALLOW_FILE":          spec.gitShallowFile,
		"GIT_SHALLOW_LOCK_FILE":     spec.gitShallowLockFile,
		"GIT_INFO_DIR":              spec.gitInfoDir,
		"GIT_INFO_DENY_ATTRIBUTES":  spec.gitInfoDenyAttributes,
		"GIT_INFO_DENY_EXCLUDE":     spec.gitInfoDenyExclude,
		"GIT_REMOTE_REF_DIR":        spec.gitRemoteRefDir,
		"GIT_REMOTE_LOG_DIR":        spec.gitRemoteLogDir,
		"GIT_TAG_REF_DIR":           spec.gitTagRefDir,
		"GIT_TAG_LOG_DIR":           spec.gitTagLogDir,
		"GIT_NOTES_REF_DIR":         spec.gitNotesRefDir,
		"GIT_NOTES_LOG_DIR":         spec.gitNotesLogDir,
	}
	got := map[string]string{}
	for i := range len(args) - 1 {
		if args[i] != "-D" {
			continue
		}
		if name, value, ok := strings.Cut(args[i+1], "="); ok {
			got[name] = value
		}
	}
	for name, wantValue := range want {
		if got[name] != wantValue {
			t.Errorf("%s = %q, want %q", name, got[name], wantValue)
		}
	}
}

// The embedded profile must actually reference every git-root param, or the
// -D value is inert and the grant silently does nothing.
func TestSandboxProfile_ReferencesAllGitRoots(t *testing.T) {
	profile := string(agentSandboxProfile)
	for _, rule := range []string{
		`(subpath (param "GIT_ADMIN_DIR"))`,
		`(subpath (param "GIT_OBJECT_DIR"))`,
		`(literal (param "GIT_BRANCH_REF_FILE"))`,
		`(literal (param "GIT_BRANCH_REF_LOCK_FILE"))`,
		`(literal (param "GIT_BRANCH_LOG_FILE"))`,
		`(literal (param "GIT_BRANCH_LOG_LOCK_FILE"))`,
		`(literal (param "GIT_STASH_REF_FILE"))`,
		`(literal (param "GIT_STASH_REF_LOCK_FILE"))`,
		`(literal (param "GIT_STASH_LOG_FILE"))`,
		`(literal (param "GIT_STASH_LOG_LOCK_FILE"))`,
		`(literal (param "GIT_PACKED_REFS_FILE"))`,
		`(literal (param "GIT_PACKED_REFS_NEW_FILE"))`,
		`(literal (param "GIT_PACKED_REFS_LOCK_FILE"))`,
		`(literal (param "GIT_GC_PID_FILE"))`,
		`(literal (param "GIT_GC_PID_LOCK_FILE"))`,
		`(literal (param "GIT_SHALLOW_FILE"))`,
		`(literal (param "GIT_SHALLOW_LOCK_FILE"))`,
		`(subpath (param "GIT_INFO_DIR"))`,
		`(literal (param "GIT_INFO_DENY_ATTRIBUTES"))`,
		`(literal (param "GIT_INFO_DENY_EXCLUDE"))`,
		`(subpath (param "GIT_REMOTE_REF_DIR"))`,
		`(subpath (param "GIT_REMOTE_LOG_DIR"))`,
		`(subpath (param "GIT_TAG_REF_DIR"))`,
		`(subpath (param "GIT_TAG_LOG_DIR"))`,
		`(subpath (param "GIT_NOTES_REF_DIR"))`,
		`(subpath (param "GIT_NOTES_LOG_DIR"))`,
	} {
		if !strings.Contains(profile, rule) {
			t.Errorf("profile missing %s; the resolved root grants nothing", rule)
		}
	}
}

// Claude Code writes per-session working files under /tmp/claude-<uid>. On
// darwin that is outside the tmp root, since os.TempDir() is $TMPDIR
// (/var/folders/.../T) while /tmp resolves to /private/tmp — so every such
// write failed EPERM and the agent retried an impossible mkdir instead of
// progressing.
func TestClaudeScratchRoot_IsExactlyTheTmpScratchpad(t *testing.T) {
	got := claudeScratchRoot()
	if got == "" {
		t.Fatal("no scratchpad root resolved; Claude Code writes would be denied")
	}
	// Pinned to the exact path, not merely "contains claude-<uid>": a
	// uid-scoped path under $TMPDIR would satisfy a looser check while
	// leaving the real /tmp scratchpad denied, which is the bug this fixes.
	want := filepath.Join("/tmp", fmt.Sprintf("claude-%d", os.Getuid()))
	wantResolved := filepath.Join("/private/tmp", fmt.Sprintf("claude-%d", os.Getuid()))
	if got != want && got != wantResolved {
		t.Fatalf("scratchpad root = %q, want %q (or its resolved form %q)", got, want, wantResolved)
	}
	// Granting /tmp wholesale would hand agents a world-writable directory
	// shared with every process on the host.
	for _, tooWide := range []string{"/tmp", "/private/tmp", "/private/var/tmp"} {
		if got == tooWide {
			t.Fatalf("scratchpad root is %q, which grants all of tmp", got)
		}
	}
}

// /tmp is world-writable, so any local process can pre-create the scratchpad
// path as a symlink. Following it would grant a writable root outside /tmp and
// defeat the boundary the sandbox exists to enforce.
func TestResolveScratchRoot_RefusesSymlink(t *testing.T) {
	base := t.TempDir()
	link := filepath.Join(base, "scratch-link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if got := resolveScratchRoot(link); got != "" {
		t.Fatalf("resolveScratchRoot() = %q via a symlink; must refuse rather than grant outside the intended root", got)
	}
}

func TestResolveScratchRoot_AcceptsRealDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scratch")

	got := resolveScratchRoot(root)

	if got != root && got != filepath.Join("/private", root) {
		t.Fatalf("resolveScratchRoot() = %q, want the created directory %q", got, root)
	}
}

func TestSandboxProfile_ReferencesClaudeScratch(t *testing.T) {
	if !strings.Contains(string(agentSandboxProfile), `(subpath (param "CLAUDE_SCRATCH"))`) {
		t.Fatal("profile has no CLAUDE_SCRATCH rule, so the resolved root grants nothing")
	}
}

// Denying writes to the device sinks broke essentially all tooling: every
// `>/dev/null` redirect failed, which took out git with "fatal: could not
// open '/dev/null' for reading and writing". Writing to them is a no-op by
// definition, so the grant adds no blast radius.
func TestSandboxProfile_AllowsDeviceSinks(t *testing.T) {
	profile := string(agentSandboxProfile)
	for _, dev := range []string{`(literal "/dev/null")`, `(literal "/dev/zero")`} {
		if !strings.Contains(profile, dev) {
			t.Errorf("profile does not permit writes to %s; every >/dev/null redirect fails without it", dev)
		}
	}
}
