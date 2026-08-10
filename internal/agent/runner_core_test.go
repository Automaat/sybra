package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/providerid"
)

func TestCanonicalProviderCommand_EnforceResolvesProviderLauncherOnly(t *testing.T) {
	installDir := t.TempDir()
	target := filepath.Join(installDir, providerid.Codex)
	if err := os.WriteFile(target, []byte("provider"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	launcher := filepath.Join(binDir, providerid.Codex)
	if err := os.Symlink(target, launcher); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	cfg := &RunConfig{
		provider: providerByName(providerid.Codex),
		sandbox:  sandboxSpec{mode: "enforce"},
	}
	resolved, err := canonicalizeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalProviderCommand(providerid.Codex, cfg); got != resolved {
		t.Fatalf("canonicalProviderCommand() = %q, want %q", got, resolved)
	}

	cfg.DisableVerifierControl = true
	if got := canonicalProviderCommand(providerid.Codex, cfg); got != providerid.Codex {
		t.Fatalf("deterministic command was rewritten to %q", got)
	}
	cfg.DisableVerifierControl = false
	cfg.sandbox.mode = "off"
	if got := canonicalProviderCommand(providerid.Codex, cfg); got != providerid.Codex {
		t.Fatalf("off-mode provider was rewritten to %q", got)
	}
}
