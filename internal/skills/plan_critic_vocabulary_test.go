package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planCriticSources lists every on-disk copy of the plan-critic skill that can
// reach a running agent.
//
// There are two, and the embedded one loses. skillsync writes the embedded
// bundle first, then syncs repoDir/.claude/skills over the top of it whenever
// repoDir/go.mod exists — true for the server's own checkout — so the repo copy
// is what agents actually read. Editing only data/plan-critic.md changes
// nothing at runtime, which is exactly how the council vocabulary below
// survived a prompt change that had supposedly removed it.
func planCriticSources(t *testing.T) map[string]string {
	t.Helper()

	embedded, err := FS.ReadFile("data/plan-critic.md")
	if err != nil {
		t.Fatalf("read embedded plan-critic skill: %v", err)
	}
	repoPath := filepath.Join("..", "..", ".claude", "skills", "plan-critic", "SKILL.md")
	repo, err := os.ReadFile(repoPath)
	if err != nil {
		t.Fatalf("read repo plan-critic skill (%s): %v", repoPath, err)
	}
	return map[string]string{
		"data/plan-critic.md":                 string(embedded),
		".claude/skills/plan-critic/SKILL.md": string(repo),
	}
}

// TestPlanCritic_NoCouncilVocabulary keeps both copies of the skill in step
// with the plan prompt.
//
// The plan step no longer convenes a council, so a critic still told not to
// require "council synthesis" or "persona names" is policing a vocabulary that
// no longer exists — and, worse, is not told to flag the scrutiny answers that
// replaced it. Both copies must agree; asserting only the embedded one would
// pass while the runtime-effective copy drifts.
func TestPlanCritic_NoCouncilVocabulary(t *testing.T) {
	t.Parallel()

	for name, text := range planCriticSources(t) {
		// These are prose files: a phrase can wrap across a line break at any
		// time, so match against whitespace-collapsed text rather than pinning
		// the current wrapping.
		flat := strings.ToLower(strings.Join(strings.Fields(text), " "))
		for _, banned := range []string{"council", "persona names"} {
			if strings.Contains(flat, banned) {
				t.Errorf("%s references %q — the plan step no longer convenes a council", name, banned)
			}
		}
		if !strings.Contains(flat, "scrutiny answers") {
			t.Errorf("%s does not name the scrutiny answers that replaced the council evidence", name)
		}
	}
}
