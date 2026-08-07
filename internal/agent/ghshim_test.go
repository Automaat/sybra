package agent

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGhOnPath installs a `gh` that echoes its argv, so a test can tell an
// executed call from a blocked one and assert what the real binary would have
// received.
func fakeGhOnPath(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'REAL-GH:'\nfor a in \"$@\"; do printf ' [%s]' \"$a\"; done\nprintf '\\n'\n"
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "sybra-cli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func newShim(t *testing.T) string {
	t.Helper()
	fakeGhOnPath(t)
	dir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}
	if dir == "" {
		t.Fatal("writeGhShim returned empty dir despite gh on PATH")
	}
	return dir
}

func runShim(t *testing.T, shimDir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(shimDir, "gh"), args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run shim: %v", err)
	}
	return out.String(), errBuf.String(), code
}

func runCredentialShim(t *testing.T, shimDir, input string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(shimDir, "git-credential-sybra"), args...)
	cmd.Stdin = strings.NewReader(input)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run git credential shim: %v", err)
	}
	return out.String(), errBuf.String(), code
}

func TestGhShim_MintsFreshAppTokenPerGhInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ghDir := t.TempDir()
	realGh := filepath.Join(ghDir, "gh")
	ghScript := "#!/bin/sh\nprintf 'REAL-GH GH_TOKEN=%s GITHUB_TOKEN=%s:' \"$GH_TOKEN\" \"$GITHUB_TOKEN\"\nfor a in \"$@\"; do printf ' [%s]' \"$a\"; done\nprintf '\\n'\n"
	if err := os.WriteFile(realGh, []byte(ghScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realGh, 0o755); err != nil {
		t.Fatal(err)
	}

	cliDir := t.TempDir()
	counter := filepath.Join(cliDir, "counter")
	fakeCLI := filepath.Join(cliDir, "sybra-cli")
	cliScript := "#!/bin/sh\n[ \"$1\" = \"github-app-token\" ] || exit 2\nn=$(cat '" + counter + "' 2>/dev/null || echo 0)\nn=$((n + 1))\nprintf '%s\\n' \"$n\" > '" + counter + "'\nprintf 'token-%s\\n' \"$n\"\n"
	if err := os.WriteFile(fakeCLI, []byte(cliScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeCLI, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}

	stdout, stderr, code := runShim(t, shimDir, "api", "rate_limit")
	if code != 0 {
		t.Fatalf("first shim call failed: exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "GH_TOKEN=token-1 GITHUB_TOKEN=token-1") {
		t.Fatalf("first call did not use freshly minted token: %q", stdout)
	}

	stdout, stderr, code = runShim(t, shimDir, "api", "rate_limit")
	if code != 0 {
		t.Fatalf("second shim call failed: exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "GH_TOKEN=token-2 GITHUB_TOKEN=token-2") {
		t.Fatalf("second call reused stale token or missed the helper: %q", stdout)
	}
}

func TestGhShimPreservesPreScopedVerifierToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ghDir := t.TempDir()
	realGh := filepath.Join(ghDir, "gh")
	if err := os.WriteFile(realGh, []byte("#!/bin/sh\nprintf 'GH=%s GITHUB=%s\\n' \"$GH_TOKEN\" \"$GITHUB_TOKEN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cliDir := t.TempDir()
	marker := filepath.Join(cliDir, "minted")
	fakeCLI := filepath.Join(cliDir, "sybra-cli")
	if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\nprintf full-token\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "contents-read-pr-write-token")
	t.Setenv("GITHUB_TOKEN", "contents-read-pr-write-token")

	stdout, stderr, code := runShim(t, filepath.Join(shimDir, "verifier"), "pr", "view", "1")
	if code != 0 || !strings.Contains(stdout, "GH=contents-read-pr-write-token GITHUB=contents-read-pr-write-token") {
		t.Fatalf("shim replaced scoped token: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("full-token mint helper ran despite pre-scoped token: %v", err)
	}
}

func TestVerifierGhShimNeverMintsBroadToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cliDir := t.TempDir()
	marker := filepath.Join(cliDir, "minted")
	if err := os.WriteFile(filepath.Join(cliDir, "sybra-cli"), []byte("#!/bin/sh\ntouch '"+marker+"'\nprintf full-token\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	_, stderr, code := runShim(t, filepath.Join(shimDir, "verifier"), "pr", "view", "1")
	if code == 0 || !strings.Contains(stderr, "restricted verifier GitHub token is unavailable") {
		t.Fatalf("verifier shim fallback = exit %d stderr %q", code, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("verifier shim invoked broad-token helper: %v", err)
	}
}

func TestVerifierGhShimUsesRotatedManagerToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ghDir := t.TempDir()
	realGh := filepath.Join(ghDir, "gh")
	if err := os.WriteFile(realGh, []byte("#!/bin/sh\nprintf 'GH=%s GITHUB=%s\\n' \"$GH_TOKEN\" \"$GITHUB_TOKEN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{ghShimDir: shimDir, logger: slog.New(slog.DiscardHandler)}
	token := "rotated-restricted-token"
	m.SetGHVerifierAppToken(func() string { return token })
	t.Setenv("GH_TOKEN", "stale-restricted-token")
	t.Setenv("GITHUB_TOKEN", "stale-restricted-token")

	stdout, stderr, code := runShim(t, filepath.Join(shimDir, "verifier"), "pr", "view", "1")
	if code != 0 || !strings.Contains(stdout, "GH=rotated-restricted-token GITHUB=rotated-restricted-token") {
		t.Fatalf("shim did not use rotated token: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	token = ""
	if err := m.SyncGHVerifierAppToken(); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runShim(t, filepath.Join(shimDir, "verifier"), "pr", "view", "1")
	if code == 0 || !strings.Contains(stderr, "restricted verifier GitHub token is unavailable") {
		t.Fatalf("blank rotated token reused stale environment: exit=%d stderr=%q", code, stderr)
	}
}

func TestGhShim_UsesResolvedSybraCLIWhenInvocationPathOmitsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ghDir := t.TempDir()
	realGh := filepath.Join(ghDir, "gh")
	ghScript := "#!/bin/sh\nprintf 'REAL-GH GH_TOKEN=%s GITHUB_TOKEN=%s\\n' \"$GH_TOKEN\" \"$GITHUB_TOKEN\"\n"
	if err := os.WriteFile(realGh, []byte(ghScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realGh, 0o755); err != nil {
		t.Fatal(err)
	}

	cliDir := t.TempDir()
	fakeCLI := filepath.Join(cliDir, "sybra-cli")
	cliScript := "#!/bin/sh\n[ \"$1\" = \"github-app-token\" ] || exit 2\nprintf 'resolved-token\\n'\n"
	if err := os.WriteFile(fakeCLI, []byte(cliScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeCLI, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}

	t.Setenv("PATH", ghDir)
	stdout, stderr, code := runShim(t, shimDir, "api", "rate_limit")
	if code != 0 {
		t.Fatalf("shim call failed without sybra-cli on PATH: exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "GH_TOKEN=resolved-token GITHUB_TOKEN=resolved-token") {
		t.Fatalf("shim did not use resolved sybra-cli path: %q", stdout)
	}
}

func TestLookRealSybraCLI_ReturnsAbsolutePath(t *testing.T) {
	fakeGhOnPath(t)
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeCLI := filepath.Join(binDir, "sybra-cli")
	if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeCLI, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("PATH", "bin")

	got := lookRealSybraCLI()
	if !filepath.IsAbs(got) {
		t.Fatalf("lookRealSybraCLI() = %q, want absolute path", got)
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat resolved path %q: %v", got, err)
	}
	wantInfo, err := os.Stat(fakeCLI)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("lookRealSybraCLI() = %q, not same file as %q", got, fakeCLI)
	}
}

func TestLookRealSybraCLI_PrefersStableHomeInstallOverStalePathEntry(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	staleDir := filepath.Join(root, "mise", "installs", "go", "old", "bin")
	stableDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleCLI := filepath.Join(staleDir, "sybra-cli")
	stableCLI := filepath.Join(stableDir, "sybra-cli")
	if err := os.WriteFile(staleCLI, []byte("#!/bin/sh\nexit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staleCLI, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stableCLI, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stableCLI, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", staleDir+string(os.PathListSeparator)+stableDir)

	got := lookRealSybraCLI()
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat resolved path %q: %v", got, err)
	}
	wantInfo, err := os.Stat(stableCLI)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("lookRealSybraCLI() = %q, want stable home install %q over stale PATH entry %q", got, stableCLI, staleCLI)
	}
}

func TestGitCredentialShim_MintsFreshAppTokenPerLookup(t *testing.T) {
	fakeGhOnPath(t)
	cliDir := t.TempDir()
	counter := filepath.Join(cliDir, "counter")
	fakeCLI := filepath.Join(cliDir, "sybra-cli")
	cliScript := "#!/bin/sh\n[ \"$1\" = \"github-app-token\" ] || exit 2\nn=$(cat '" + counter + "' 2>/dev/null || echo 0)\nn=$((n + 1))\nprintf '%s\\n' \"$n\" > '" + counter + "'\nprintf 'token-%s\\n' \"$n\"\n"
	if err := os.WriteFile(fakeCLI, []byte(cliScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeCLI, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}

	stdout, stderr, code := runCredentialShim(t, shimDir, "protocol=https\nhost=github.com\n\n", "get")
	if code != 0 {
		t.Fatalf("first credential lookup failed: exit=%d stderr=%q", code, stderr)
	}
	if stdout != "username=x-access-token\npassword=token-1\n" {
		t.Fatalf("first credential lookup = %q, want fresh token-1", stdout)
	}

	stdout, stderr, code = runCredentialShim(t, shimDir, "protocol=https\nhost=github.com\n\n", "get")
	if code != 0 {
		t.Fatalf("second credential lookup failed: exit=%d stderr=%q", code, stderr)
	}
	if stdout != "username=x-access-token\npassword=token-2\n" {
		t.Fatalf("second credential lookup = %q, want fresh token-2", stdout)
	}
}

func TestGitCredentialShim_IgnoresNonGitHubAndStoreErase(t *testing.T) {
	fakeGhOnPath(t)
	cliDir := t.TempDir()
	marker := filepath.Join(cliDir, "minted")
	fakeCLI := filepath.Join(cliDir, "sybra-cli")
	cliScript := "#!/bin/sh\n[ \"$1\" = \"github-app-token\" ] || exit 2\nprintf minted > '" + marker + "'\nprintf 'token\\n'\n"
	if err := os.WriteFile(fakeCLI, []byte(cliScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeCLI, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}

	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{"non github", []string{"get"}, "protocol=https\nhost=example.com\n\n"},
		{"plain http github", []string{"get"}, "protocol=http\nhost=github.com\n\n"},
		{"ssh github", []string{"get"}, "protocol=ssh\nhost=github.com\n\n"},
		{"store", []string{"store"}, "protocol=https\nhost=github.com\n\n"},
		{"erase", []string{"erase"}, "protocol=https\nhost=github.com\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCredentialShim(t, shimDir, tc.input, tc.args...)
			if code != 0 || stdout != "" || stderr != "" {
				t.Fatalf("credential shim = stdout %q stderr %q code %d, want quiet success", stdout, stderr, code)
			}
		})
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("credential shim minted token for a non-credential lookup")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}

func TestGhShim_DoesNotMintAppTokenForBlockedInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ghDir := t.TempDir()
	realGh := filepath.Join(ghDir, "gh")
	if err := os.WriteFile(realGh, []byte("#!/bin/sh\nprintf 'REAL-GH\\n'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realGh, 0o755); err != nil {
		t.Fatal(err)
	}

	cliDir := t.TempDir()
	marker := filepath.Join(cliDir, "minted")
	fakeCLI := filepath.Join(cliDir, "sybra-cli")
	cliScript := "#!/bin/sh\n[ \"$1\" = \"github-app-token\" ] || exit 2\nprintf minted > '" + marker + "'\nprintf 'token\\n'\n"
	if err := os.WriteFile(fakeCLI, []byte(cliScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeCLI, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shimDir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}

	stdout, stderr, code := runShim(t, shimDir, "pr", "review", "--approve", "1")
	if code == 0 {
		t.Fatal("blocked invocation unexpectedly succeeded")
	}
	if strings.Contains(stdout, "REAL-GH") {
		t.Fatalf("blocked invocation reached real gh: %q", stdout)
	}
	if !strings.Contains(stderr, GhShimReason) {
		t.Fatalf("stderr = %q, want gh shim reason", stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("blocked invocation minted a GitHub App token")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}

// The shim sees real argv, so every shell shape that defeated string-parsing —
// trailing separators, subshells, command substitution, quoted flags — reduces
// to the same argv here. These are the reproductions that broke the previous
// approach, expressed as the shell would actually deliver them.
func TestGhShim_BlocksSubmittedReviews(t *testing.T) {
	shimDir := newShim(t)

	tests := []struct {
		name string
		args []string
	}{
		{"long flag", []string{"pr", "review", "--approve", "1"}},
		{"short flag", []string{"pr", "review", "-a", "1"}},
		{"flag after number", []string{"pr", "review", "1", "--approve"}},
		{"flag with value", []string{"pr", "review", "1", "--approve=true"}},
		{"approve with body", []string{"pr", "review", "1", "--approve", "-b", "lgtm"}},
		{"approve after a body flag", []string{"pr", "review", "-b", "lgtm", "--approve", "1"}},
		{"approve after a body-file flag", []string{"pr", "review", "-F", "notes.md", "--approve", "1"}},
		{"approve after an inline body", []string{"pr", "review", "--body=lgtm", "--approve", "1"}},
		{"repo flag first", []string{"pr", "review", "-R", "owner/repo", "--approve", "1"}},
		{"no verdict flag at all", []string{"pr", "review", "1", "-b", "lgtm"}},
		{"no verdict flag, long body only", []string{"pr", "review", "1"}},
		{"unrecognized short flag", []string{"pr", "review", "1", "-z"}},
		{"approve bundled with comment", []string{"pr", "review", "1", "-ac"}},
		{"rest approve event", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "event=APPROVE"}},
		{"graphql add review", []string{"api", "graphql", "-f", `query=mutation { addPullRequestReview(input: {event: APPROVED}) { id } }`}},
		{"graphql submit review", []string{"api", "graphql", "-f", `query=mutation { submitPullRequestReview(input: {event: APPROVE}) { id } }`}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runShim(t, shimDir, tc.args...)
			if code == 0 {
				t.Fatalf("shim allowed %v (exit 0)", tc.args)
			}
			if strings.Contains(stdout, "REAL-GH") {
				t.Fatalf("shim executed real gh for %v: %s", tc.args, stdout)
			}
			if !strings.Contains(stderr, GhShimReason) {
				t.Fatalf("shim stderr missing reason for %v: %q", tc.args, stderr)
			}
		})
	}
}

// A false deny is as damaging as a bypass: it blocks the exact action the deny
// reason tells the agent to retry.
func TestGhShim_AllowsPendingDraftReviews(t *testing.T) {
	shimDir := newShim(t)

	tests := []struct {
		name string
		args []string
	}{
		{"clean pending review", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "body=clean"}},
		{"pending review with inline comment", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "body=needs work", "-f", "comments[][path]=main.go", "-f", "comments[][line]=12", "-f", "comments[][side]=RIGHT", "-f", "comments[][body]=fix this"}},
		{"pending review path contains event comment", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "body=summary", "-f", "comments[][path]=internal/event/comment.go", "-f", "comments[][line]=12", "-f", "comments[][side]=RIGHT", "-f", "comments[][body]=fix this"}},
		{"body mentions approve event", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "body=event: APPROVED is banned"}},
		{
			// A multi-paragraph markdown body with a footer is impractical to
			// pass inline (quoting), so gh api's own docs recommend @file for it.
			// gh api scopes a file value to exactly the named field, so this
			// cannot smuggle a sibling "event" key the way --input/query=@ can.
			name: "pending review with file-sourced body",
			args: []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "commit_id=abc123", "-F", "body=@review_body.txt"},
		},
		{
			name: "pending review with file-sourced inline comment bodies",
			args: []string{
				"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews",
				"-f", "commit_id=abc123", "-F", "body=@review_body.txt",
				"-f", "comments[][path]=main.go", "-F", "comments[][line]=12", "-f", "comments[][side]=RIGHT", "-F", "comments[][body]=@c1.txt",
			},
		},
		{"reading reviews", []string{"pr", "view", "1", "--json", "reviews"}},
		{"listing approved reviews", []string{"api", "graphql", "-f", `query=query { reviews(states: APPROVED) { id } }`}},
		{"merge is gated elsewhere", []string{"pr", "merge", "1", "--squash"}},
		{"unrelated", []string{"repo", "view"}},
		{"comment review", []string{"pr", "review", "--comment", "-b", "summary", "1"}},
		{"request changes", []string{"pr", "review", "--request-changes", "-b", "fix this", "1"}},
		{"request changes short flag", []string{"pr", "review", "1", "-r", "-b", "fix this"}},
		{"comment with repo flag", []string{"pr", "review", "-R", "owner/repo", "--comment", "-b", "fix this", "1"}},
		// A body value that starts with '-' is a value, not a flag bundle: it must
		// not be misread as an unrecognized short flag and blocked.
		{"request changes with dash-leading body", []string{"pr", "review", "123", "--request-changes", "-b", "-1, this design has issues"}},
		{"comment with dash-leading body-file", []string{"pr", "review", "123", "--comment", "-F", "-notes.md"}},
		{"rest comment event", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "event=COMMENT"}},
		{"rest request changes event", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "event=REQUEST_CHANGES"}},
		{"rest pending submit with comment event", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews/99/events", "-f", "event=COMMENT"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runShim(t, shimDir, tc.args...)
			if code != 0 {
				t.Fatalf("shim blocked legitimate %v: exit=%d stderr=%q", tc.args, code, stderr)
			}
			if !strings.Contains(stdout, "REAL-GH") {
				t.Fatalf("shim did not exec real gh for %v: %q", tc.args, stdout)
			}
		})
	}
}

// The shim must hand the real binary exactly what it was given: a guard that
// mangles argv would silently corrupt every review the agent posts.
func TestGhShim_ForwardsArgvVerbatim(t *testing.T) {
	shimDir := newShim(t)

	stdout, _, code := runShim(t, shimDir,
		"api", "repos/o/r/pulls/1/reviews", "--method", "POST", "-f", "body=two  spaces and 'quotes' and $VAR")
	if code != 0 {
		t.Fatalf("unexpected block: exit=%d", code)
	}
	want := "REAL-GH: [api] [repos/o/r/pulls/1/reviews] [--method] [POST] [-f] [body=two  spaces and 'quotes' and $VAR]"
	if strings.TrimSpace(stdout) != want {
		t.Fatalf("argv not forwarded verbatim:\n got %q\nwant %q", strings.TrimSpace(stdout), want)
	}
}

// A restart rewrites the shim while agents may be exec'ing it, so the install
// must be atomic and leave no staging files behind on PATH.
func TestWriteGhShim_RewriteIsAtomicAndIdempotent(t *testing.T) {
	fakeGhOnPath(t)
	dir := t.TempDir()

	for range 3 {
		if _, err := writeGhShim(dir); err != nil {
			t.Fatalf("writeGhShim: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	wantNames := []string{"gh", "git-credential-sybra", "verifier"}
	slices.Sort(names)
	if !slices.Equal(names, wantNames) {
		t.Fatalf("shim dir = %v, want exactly %v (no staging leftovers)", names, wantNames)
	}

	for _, name := range []string{"gh", "git-credential-sybra"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s mode = %v, want executable", name, info.Mode().Perm())
		}
	}
	verifierEntries, err := os.ReadDir(filepath.Join(dir, "verifier"))
	if err != nil {
		t.Fatal(err)
	}
	if len(verifierEntries) != 1 || verifierEntries[0].Name() != "gh" {
		t.Fatalf("verifier shim dir = %v, want exactly [gh] (no staging leftovers)", verifierEntries)
	}
	verifierInfo, err := os.Stat(filepath.Join(dir, "verifier", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	if verifierInfo.Mode().Perm()&0o111 == 0 {
		t.Fatalf("verifier gh mode = %v, want executable", verifierInfo.Mode().Perm())
	}

	_, _, code := runShim(t, dir, "pr", "review", "--approve", "1")
	if code == 0 {
		t.Fatal("rewritten shim allowed a submitted review")
	}
}

func TestWriteGhShim_NoGhInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	dir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}
	if dir == "" {
		t.Fatal("writeGhShim returned empty; git credential helper should still be installed without gh")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "git-credential-sybra" {
		t.Fatalf("shim dir entries = %v, want only git-credential-sybra", entries)
	}
}

func TestPrependPATH(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	sep := string(os.PathListSeparator)

	got := prependPATH(nil, "/shims")
	if len(got) != 1 || got[0] != "PATH=/shims"+sep+"/usr/bin" {
		t.Fatalf("prependPATH(nil) = %v", got)
	}

	got = prependPATH([]string{"FOO=bar", "PATH=/custom"}, "/shims")
	if !slices.Contains(got, "PATH=/shims"+sep+"/custom") {
		t.Fatalf("prependPATH did not extend the caller PATH: %v", got)
	}
	if !slices.Contains(got, "FOO=bar") {
		t.Fatalf("prependPATH dropped unrelated env: %v", got)
	}
	var pathCount int
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") {
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Fatalf("prependPATH left %d PATH entries: %v", pathCount, got)
	}
}

// Review submission bypasses defeat argv scanning the same way: they move EVENT
// somewhere argv cannot see it. The shim cannot read a request body, so a payload
// it cannot inspect on a reviews endpoint is refused rather than assumed benign.
func TestGhShim_BlocksReviewEventsInvisibleToArgv(t *testing.T) {
	shim := newShim(t)
	tests := []struct {
		name string
		args []string
	}{
		{
			// The body never appears in argv at all.
			name: "review body piped on stdin",
			args: []string{"api", "repos/o/r/pulls/151/reviews", "--method", "POST", "--input", "-"},
		},
		{
			name: "review body read from a file",
			args: []string{"api", "repos/o/r/pulls/151/reviews", "--method", "POST", "--input", "/tmp/body.json"},
		},
		{
			name: "review body via --input=",
			args: []string{"api", "repos/o/r/pulls/151/reviews", "--input=/tmp/body.json"},
		},
		{
			// EVENT and APPROVE land in different argv elements, so the
			// event=APPROVE glob never matches either one.
			name: "graphql mutation with the event in a variable",
			args: []string{
				"api", "graphql",
				"-f", "query=mutation($e:PullRequestReviewEvent!){addPullRequestReview(input:{event:$e}){clientMutationId}}",
				"-F", "e=APPROVE",
			},
		},
		{
			name: "graphql mutation with the event inline",
			args: []string{
				"api", "graphql",
				"-f", "query=mutation{addPullRequestReview(input:{pullRequestId:\"x\",event:APPROVE}){clientMutationId}}",
			},
		},
		{
			name: "graphql submit mutation with the event in a variable",
			args: []string{
				"api", "graphql",
				"-f", "query=mutation($id:ID!,$e:PullRequestReviewEvent!){submitPullRequestReview(input:{pullRequestReviewId:$id,event:$e}){pullRequestReview{id}}}",
				"-f", "id=PRR_kwDO",
				"-f", "e=APPROVE",
			},
		},
		{
			// -F key=@path reads the value from a file, so the whole mutation
			// stays out of argv — the same hole as --input, one field wide.
			name: "graphql query read from a file",
			args: []string{"api", "graphql", "-F", "query=@/tmp/mutation.graphql"},
		},
		{
			name: "graphql query read from stdin",
			args: []string{"api", "graphql", "-F", "query=@-"},
		},
		{
			name: "review event read from a file",
			args: []string{"api", "repos/o/r/pulls/151/reviews", "-F", "event=@/tmp/event.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stderr, code := runShim(t, shim, tt.args...)
			if code == 0 {
				t.Fatalf("shim allowed a review event it could not inspect: %v", tt.args)
			}
			if !strings.Contains(stderr, GhShimReason) {
				t.Errorf("stderr = %q, want the shim's refusal reason", stderr)
			}
		})
	}
}

// The tightening must not cost the agent the draft reviews it is supposed to post.
// A guard that blocks legitimate work gets routed around by the next person.
func TestGhShim_AllowsInspectableAndUnrelatedCalls(t *testing.T) {
	shim := newShim(t)
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "pending review draft",
			args: []string{"api", "repos/o/r/pulls/151/reviews", "--method", "POST", "-f", "body=looks good"},
		},
		{
			// The body is prose; "approve" and "event" in it mean nothing as
			// long as no review event field is present.
			name: "draft body that merely mentions approving an event",
			args: []string{"api", "repos/o/r/pulls/151/reviews", "--method", "POST", "-f", "body=I approve of this event handler"},
		},
		{"reading reviews", []string{"api", "repos/o/r/pulls/151/reviews"}},
		{
			// --input is only suspicious when the target is a reviews endpoint.
			name: "input body on a non-review endpoint",
			args: []string{"api", "repos/o/r/issues/1/comments", "--method", "POST", "--input", "-"},
		},
		{"unrelated graphql", []string{"api", "graphql", "-f", "query=query{viewer{login}}"}},
		{
			// Variables are fine; it is a file-sourced *value* that hides intent.
			name: "graphql read query with variables",
			args: []string{
				"api", "graphql",
				"-f", "query=query($n:Int!){repository(owner:\"o\",name:\"r\"){pullRequest(number:$n){id}}}",
				"-F", "n=151",
			},
		},
		{
			name: "file-sourced body on a non-review endpoint",
			args: []string{"api", "repos/o/r/issues/1/comments", "-F", "body=@/tmp/note.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stderr, _ := runShim(t, shim, tt.args...)
			if strings.Contains(stderr, GhShimReason) {
				t.Fatalf("shim blocked a legitimate call: %v", tt.args)
			}
		})
	}
}
