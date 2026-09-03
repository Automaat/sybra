//go:build darwin

package agent

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/Automaat/sybra/internal/providerid"
)

func TestSandboxReadEnforce_AllowsInheritedStderrMetadata(t *testing.T) {
	if os.Getenv(sandboxProbeChildEnv) == "1" {
		var stat syscall.Stat_t
		if err := syscall.Fstat(2, &stat); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "fstat stderr: %v", err)
			os.Exit(2)
		}
		return
	}
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)
	profilePath, err := buildReadProfile(cfg.sandbox.profilePath, []string{os.Args[0]}, wt)
	if err != nil {
		t.Fatalf("build read profile: %v", err)
	}
	cfg.sandbox.profilePath = profilePath
	cmd := newDarwinSandboxCmd(cfg, os.Args[0], "-test.run=^TestSandboxReadEnforce_AllowsInheritedStderrMetadata$")
	cmd.Env = append(cmd.Env, sandboxProbeChildEnv+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fstat inherited stderr under read sandbox: %v: %s", err, stderr.String())
	}
}

// TestSandboxReadEnforce_AllowsNativeClaudeInstall reproduces the native
// Claude installer layout: ~/.local/bin/claude points at a versioned Bun
// executable under ~/.local/share/claude. The symlink alone is insufficient;
// Bun reads its own image during startup and exits with EPERM before NDJSON
// when the resolved install root is absent from the production allowlist.
func TestSandboxReadEnforce_AllowsNativeClaudeInstall(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)
	home := t.TempDir()
	t.Setenv("HOME", home)
	nativeInstall := filepath.Join(home, ".local", "share", providerid.Claude)
	claudeTarget := filepath.Join(nativeInstall, "versions", "test")
	if err := os.MkdirAll(filepath.Dir(claudeTarget), 0o700); err != nil {
		t.Fatalf("create native install fixture: %v", err)
	}
	if err := os.WriteFile(claudeTarget, []byte("#!/bin/sh\necho CLAUDE_OK\n"), 0o700); err != nil {
		t.Fatalf("write native install fixture: %v", err)
	}
	claudePath := filepath.Join(home, ".local", "bin", providerid.Claude)
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o700); err != nil {
		t.Fatalf("create native launcher fixture: %v", err)
	}
	if err := os.Symlink(claudeTarget, claudePath); err != nil {
		t.Fatalf("link native launcher fixture: %v", err)
	}
	m := newReadModeManager("enforce")
	cfg.Role = RoleReview
	profilePath, err := buildReadProfile(cfg.sandbox.profilePath, m.resolveSandboxReadRoots(cfg), wt)
	if err != nil {
		t.Fatalf("build read profile: %v", err)
	}
	cfg.sandbox.profilePath = profilePath

	cmd := newDarwinSandboxCmd(cfg, claudePath, "--version")
	cmd.Dir = wt
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("native Claude/Bun startup under read sandbox: %v: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "CLAUDE_OK" {
		t.Fatalf("native Claude fixture output = %q, want CLAUDE_OK", got)
	}
}

func TestSandboxReadEnforce_AllowsExternalProviderExecutable(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)
	home := t.TempDir()
	t.Setenv("HOME", home)
	providerScope := filepath.Join(home, ".provider-install", "node_modules", "@openai")
	packageDir := filepath.Join(providerScope, providerid.Codex)
	platformDir := filepath.Join(providerScope, "codex-darwin-arm64")
	target := filepath.Join(packageDir, "bin", providerid.Codex)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("create provider install fixture: %v", err)
	}
	if err := os.MkdirAll(platformDir, 0o700); err != nil {
		t.Fatalf("create provider platform fixture: %v", err)
	}
	packageFile := filepath.Join(packageDir, "package.json")
	platformFile := filepath.Join(platformDir, "binary")
	companion := filepath.Join(filepath.Dir(target), "codex-code-mode-host")
	if err := os.WriteFile(packageFile, []byte("PACKAGE\n"), 0o600); err != nil {
		t.Fatalf("write provider package fixture: %v", err)
	}
	if err := os.WriteFile(platformFile, []byte("PLATFORM\n"), 0o600); err != nil {
		t.Fatalf("write provider platform fixture: %v", err)
	}
	if err := os.WriteFile(companion, []byte("#!/bin/sh\necho HOST\n"), 0o700); err != nil {
		t.Fatalf("write provider companion fixture: %v", err)
	}
	// Match Codex's sibling discovery: derive the Code Mode host from the
	// invoked executable spelling. newProviderCmd must therefore replace the
	// public launcher symlink with target before entering Seatbelt.
	script := fmt.Sprintf("#!/bin/sh\n/bin/cat %q %q\ndir=$(/usr/bin/dirname \"$0\")\n\"$dir/codex-code-mode-host\"\n", packageFile, platformFile)
	if err := os.WriteFile(target, []byte(script), 0o700); err != nil {
		t.Fatalf("write provider install fixture: %v", err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("create provider launcher directory: %v", err)
	}
	launcher := filepath.Join(binDir, providerid.Codex)
	if err := os.Symlink(target, launcher); err != nil {
		t.Fatalf("link provider launcher fixture: %v", err)
	}
	t.Setenv("PATH", binDir)
	m := newReadModeManager("enforce")
	cfg.Role = RoleReview
	cfg.provider = providerByName(providerid.Codex)
	profilePath, err := buildReadProfile(cfg.sandbox.profilePath, m.resolveSandboxReadRoots(cfg), wt)
	if err != nil {
		t.Fatalf("build read profile: %v", err)
	}
	cfg.sandbox.profilePath = profilePath

	cmd := newDarwinSandboxCmd(cfg, providerid.Codex, "--version")
	cmd.Dir = wt
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("external provider startup under read sandbox: %v: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "PACKAGE\nPLATFORM\nHOST" {
		t.Fatalf("external provider fixture output = %q, want package, platform, and companion data", got)
	}
}

func TestSandboxEnforce_DarwinCommitSurvivesSandboxReset(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	sandboxHome := t.TempDir()
	ambientObjects := t.TempDir()
	t.Setenv("GIT_OBJECT_DIRECTORY", ambientObjects)
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", "/attacker/alternates")
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})
	sharedObjects, err := canonicalizeRoot(filepath.Join(bare, "objects"))
	if err != nil {
		t.Fatalf("canonicalize shared object store: %v", err)
	}
	prepare := func() RunConfig {
		cfg, _, err := m.prepareRunConfig(RunConfig{
			TaskID:      "task-durable-objects",
			Mode:        "headless",
			Dir:         wt,
			SandboxMode: "enforce",
		})
		if err != nil {
			t.Fatalf("prepareRunConfig: %v", err)
		}
		if got, want := darwinEnvValue(cfg.ExtraEnv, "GIT_OBJECT_DIRECTORY"), sharedObjects; got != want {
			t.Fatalf("GIT_OBJECT_DIRECTORY = %q, want trusted shared store %q", got, want)
		}
		if got := darwinEnvValue(cfg.ExtraEnv, "GIT_ALTERNATE_OBJECT_DIRECTORIES"); got != "" {
			t.Fatalf("GIT_ALTERNATE_OBJECT_DIRECTORIES = %q, want empty trusted override", got)
		}
		return cfg
	}

	cfg := prepare()
	cmd := newProviderCmd(context.Background(), &cfg, false, "sh", "-c", "echo durable > durable.txt && git add durable.txt && git commit -q -m durable")
	cmd.Dir = wt
	cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed commit: %v: %s", err, out)
	}
	headCmd := exec.Command("git", "rev-parse", "HEAD")
	headCmd.Dir = wt
	headCmd.Env = gitSandboxDiscoveryEnv()
	headOut, err := headCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve committed HEAD: %v: %s", err, headOut)
	}
	head := strings.TrimSpace(string(headOut))

	if err := os.RemoveAll(ambientObjects); err != nil {
		t.Fatalf("remove ambient object store: %v", err)
	}
	if err := os.RemoveAll(sandboxHome); err != nil {
		t.Fatalf("remove sandbox home: %v", err)
	}
	if err := os.MkdirAll(sandboxHome, 0o755); err != nil {
		t.Fatalf("recreate sandbox home: %v", err)
	}
	_ = prepare()

	verify := exec.Command("git", "cat-file", "-e", head+"^{commit}")
	verify.Dir = bare
	verify.Env = gitSandboxDiscoveryEnv()
	if out, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("commit disappeared after sandbox reset: %v: %s", err, out)
	}
}

func darwinEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, assignment := range env {
		if value, ok := strings.CutPrefix(assignment, prefix); ok {
			return value
		}
	}
	return ""
}

func TestPrepareGitSandboxOverlay_DarwinRejectsInvalidLegacyObjects(t *testing.T) {
	sandboxHome := t.TempDir()
	legacy := filepath.Join(sandboxHome, ".sybra-git-overlay", "objects", "ab", "cdef")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o444); err != nil {
		t.Fatal(err)
	}

	_, err := prepareGitSandboxOverlay(context.Background(), t.TempDir(), sandboxHome, gitSandboxRoots{objectDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unsupported legacy Git object payload") {
		t.Fatalf("prepareGitSandboxOverlay error = %v, want invalid legacy-object failure", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy object was not preserved: %v", err)
	}
}

func TestPrepareGitSandboxOverlay_DarwinPreservesCorruptCanonicalLegacyObject(t *testing.T) {
	sandboxHome := t.TempDir()
	legacy := filepath.Join(sandboxHome, ".sybra-git-overlay", "objects", "ab", strings.Repeat("0", 38))
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("not a zlib Git object"), 0o444); err != nil {
		t.Fatal(err)
	}

	_, err := prepareGitSandboxOverlay(context.Background(), t.TempDir(), sandboxHome, gitSandboxRoots{objectDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "verify legacy object") {
		t.Fatalf("prepareGitSandboxOverlay error = %v, want corrupt legacy-object failure", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("corrupt legacy object was not preserved: %v", err)
	}
}

func TestVerifyLooseObject_RejectsOversizedObjectBeforeReadingBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zlib.NewWriter(file)
	if _, err := fmt.Fprintf(zw, "blob %d\x00", int64(maxLegacyLooseObjectSize)+1); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	err = verifyLooseObject(file, strings.Repeat("0", 40))
	if err == nil || !strings.Contains(err.Error(), "exceeds migration limit") {
		t.Fatalf("verifyLooseObject error = %v, want migration-size limit", err)
	}
}

func TestPrepareGitSandboxOverlay_DarwinMigratesLegacyObjectsBeforeReset(t *testing.T) {
	bare, wt := setupLinkedWorktree(t)
	sandboxHome := t.TempDir()
	legacyObjects := filepath.Join(sandboxHome, ".sybra-git-overlay", "objects")
	if err := os.MkdirAll(legacyObjects, 0o755); err != nil {
		t.Fatal(err)
	}
	sharedObjects := filepath.Join(bare, "objects")

	if err := os.WriteFile(filepath.Join(wt, "legacy.txt"), []byte("preserved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOrFatal(t, wt, "add", "legacy.txt")
	commit := exec.Command("git", "commit", "-q", "-m", "legacy overlay commit")
	commit.Dir = wt
	commit.Env = append(gitSandboxDiscoveryEnv(),
		"GIT_OBJECT_DIRECTORY="+legacyObjects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+sharedObjects,
	)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("create legacy-overlay commit: %v: %s", err, out)
	}
	head := strings.TrimSpace(runGitOutput(t, wt, "rev-parse", "HEAD"))
	if _, err := os.Stat(filepath.Join(sharedObjects, head[:2], head[2:])); !os.IsNotExist(err) {
		t.Fatalf("legacy commit unexpectedly present in shared store before migration: %v", err)
	}

	roots, err := resolveGitSandboxRoots(context.Background(), wt)
	if err != nil {
		t.Fatalf("resolveGitSandboxRoots: %v", err)
	}
	if _, err := prepareGitSandboxOverlay(context.Background(), wt, sandboxHome, roots); err != nil {
		t.Fatalf("prepareGitSandboxOverlay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sharedObjects, head[:2], head[2:])); err != nil {
		t.Fatalf("migrated commit missing from shared store: %v", err)
	}
	if _, err := os.Stat(legacyObjects); !os.IsNotExist(err) {
		t.Fatalf("verified legacy object overlay was not reset: %v", err)
	}
	verify := exec.Command("git", "cat-file", "-e", head+"^{commit}")
	verify.Dir = bare
	verify.Env = gitSandboxDiscoveryEnv()
	if out, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("migrated commit not independently readable: %v: %s", err, out)
	}
}

func TestPrepareGitSandboxOverlay_DarwinRemovesEmptyLegacyObjectDirs(t *testing.T) {
	sandboxHome := t.TempDir()
	legacyObjects := filepath.Join(sandboxHome, ".sybra-git-overlay", "objects")
	for _, dir := range []string{legacyObjects, filepath.Join(legacyObjects, "info"), filepath.Join(legacyObjects, "pack")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, metadata := range []string{
		filepath.Join("info", "commit-graph"),
		filepath.Join("info", "commit-graphs", "commit-graph-chain"),
		filepath.Join("info", "packs"),
	} {
		path := filepath.Join(legacyObjects, metadata)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("derived"), 0o444); err != nil {
			t.Fatal(err)
		}
	}

	_, err := prepareGitSandboxOverlay(context.Background(), t.TempDir(), sandboxHome, gitSandboxRoots{objectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("prepareGitSandboxOverlay: %v", err)
	}
	if _, err := os.Stat(legacyObjects); !os.IsNotExist(err) {
		t.Fatalf("metadata-only legacy object dirs were not reset: %v", err)
	}
}

func TestPrepareGitSandboxOverlay_DarwinPreservesLegacyPack(t *testing.T) {
	sandboxHome := t.TempDir()
	legacyPack := filepath.Join(sandboxHome, ".sybra-git-overlay", "objects", "pack", "pack-deadbeef.pack")
	if err := os.MkdirAll(filepath.Dir(legacyPack), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPack, []byte("legacy"), 0o444); err != nil {
		t.Fatal(err)
	}

	_, err := prepareGitSandboxOverlay(context.Background(), t.TempDir(), sandboxHome, gitSandboxRoots{objectDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unsupported legacy Git object payload") {
		t.Fatalf("prepareGitSandboxOverlay error = %v, want preserved legacy-pack failure", err)
	}
	if _, err := os.Stat(legacyPack); err != nil {
		t.Fatalf("legacy pack was not preserved: %v", err)
	}
}

func TestPrepareGitSandboxOverlay_DarwinPreservesLegacyAlternates(t *testing.T) {
	sandboxHome := t.TempDir()
	legacyAlternates := filepath.Join(sandboxHome, ".sybra-git-overlay", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(legacyAlternates), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyAlternates, []byte("/objects/elsewhere\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	_, err := prepareGitSandboxOverlay(context.Background(), t.TempDir(), sandboxHome, gitSandboxRoots{objectDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unsupported legacy Git object payload") {
		t.Fatalf("prepareGitSandboxOverlay error = %v, want preserved legacy-alternates failure", err)
	}
	if _, err := os.Stat(legacyAlternates); err != nil {
		t.Fatalf("legacy alternates file was not preserved: %v", err)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitSandboxDiscoveryEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
	return string(out)
}

// runGitOrFatal runs a git command in dir, unsandboxed, for test setup.
func runGitOrFatal(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
}

// setupLinkedWorktree builds a bare clone + linked worktree pair matching a
// real Sybra task layout: a shared bare clone (analogous to
// ~/.sybra/clones/<owner>/<repo>.git) with a linked worktree checked out from
// it on its own branch, and the standard tracking refspec CloneBare
// configures so `git fetch origin` actually updates refs/remotes/origin/*.
func setupLinkedWorktree(t *testing.T) (bare, worktree string) {
	t.Helper()
	src := t.TempDir()
	runGitOrFatal(t, src, "init", "-q", "-b", "main")
	runGitOrFatal(t, src, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")

	bare = filepath.Join(t.TempDir(), "bare.git")
	runGitOrFatal(t, "", "clone", "-q", "--bare", src, bare)
	runGitOrFatal(t, bare, "config", "user.email", "t@t")
	runGitOrFatal(t, bare, "config", "user.name", "t")
	runGitOrFatal(t, bare, "config", "commit.gpgsign", "false")
	runGitOrFatal(t, bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")

	worktree = filepath.Join(t.TempDir(), "wt")
	runGitOrFatal(t, bare, "worktree", "add", worktree, "-b", "fix/task-branch", "main")

	// Advance upstream after the worktree is created, so fetch+merge below
	// has real forward progress to make, not a no-op.
	runGitOrFatal(t, src, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "upstream change")
	return bare, worktree
}

// buildEnforceCfg drives the exact production discovery pipeline
// (resolveGitSandboxRoots → enforceSpec) that Manager.buildEnforceSpec uses,
// rather than hand-constructing a sandboxSpec — a hand-built spec can grant
// paths a real run would never compute the same way, silently passing a test
// that a production run would fail.
func buildEnforceCfg(t *testing.T, worktree string) *RunConfig {
	t.Helper()
	wtCanon, err := canonicalizeRoot(worktree)
	if err != nil {
		t.Fatalf("canonicalizeRoot(worktree): %v", err)
	}
	gitRoots, err := resolveGitSandboxRoots(context.Background(), wtCanon)
	if err != nil {
		t.Fatalf("resolveGitSandboxRoots: %v", err)
	}
	if err := prepareGitLooseObjectDirs(gitRoots.objectDir); err != nil {
		t.Fatalf("prepare loose object dirs: %v", err)
	}
	spec := enforceSpec(wtCanon, nil, wtCanon, wtCanon, "", wtCanon, "", "", gitRoots, gitSandboxOverlay{})
	cfg := &RunConfig{sandbox: spec}
	if err := injectSandboxGitEnv(cfg, gitRoots, gitSandboxOverlay{}); err != nil {
		t.Fatalf("inject sandbox git env: %v", err)
	}
	return cfg
}

func newDarwinSandboxCmd(cfg *RunConfig, name string, args ...string) *exec.Cmd {
	cmd := newProviderCmd(context.Background(), cfg, false, name, args...)
	cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	return cmd
}

// TestSandboxEnforce_FullGitWorkflowSucceeds reproduces task 24849431's
// EPERM end to end: fetch, fast-forward merge, add, and commit — the exact
// operations the task's own diagnosis named — run for real under
// sandbox-exec, through the same discovery pipeline (resolveGitSandboxRoots,
// enforceSpec, newProviderCmd → wrapInvocation) a production dispatch uses.
// A linked worktree's gitdir (HEAD/index/logs/HEAD) and its branch's own
// ref/reflog live outside WORKTREE, under the shared bare clone; without
// these grants git fails partway through with the reported EPERM.
func TestSandboxEnforce_FullGitWorkflowSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "set -e; cd " + wt + "\n" +
		"git fetch origin\n" +
		"git merge --ff-only refs/remotes/origin/main\n" +
		"echo hello > f.txt\n" +
		"git add f.txt\n" +
		"git commit -q -m 'task change'\n" +
		"echo WORKFLOW_OK"
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	got := string(out)

	if err != nil {
		t.Fatalf("fetch+merge+add+commit failed under sandbox (err=%v): %s", err, got)
	}
	if !strings.Contains(got, "WORKFLOW_OK") {
		t.Errorf("workflow did not reach its final step: %s", got)
	}

	log := exec.Command("git", "log", "--format=%s", "-1")
	log.Dir = wt
	logOut, logErr := log.CombinedOutput()
	if logErr != nil || strings.TrimSpace(string(logOut)) != "task change" {
		t.Errorf("commit did not land as expected (err=%v): %s", logErr, logOut)
	}
}

func TestSandboxEnforce_SignedCommitSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	if out, err := exec.Command("git", "config", "--global", "--get", "user.signingkey").Output(); err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skip("no GPG signing key configured on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", "cd "+wt+" && echo signed > signed.txt && git add signed.txt && git commit -q -S -m signed")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed signed commit: %v: %s", err, out)
	}
}

func TestSandboxEnforce_LargeFetchUnpacksLooseObjects(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	remoteCmd := exec.Command("git", "remote", "get-url", "origin")
	remoteCmd.Dir = bare
	remoteOut, err := remoteCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve test remote: %v: %s", err, remoteOut)
	}
	remote := strings.TrimSpace(string(remoteOut))
	for i := range 160 {
		name := filepath.Join(remote, "bulk", fmt.Sprintf("file-%03d.txt", i))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatalf("mkdir bulk fixture: %v", err)
		}
		if err := os.WriteFile(name, fmt.Appendf(nil, "payload-%03d\n", i), 0o644); err != nil {
			t.Fatalf("write bulk fixture: %v", err)
		}
	}
	runGitOrFatal(t, remote, "add", "bulk")
	runGitOrFatal(t, remote, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "large upstream change")

	cfg := buildEnforceCfg(t, wt)
	before := gitRepoWideMaintenanceState(t, bare)
	cmd := newDarwinSandboxCmd(cfg, "git", "fetch", "origin")
	cmd.Dir = wt
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("large sandboxed fetch: %v: %s", err, out)
	}
	if after := gitRepoWideMaintenanceState(t, bare); after != before {
		t.Fatalf("large fetch wrote shared pack/info state instead of loose objects\nbefore: %s\nafter: %s", before, after)
	}
	runGitOrFatal(t, wt, "cat-file", "-e", "refs/remotes/origin/main^{commit}")
}

func TestSandboxEnforce_TmpAliasAllowsHelpersButKeepsOtherTempRootsReadOnly(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	worktree := t.TempDir()
	sandboxHome := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-darwin-tmp-alias",
		Mode:        "headless",
		Dir:         worktree,
		SandboxMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if got := cfg.sandbox.tmpAlias; got != `^/private/tmp/claude-[^/]+-cwd(/.*)?$` {
		t.Fatalf("sandbox tmpAlias = %q, want narrow Claude cwd pattern", got)
	}

	canonFile := filepath.Join(cfg.sandbox.tmp, "sybra-enforce-canon-probe")
	aliasDir := filepath.Join("/tmp", "claude-sybra-cwd")
	aliasFile := filepath.Join(aliasDir, "sybra-enforce-alias-probe")
	deniedFile := filepath.Join("/private/var/tmp", "sybra-enforce-denied-probe")
	for _, path := range []string{canonFile, aliasFile, deniedFile} {
		_ = os.Remove(path)
		t.Cleanup(func() { _ = os.Remove(path) })
	}
	if err := os.MkdirAll(aliasDir, 0o700); err != nil {
		t.Fatalf("create provider-owned alias dir: %v", err)
	}

	script := fmt.Sprintf(
		"set -e; echo canon > %q; echo alias > %q; (echo leak > %q 2>/dev/null && echo LEAK) || echo DENIED",
		canonFile, aliasFile, deniedFile,
	)
	cmd := newProviderCmd(context.Background(), &cfg, false, "sh", "-c", script)
	cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("tmp alias probe failed: %v: %s", err, got)
	}
	if strings.Contains(got, "LEAK") || !strings.Contains(got, "DENIED") {
		t.Fatalf("unexpected tmp alias probe output: %q", got)
	}
	for _, path := range []string{canonFile, aliasFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected writable temp path %q missing: %v", path, err)
		}
	}
	if _, err := os.Stat(deniedFile); !os.IsNotExist(err) {
		t.Fatalf("unrelated temp path %q became writable: %v", deniedFile, err)
	}
}

// TestSandboxEnforce_BlocksSharedCloneMaintenance proves that provider-run
// maintenance cannot mutate repository-wide pack or packed-ref state. Each
// command gets a fresh clone because a failed maintenance operation must not
// influence the next assertion.
func TestSandboxEnforce_BlocksSharedCloneMaintenance(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	for _, tc := range []struct {
		name               string
		args               string
		requireNonzeroExit bool
	}{
		{name: "gc", args: "gc", requireNonzeroExit: true},
		{name: "gc-reflog-bypass", args: "-c gc.packRefs=false -c gc.reflogExpire=now -c gc.reflogExpireUnreachable=now gc --prune=never", requireNonzeroExit: true},
		{name: "repack", args: "repack -ad", requireNonzeroExit: true},
		{name: "pack-refs", args: "pack-refs --all", requireNonzeroExit: true},
		// Git treats prune-packed as a best-effort maintenance subtask and may
		// return success after Seatbelt denies its unlink. The security contract
		// is the complete shared-state snapshot below, not the umbrella exit code.
		{name: "maintenance", args: "maintenance run --task=loose-objects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bare, wt := setupLinkedWorktree(t)
			if tc.name == "maintenance" {
				seedPackedLooseObject(t, bare, wt)
			}
			cfg := buildEnforceCfg(t, wt)
			before := gitMaintenanceState(t, bare)
			cmd := newDarwinSandboxCmd(cfg, "sh", "-c", "cd "+wt+" && git "+tc.args)
			out, err := cmd.CombinedOutput()
			if err == nil && tc.requireNonzeroExit {
				t.Fatalf("git %s unexpectedly succeeded: %s", tc.args, out)
			}
			if after := gitMaintenanceState(t, bare); after != before {
				t.Fatalf("git %s partially mutated shared maintenance state\nbefore: %s\nafter:  %s\noutput: %s", tc.args, before, after, out)
			}
		})
	}
}

func TestSandboxEnforce_BlocksNestedObjectDirectoryBypass(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	nested := filepath.Join(bare, "objects", "nested")
	for _, dir := range []string{nested, filepath.Join(nested, "pack"), filepath.Join(nested, "info")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir nested object fixture: %v", err)
		}
	}
	cfg := buildEnforceCfg(t, wt)
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", "cd "+wt+" && GIT_OBJECT_DIRECTORY="+nested+" GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(bare, "objects")+" git repack -ad")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("nested-object repack unexpectedly succeeded: %s", out)
	}
	entries, readErr := os.ReadDir(filepath.Join(nested, "pack"))
	if readErr != nil {
		t.Fatalf("read nested pack dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("nested-object repack left shared artifacts: %v", entries)
	}
}

func gitMaintenanceState(t *testing.T, bare string) string {
	t.Helper()
	var state []string
	for _, root := range []string{filepath.Join(bare, "objects"), filepath.Join(bare, "logs")} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state = append(state, path+"="+string(content))
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot %s: %v", root, err)
		}
	}
	for _, name := range []string{"packed-refs", "packed-refs.new", "packed-refs.lock", "gc.pid", "gc.pid.lock"} {
		path := filepath.Join(bare, name)
		content, err := os.ReadFile(path)
		if err == nil {
			state = append(state, path+"="+string(content))
		} else if !os.IsNotExist(err) {
			t.Fatalf("snapshot %s: %v", path, err)
		}
	}
	sort.Strings(state)
	return strings.Join(state, "\n")
}

func gitRepoWideMaintenanceState(t *testing.T, bare string) string {
	t.Helper()
	var state []string
	for _, root := range []string{filepath.Join(bare, "objects", "pack"), filepath.Join(bare, "objects", "info")} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state = append(state, path+"="+string(content))
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot %s: %v", root, err)
		}
	}
	for _, name := range []string{"packed-refs", "packed-refs.new", "packed-refs.lock", "gc.pid", "gc.pid.lock"} {
		path := filepath.Join(bare, name)
		content, err := os.ReadFile(path)
		if err == nil {
			state = append(state, path+"="+string(content))
		} else if !os.IsNotExist(err) {
			t.Fatalf("snapshot %s: %v", path, err)
		}
	}
	sort.Strings(state)
	return strings.Join(state, "\n")
}

func seedPackedLooseObject(t *testing.T, bare, wt string) {
	t.Helper()
	rev := exec.Command("git", "rev-parse", "HEAD^{tree}")
	rev.Dir = wt
	out, err := rev.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve loose object fixture: %v: %s", err, out)
	}
	oid := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(bare, "objects", oid[:2], oid[2:])); err != nil {
		t.Fatalf("loose object fixture missing: %v", err)
	}
	pack := exec.Command("git", "pack-objects", filepath.Join(bare, "objects", "pack", "maintenance"))
	pack.Dir = bare
	pack.Stdin = strings.NewReader(oid + "\n")
	packOut, err := pack.CombinedOutput()
	if err != nil {
		t.Fatalf("pack loose object fixture: %v: %s", err, packOut)
	}
}

// TestSandboxEnforce_GitAdminDirIsolatedFromSiblingWorktree proves the
// per-task grant does not widen into a sibling task's own worktree admin
// dir sharing the same bare clone — the isolation property the narrow,
// per-task scoping depends on.
func TestSandboxEnforce_GitAdminDirIsolatedFromSiblingWorktree(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	siblingWt := filepath.Join(filepath.Dir(wt), "sibling-wt")
	runGitOrFatal(t, bare, "worktree", "add", siblingWt, "-b", "fix/sibling-branch", "main")
	siblingAdminDir := filepath.Join(bare, "worktrees", "sibling-wt")

	script := "(touch " + filepath.Join(siblingAdminDir, "HEAD") + " 2>/dev/null && echo LEAK) || echo DENIED"
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "LEAK") || !strings.Contains(got, "DENIED") {
		t.Errorf("write to a sibling worktree's admin dir must be kernel-denied: %q", got)
	}
}

// TestSandboxEnforce_SiblingBranchRefFileIsolated proves the branch-ref
// literal grant does not widen into a sibling task's own branch ref, even
// when both branches nest under the same parent path segment (both live
// under refs/heads/fix/) — the isolation property GIT_BRANCH_REF_FILE's
// literal-not-subpath design (see agent_sandbox.sb) exists to guarantee.
func TestSandboxEnforce_SiblingBranchRefFileIsolated(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	runGitOrFatal(t, bare, "branch", "fix/sibling-branch", "main")
	siblingRefFile := filepath.Join(bare, "refs", "heads", "fix", "sibling-branch")

	script := "(echo tampered > " + siblingRefFile + " 2>/dev/null && echo LEAK) || echo DENIED"
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "LEAK") || !strings.Contains(got, "DENIED") {
		t.Errorf("write to a sibling branch's own ref file must be kernel-denied: %q", got)
	}
}

// TestSandboxEnforce_InfoDirIsReadOnly proves every shared info/* file stays
// read-only. Maintenance-generated info/refs and behavior-altering attributes
// or exclude files are all repository-wide state outside normal task work.
func TestSandboxEnforce_InfoDirIsReadOnly(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "COMMON=$(cd " + wt + " && git rev-parse --git-common-dir); " +
		"(echo evil > \"$COMMON/info/attributes\" 2>/dev/null && echo LEAK_ATTR) || echo DENIED_ATTR; " +
		"(echo evil >> \"$COMMON/info/exclude\" 2>/dev/null && echo LEAK_EXCLUDE) || echo DENIED_EXCLUDE; " +
		"(echo x > \"$COMMON/info/refs\" 2>/dev/null && echo ALLOWED_REFS) || echo DENIED_REFS"
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "LEAK_ATTR") || !strings.Contains(got, "DENIED_ATTR") {
		t.Errorf("write to info/attributes must be kernel-denied: %q", got)
	}
	if strings.Contains(got, "LEAK_EXCLUDE") || !strings.Contains(got, "DENIED_EXCLUDE") {
		t.Errorf("write to info/exclude must be kernel-denied: %q", got)
	}
	if strings.Contains(got, "ALLOWED_REFS") || !strings.Contains(got, "DENIED_REFS") {
		t.Errorf("write to repository-wide info/refs must be kernel-denied: %q", got)
	}
}

// TestSandboxEnforce_ShallowFetchSucceeds covers a git operation not issued
// by Sybra's own git calls today but reachable if an agent runs one
// directly: a shallow fetch locks gitCommonDir/shallow the same way a
// normal fetch locks packed-refs.
func TestSandboxEnforce_ShallowFetchSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "cd " + wt + " && git fetch --depth=1 origin main 2>&1; echo EXIT=$?"
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "Operation not permitted") {
		t.Errorf("shallow fetch hit EPERM under sandbox: %s", got)
	}
	if !strings.Contains(got, "EXIT=0") {
		t.Errorf("shallow fetch did not exit cleanly: %s", got)
	}
}

// TestSandboxEnforce_GitStashSucceeds reproduces a gap found by an
// independent adversarial manual-test pass: refs/stash is a single,
// fixed-name ref directly under refs/, outside every ref-dir/tag-dir/
// remote-dir grant above and outside the branch's own literal grant (which
// only covers that branch's own ref, not a repo-wide stash stack) — a very
// plausible mid-task operation (stash local edits before a rebase/pull)
// that failed with "Cannot save the current status" before this grant.
func TestSandboxEnforce_GitStashSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "cd " + wt + " && echo base > f.txt && git add f.txt && git commit -q -m tracked && " +
		"echo modified >> f.txt && git stash push -m mystash 2>&1 && git stash pop 2>&1; echo EXIT=$?"
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "Operation not permitted") {
		t.Errorf("git stash hit EPERM under sandbox: %s", got)
	}
	if !strings.Contains(got, "EXIT=0") {
		t.Errorf("git stash did not exit cleanly: %s", got)
	}
}

// TestSandboxEnforce_GitNotesSucceeds reproduces a second gap found by the
// same adversarial pass: refs/notes/* is a repo-wide annotation namespace
// (like remotes/tags, not a task's own exclusive branch work), outside
// every grant above — `git notes add` failed with "cannot lock ref
// 'refs/notes/commits': unable to create directory" before this grant.
func TestSandboxEnforce_GitNotesSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "cd " + wt + " && git notes add -m 'note body' 2>&1; echo EXIT=$?"
	cmd := newDarwinSandboxCmd(cfg, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "Operation not permitted") {
		t.Errorf("git notes hit EPERM under sandbox: %s", got)
	}
	if !strings.Contains(got, "EXIT=0") {
		t.Errorf("git notes did not exit cleanly: %s", got)
	}
}
