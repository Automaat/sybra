package config

import "testing"

func TestValidateResolvedConfig_AutoUpdateAutoRequiresChecks(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.AutoUpdate.Enabled = true
	cfg.AutoUpdate.Mode = "auto"
	cfg.AutoUpdate.RequiredChecks = nil

	err := ValidateResolvedConfig(cfg)
	if err == nil {
		t.Fatal("ValidateResolvedConfig() err = nil, want error")
	}
	msgs := ValidationMessages(err)
	if len(msgs) != 1 || msgs[0] != "auto_update.required_checks must be non-empty when auto_update.mode=auto" {
		t.Fatalf("messages = %v", msgs)
	}
}
