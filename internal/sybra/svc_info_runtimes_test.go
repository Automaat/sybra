package sybra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectAvailableRuntimes(t *testing.T) {
	dir := t.TempDir()
	writeRuntimeExe(t, filepath.Join(dir, "claude"), "#!/bin/sh\nprintf 'Claude 1.2.3\\n'\n")
	writeRuntimeExe(t, filepath.Join(dir, "opencode"), "#!/bin/sh\nprintf 'OpenCode 0.9.0\\n'\n")
	writeRuntimeExe(t, filepath.Join(dir, "hermes"), "#!/bin/sh\nprintf 'Hermes 2.0.0\\n'\n")
	t.Setenv("PATH", dir)

	got := detectAvailableRuntimes()
	if len(got) != len(knownRuntimeSpecs) {
		t.Fatalf("len(runtimes) = %d, want %d", len(got), len(knownRuntimeSpecs))
	}

	claude := runtimeByID(t, got, "claude")
	if !claude.Installed {
		t.Fatal("claude should be installed")
	}
	if claude.Path != filepath.Join(dir, "claude") {
		t.Fatalf("claude path = %q", claude.Path)
	}
	if claude.Version != "Claude 1.2.3" {
		t.Fatalf("claude version = %q", claude.Version)
	}
	if claude.Error != "" {
		t.Fatalf("claude error = %q, want empty", claude.Error)
	}

	codex := runtimeByID(t, got, "codex")
	if codex.Installed {
		t.Fatal("codex should be missing")
	}
	if codex.Path != "" || codex.Version != "" || codex.Error != "" {
		t.Fatalf("missing codex should be empty metadata, got %#v", codex)
	}

	hermes := runtimeByID(t, got, "hermes")
	if !hermes.InformationalOnly {
		t.Fatal("hermes should be informational-only")
	}
	if hermes.Version != "Hermes 2.0.0" {
		t.Fatalf("hermes version = %q", hermes.Version)
	}
}

func TestRuntimeProbeCommandFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	writeRuntimeExe(t, path, "#!/bin/sh\necho 'fatal: broken login' >&2\nexit 7\n")

	version, probeErr := probeRuntimeVersion(path, []string{"--version"}, 2*time.Second)
	if version != "" {
		t.Fatalf("version = %q, want empty", version)
	}
	if !strings.Contains(probeErr, "fatal: broken login") {
		t.Fatalf("probeErr = %q", probeErr)
	}
	if len(probeErr) > runtimeProbeErrorMax {
		t.Fatalf("probeErr length = %d, want <= %d", len(probeErr), runtimeProbeErrorMax)
	}
}

func TestRuntimeProbeTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	writeRuntimeExe(t, path, "#!/bin/sh\nsleep 1\nprintf 'late\\n'\n")

	version, probeErr := probeRuntimeVersion(path, []string{"--version"}, 50*time.Millisecond)
	if version != "" {
		t.Fatalf("version = %q, want empty", version)
	}
	if !strings.Contains(probeErr, "timed out") {
		t.Fatalf("probeErr = %q", probeErr)
	}
}

func TestInfoServiceGetAvailableRuntimesCachesStartupSnapshot(t *testing.T) {
	svc := &InfoService{
		detectRuntimes: func() []RuntimeInfo {
			return []RuntimeInfo{{ID: "claude", Name: "Claude Code", Installed: true, Version: "Claude 9.9.9"}}
		},
	}

	svc.primeRuntimeSnapshot()
	svc.detectRuntimes = func() []RuntimeInfo {
		return []RuntimeInfo{{ID: "claude", Name: "Claude Code", Installed: false}}
	}

	first := svc.GetAvailableRuntimes()
	second := svc.GetAvailableRuntimes()
	if len(first) != 1 || first[0].Version != "Claude 9.9.9" {
		t.Fatalf("first snapshot = %#v", first)
	}
	if len(second) != 1 || second[0].Version != "Claude 9.9.9" {
		t.Fatalf("second snapshot = %#v", second)
	}

	first[0].Version = "mutated"
	third := svc.GetAvailableRuntimes()
	if third[0].Version != "Claude 9.9.9" {
		t.Fatalf("cached snapshot should be cloned, got %#v", third)
	}
}

func runtimeByID(t *testing.T, runtimes []RuntimeInfo, id string) RuntimeInfo {
	t.Helper()
	for _, runtime := range runtimes {
		if runtime.ID == id {
			return runtime
		}
	}
	t.Fatalf("runtime %q not found in %#v", id, runtimes)
	return RuntimeInfo{}
}

func writeRuntimeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
