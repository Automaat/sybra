package deploy_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLeaderDevCommandsStampVCS(t *testing.T) {
	data, err := os.ReadFile("../../mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"go run -buildvcs=true .\"", "go run -buildvcs=true ./cmd/sybra-server\""} {
		if !strings.Contains(string(data), command) {
			t.Errorf("leader startup must preserve running source identity: missing %q", command)
		}
	}
}

// Exercise real go-run build metadata, not a fabricated debug.BuildInfo: the
// deployment regression survived the latter while the healthy leader returned
// 503 from its release endpoint. This fixture never starts an app or provider.
func TestLeaderGoRunCleanAndDirtyRevision(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/leader-build-fixture\n\ngo 1.26.0\n")
	write("main.go", "package main\nimport (\"fmt\"; \"example.com/leader-build-fixture/version\")\nfunc main() { fmt.Print(version.CleanRevision()) }\n")
	if err := os.Mkdir(filepath.Join(dir, "version"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"version.go", "revision.go"} {
		data, err := os.ReadFile(filepath.Join("../../internal/version", name))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("version", name), string(data))
	}
	env := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			env = append(env, value)
		}
	}
	env = append(env, "GOENV=off", "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	run := func(name string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("git", "init", "--quiet")
	run("git", "add", ".")
	run("git", "-c", "user.name=Build Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath="+os.DevNull, "commit", "--quiet", "-m", "fixture")
	sha := run("git", "rev-parse", "HEAD")
	if got := run("go", "run", "-buildvcs=true", "."); got != sha {
		t.Fatalf("clean running revision = %q, want %q", got, sha)
	}
	write("main.go", "package main\nimport (\"fmt\"; \"example.com/leader-build-fixture/version\")\nfunc main() { fmt.Print(version.CleanRevision()) }\n// dirty\n")
	if got := run("go", "run", "-buildvcs=true", "."); got != "" {
		t.Fatalf("dirty running revision = %q, want no authorized release", got)
	}
}
