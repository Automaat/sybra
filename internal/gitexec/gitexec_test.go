package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentModes(t *testing.T) {
	bin := t.TempDir()
	writeFakeGit(t, bin, `
printf '%s|%s|%s\n' "${GITEXEC_AMBIENT-unset}" "${GITEXEC_VALUE-unset}" "$GIT_TERMINAL_PROMPT"
`)
	t.Setenv("PATH", bin)
	t.Setenv("GITEXEC_AMBIENT", "inherited")
	t.Setenv("GITEXEC_VALUE", "ambient")

	tests := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "inherited",
			want: "inherited|ambient|0",
		},
		{
			name: "augmented",
			opts: Options{ExtraEnv: []string{"GITEXEC_VALUE=augmented", "GIT_TERMINAL_PROMPT=1"}},
			want: "inherited|augmented|0",
		},
		{
			name: "isolated",
			opts: Options{Env: []string{"GITEXEC_VALUE=isolated"}},
			want: "unset|isolated|0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Output(context.Background(), tt.opts, "environment")
			if err != nil {
				t.Fatalf("Output: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutputFormsAndWorkingDirectory(t *testing.T) {
	bin := t.TempDir()
	writeFakeGit(t, bin, `
if [ "$1" = stdin ]; then
  /bin/cat
  exit
fi
printf '  %s  \n' "$PWD"
`)
	t.Setenv("PATH", bin)
	dir := t.TempDir()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	got, err := Output(context.Background(), Options{Dir: dir}, "pwd")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if got != resolvedDir {
		t.Fatalf("Output = %q, want %q", got, resolvedDir)
	}

	raw, err := RawOutput(context.Background(), Options{Dir: dir}, "pwd")
	if err != nil {
		t.Fatalf("RawOutput: %v", err)
	}
	if want := "  " + resolvedDir + "  \n"; string(raw) != want {
		t.Fatalf("RawOutput = %q, want %q", raw, want)
	}

	raw, err = RawOutput(context.Background(), Options{Stdin: strings.NewReader("patch\nbytes\n")}, "stdin")
	if err != nil {
		t.Fatalf("RawOutput with stdin: %v", err)
	}
	if string(raw) != "patch\nbytes\n" {
		t.Fatalf("stdin output = %q", raw)
	}
}

func TestNonZeroExitPreservesOutputAndCode(t *testing.T) {
	bin := t.TempDir()
	writeFakeGit(t, bin, `
printf 'stdout detail\n'
printf 'stderr detail\n' >&2
exit 7
`)
	t.Setenv("PATH", bin)

	out, err := CombinedOutput(context.Background(), Options{}, "failing", "argument")
	if err == nil {
		t.Fatal("CombinedOutput unexpectedly succeeded")
	}
	if got := string(out); !strings.Contains(got, "stdout detail") || !strings.Contains(got, "stderr detail") {
		t.Fatalf("combined output = %q", got)
	}
	if got := err.Error(); !strings.Contains(got, "git failing argument") || !strings.Contains(got, "stdout detail") || !strings.Contains(got, "stderr detail") {
		t.Fatalf("error = %q", got)
	}
	if code, ok := ExitCode(err); !ok || code != 7 {
		t.Fatalf("ExitCode = (%d, %v), want (7, true)", code, ok)
	}
	if _, ok := ExitCode(errors.New("not an exit error")); ok {
		t.Fatal("ExitCode reported a code for an unrelated error")
	}
}

func TestOutputFailureIncludesStderr(t *testing.T) {
	bin := t.TempDir()
	writeFakeGit(t, bin, `
printf 'specific stderr\n' >&2
exit 3
`)
	t.Setenv("PATH", bin)

	_, err := Output(context.Background(), Options{}, "bad-output")
	if err == nil || !strings.Contains(err.Error(), "specific stderr") {
		t.Fatalf("Output error = %v, want captured stderr", err)
	}
}

func TestExecutableResolutionSkipsDanglingShim(t *testing.T) {
	badBin := t.TempDir()
	goodBin := t.TempDir()
	if err := os.Symlink(filepath.Join(badBin, "missing-git"), filepath.Join(badBin, "git")); err != nil {
		t.Fatalf("create dangling git shim: %v", err)
	}
	writeFakeGit(t, goodBin, `printf 'usable\n'`)
	t.Setenv("PATH", badBin+string(os.PathListSeparator)+goodBin)

	got, err := Output(context.Background(), Options{}, "version")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if got != "usable" {
		t.Fatalf("Output = %q, want usable", got)
	}
}

func TestExecutableResolutionSkipsInaccessibleFile(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("effective-user executable checks are supported on darwin and Linux")
	}
	badBin := t.TempDir()
	goodBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(badBin, "git"), []byte("not executable\n"), 0o010); err != nil {
		t.Fatalf("write inaccessible git: %v", err)
	}
	writeFakeGit(t, goodBin, `printf 'usable\n'`)
	t.Setenv("PATH", badBin+string(os.PathListSeparator)+goodBin)

	got, err := Output(context.Background(), Options{}, "version")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if got != "usable" {
		t.Fatalf("Output = %q, want usable", got)
	}
}

func TestCancellationKillsProcessGroup(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-group cancellation is supported on darwin and Linux")
	}
	bin := t.TempDir()
	writeFakeGit(t, bin, `
/bin/sleep 30 &
child=$!
printf '%s\n' "$child" > "$GITEXEC_PID_FILE"
wait "$child"
`)
	t.Setenv("PATH", bin)
	pidFile := filepath.Join(t.TempDir(), "child.pid")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{ExtraEnv: []string{"GITEXEC_PID_FILE=" + pidFile}}, "wait")
	}()

	pid := waitForChildPID(t, pidFile)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run unexpectedly succeeded after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Git process did not stop after cancellation")
	}

	deadline := time.Now().Add(3 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("Git helper process %d survived context cancellation", pid)
	}
}

func writeFakeGit(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); scanErr == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for child pid in %s", path)
	return 0
}
