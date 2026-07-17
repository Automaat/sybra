package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// restrictedTools is the plan step's real list — the one #2179 narrowed
// specifically so the council could not be reconvened by an unprompted Skill
// call, which is what turned this from a convenience into a containment claim.
var restrictedTools = []string{"Bash", "Read", "Grep", "Glob", "Write"}

// TestHonorsAllowedTools_MatchesTheSpawnedArgv is the invariant: a provider may
// claim to honour allowed_tools only if its argv actually carries the list.
//
// The claim and the argv are what drift apart — a provider gaining a per-tool
// flag without flipping the capability keeps warning about containment it now
// has, and one flipping the capability without wiring the flag reports a fence
// that isn't there. The second direction is the dangerous one.
func TestHonorsAllowedTools_MatchesTheSpawnedArgv(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"claude", "codex", "copilot", "opencode"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prov := providerByName(name)
			if prov == nil {
				t.Skipf("provider %s not registered", name)
			}
			a := &Agent{ID: "a1", Provider: name}
			inv, err := prov.BuildHeadlessInvocation(a, RunConfig{
				Prompt:       "plan it",
				AllowedTools: restrictedTools,
			})
			if err != nil {
				t.Fatalf("BuildHeadlessInvocation: %v", err)
			}
			argv := strings.Join(inv.args, " ")
			carriesList := strings.Contains(argv, strings.Join(restrictedTools, ","))

			if got := prov.HonorsAllowedTools(); got != carriesList {
				t.Errorf("HonorsAllowedTools() = %v but argv carries the list = %v\nargv: %s",
					got, carriesList, argv)
			}
		})
	}
}

// A provider that does not honour the list must not be quietly handed it: the
// same step is enforced on a claude spawn and unenforced on the copilot spawn
// beside it, since ab/cross pick the provider at dispatch.
func TestPrepareRunConfig_WarnsWhenProviderIgnoresAllowedTools(t *testing.T) {
	var buf bytes.Buffer
	m, _ := newTestManagerWithLogger(t, slog.New(slog.NewTextHandler(&buf, nil)))

	for _, tc := range []struct {
		provider  string
		wantWarn  bool
		wantInLog string
	}{
		{provider: "claude", wantWarn: false},
		{provider: "codex", wantWarn: true, wantInLog: "codex"},
		{provider: "copilot", wantWarn: true, wantInLog: "copilot"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			buf.Reset()
			prov := providerByName(tc.provider)
			if prov == nil {
				t.Skipf("provider %s not registered", tc.provider)
			}
			m.warnUnenforceableAllowedTools(RunConfig{
				TaskID:       "t1",
				Name:         "plan:t1",
				AllowedTools: restrictedTools,
			}, prov)

			logged := strings.Contains(buf.String(), "allowed_tools.unenforced")
			if logged != tc.wantWarn {
				t.Errorf("warned = %v, want %v (log: %s)", logged, tc.wantWarn, buf.String())
			}
			if tc.wantWarn && !strings.Contains(buf.String(), tc.wantInLog) {
				t.Errorf("log must name the provider that ignored the list, got: %s", buf.String())
			}
			if tc.wantWarn && !strings.Contains(buf.String(), "Bash,Read,Grep,Glob,Write") {
				t.Errorf("log must name the list that is not enforced, got: %s", buf.String())
			}
		})
	}
}

// No list, nothing to warn about — every step that never declared one must stay
// silent, or the signal drowns.
func TestPrepareRunConfig_SilentWithoutAllowedTools(t *testing.T) {
	var buf bytes.Buffer
	m, _ := newTestManagerWithLogger(t, slog.New(slog.NewTextHandler(&buf, nil)))

	m.warnUnenforceableAllowedTools(RunConfig{TaskID: "t1", Name: "plan:t1"}, providerByName("codex"))

	if strings.Contains(buf.String(), "allowed_tools") {
		t.Errorf("warned with no allowed_tools set: %s", buf.String())
	}
}
