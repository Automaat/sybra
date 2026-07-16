package agent

import (
	"errors"
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
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'REAL-GH:'\nfor a in \"$@\"; do printf ' [%s]' \"$a\"; done\nprintf '\\n'\n"
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
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

// The shim sees real argv, so every shell shape that defeated string-parsing —
// trailing separators, subshells, command substitution, quoted flags — reduces
// to the same argv here. These are the reproductions that broke the previous
// approach, expressed as the shell would actually deliver them.
func TestGhShim_BlocksApproval(t *testing.T) {
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
		{"repo flag first", []string{"pr", "review", "-R", "owner/repo", "--approve", "1"}},
		{"rest event", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews", "-f", "event=APPROVE"}},
		{"rest pending submit", []string{"api", "-X", "POST", "repos/owner/repo/pulls/1/reviews/99/events", "-f", "event=APPROVE"}},
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
			if !strings.Contains(stderr, "no PR-approval authority") {
				t.Fatalf("shim stderr missing reason for %v: %q", tc.args, stderr)
			}
		})
	}
}

// A false deny is as damaging as a bypass: it blocks the exact action the deny
// reason tells the agent to retry. Bodies arrive as one argv element, so text
// mentioning a flag can never be read as one.
func TestGhShim_AllowsLegitimateReviews(t *testing.T) {
	shimDir := newShim(t)

	tests := []struct {
		name string
		args []string
	}{
		{"comment review", []string{"pr", "review", "--comment", "-b", "summary", "1"}},
		{"request changes", []string{"pr", "review", "--request-changes", "-b", "fix this", "1"}},
		{"body mentions short flag", []string{"pr", "review", "--request-changes", "-b", "use -a instead of --all", "1"}},
		{"body mentions approve flag", []string{"pr", "review", "--comment", "-b", "do not --approve this", "1"}},
		{"body quotes approve command", []string{"pr", "review", "--comment", "-b", "never run gh pr review --approve", "1"}},
		{"body has apostrophe", []string{"pr", "review", "--request-changes", "-b", "Don't use -a here", "1"}},
		{"body mentions approve event", []string{"pr", "review", "--comment", "-b", "event: APPROVED is banned", "1"}},
		{"reading reviews", []string{"pr", "view", "1", "--json", "reviews"}},
		{"listing approved reviews", []string{"api", "graphql", "-f", `query=query { reviews(states: APPROVED) { id } }`}},
		{"merge is gated elsewhere", []string{"pr", "merge", "1", "--squash"}},
		{"unrelated", []string{"repo", "view"}},
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
		"pr", "review", "--comment", "-b", "two  spaces and 'quotes' and $VAR", "1")
	if code != 0 {
		t.Fatalf("unexpected block: exit=%d", code)
	}
	want := "REAL-GH: [pr] [review] [--comment] [-b] [two  spaces and 'quotes' and $VAR] [1]"
	if strings.TrimSpace(stdout) != want {
		t.Fatalf("argv not forwarded verbatim:\n got %q\nwant %q", strings.TrimSpace(stdout), want)
	}
}

func TestWriteGhShim_NoGhInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir, err := writeGhShim(t.TempDir())
	if err != nil {
		t.Fatalf("writeGhShim: %v", err)
	}
	if dir != "" {
		t.Fatalf("writeGhShim returned %q, want empty when gh is absent", dir)
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
