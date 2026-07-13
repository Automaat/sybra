//go:build !linux

package agent

import (
	"context"
	"log/slog"
	"testing"
)

func TestParseLsofCWDs(t *testing.T) {
	t.Parallel()

	got := parseLsofCWDs("p123\nfcwd\nn/tmp/one\nftxt\nn/usr/bin/claude\np456\nfcwd\nn/tmp/two with spaces\n")
	if got[123] != "/tmp/one" {
		t.Fatalf("pid 123 cwd = %q, want /tmp/one", got[123])
	}
	if got[456] != "/tmp/two with spaces" {
		t.Fatalf("pid 456 cwd = %q, want /tmp/two with spaces", got[456])
	}
}

func TestParseLsofCWDsSkipsMalformedPID(t *testing.T) {
	t.Parallel()

	got := parseLsofCWDs("pbad\nfcwd\nn/tmp/bad\np789\nftxt\nn/usr/bin/codex\n")
	if len(got) != 0 {
		t.Fatalf("cwds = %#v, want empty", got)
	}
}

func TestReapOrphanProviderProcessesCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})

	if got := m.ReapOrphanProviderProcesses(ctx, []string{t.TempDir()}); got != 0 {
		t.Fatalf("reaped = %d, want 0 for canceled scan", got)
	}
}
