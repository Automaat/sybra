package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// rewriteSkillInvocations converts Claude-style `/skill-name` invocations
// into Codex-style `$skill-name` invocations for prompts routed to codex.
// Only exact matches against the given skill names are rewritten, and the
// invocation must be preceded by start-of-line or a non-word, non-`/`
// character, and followed by a non-identifier char or end — so path
// segments like `/tmp/synapse-plan-xxx.md` are never touched.
func rewriteSkillInvocations(prompt string, skillNames []string) string {
	if len(skillNames) == 0 || prompt == "" {
		return prompt
	}
	// Sort descending by length so longer names are tried first (avoids a
	// shorter prefix like "plan" consuming part of "plan-critic").
	sorted := make([]string, len(skillNames))
	copy(sorted, skillNames)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, name := range sorted {
		if name == "" {
			continue
		}
		pattern := `(^|[^\w/])/` + regexp.QuoteMeta(name) + `([^a-z0-9-]|$)`
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		prompt = re.ReplaceAllString(prompt, "${1}$$"+name+"${2}")
	}
	return prompt
}

// discoverCodexSkills returns the list of skill names installed under
// ~/.codex/skills/. A skill name is a direct subdir containing a SKILL.md
// file. Returns nil on any error — the caller treats that as "no rewrite".
func discoverCodexSkills() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return listSkillDirs(filepath.Join(home, ".codex", "skills"))
}

func listSkillDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "SKILL.md")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}
