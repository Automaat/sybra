package project

import "testing"

func TestMergeChecksCIPolicyHasExplicitOptOut(t *testing.T) {
	app := &ChecksConfig{CI: &CIConfig{Enabled: true, RequiredChecks: []string{"Tests"}}}
	if got := MergeChecks(nil, app); got == nil || got.CI == nil || !got.CI.Enabled {
		t.Fatal("app-only CI policy lost")
	}
	if got := MergeChecks(&ChecksConfig{}, app); got == nil || got.CI == nil || !got.CI.Enabled {
		t.Fatal("unspecified repo policy cleared app CI")
	}
	if got := MergeChecks(&ChecksConfig{CI: &CIConfig{Enabled: false}}, app); got == nil || got.CI == nil || got.CI.Enabled {
		t.Fatal("explicit trusted opt-out did not override app policy")
	}
	if MergeChecks(nil, nil) != nil {
		t.Fatal("legacy no-policy project changed")
	}
}
