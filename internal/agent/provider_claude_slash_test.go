package agent

import (
	"slices"
	"testing"
)

func TestClaudeHeadlessDisablesSlashCommands(t *testing.T) {
	t.Parallel()

	for _, steerable := range []bool{false, true} {
		inv, err := claudeProvider{}.BuildHeadlessInvocation(&Agent{}, RunConfig{HeadlessSteerable: steerable})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(inv.args, "--disable-slash-commands") {
			t.Errorf("steerable=%v args %v lack --disable-slash-commands", steerable, inv.args)
		}
	}
}

func TestCodexHeadlessAlwaysIgnoresUserConfig(t *testing.T) {
	t.Parallel()

	inv, err := codexProvider{}.BuildHeadlessInvocation(&Agent{}, RunConfig{Prompt: "Review it"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inv.args, "--ignore-user-config") {
		t.Errorf("args %v lack --ignore-user-config", inv.args)
	}
}
