package completion

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/agent"
)

// Every completed run's final result is truncated into AgentRun.Result, which
// internal/task persists as YAML frontmatter (`yaml:"result,omitempty"`). This
// is the highest-volume raw-agent-output-to-YAML path in the system, so a cut
// landing inside a multibyte rune turns the run record into an unreadable
// !!binary base64 block rather than corrupting one status reason.
func TestBuildRunPatchResultSurvivesMultibyteAgentOutput(t *testing.T) {
	for name, result := range map[string]string{
		"ellipsis run":       strings.Repeat("…", 1000),
		"arrow run":          strings.Repeat("→", 1000),
		"emoji run":          strings.Repeat("🚀", 800),
		"box-drawing border": strings.Repeat("│─", 1000),
	} {
		t.Run(name, func(t *testing.T) {
			patch := (&Handler{}).buildRunPatch(&agent.Agent{}, agent.StateStopped, 0, 0, result, nil)

			if patch.Result == nil {
				t.Fatal("Result not set on the run patch")
			}
			if !utf8.ValidString(*patch.Result) {
				t.Fatalf("run result is not valid UTF-8: %q", *patch.Result)
			}
			if !strings.HasSuffix(*patch.Result, "\n... (truncated)") {
				t.Fatalf("run result = %q, want the truncation marker", *patch.Result)
			}

			data, err := yaml.Marshal(map[string]string{"result": *patch.Result})
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}
			if strings.Contains(string(data), "!!binary") {
				t.Fatalf("run result marshalled as a binary block:\n%s", data)
			}
		})
	}
}
