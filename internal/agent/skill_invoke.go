package agent

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// rewriteSkillInvocations converts Claude-style `/skill-name` invocations
// into Codex-style `$skill-name` invocations for prompts routed to codex.
// Only exact matches against the given skill names are rewritten, and the
// invocation must be preceded by start-of-line or a non-word, non-`/`
// character, and followed by a non-identifier char or end — so path
// segments like `/tmp/sybra-plan-xxx.md` are never touched.
func rewriteSkillInvocations(prompt string, skillNames []string) string {
	if len(skillNames) == 0 || prompt == "" {
		return prompt
	}
	// Sort descending by length so longer names are tried first (avoids a
	// shorter prefix like "plan" consuming part of "plan-critic").
	sorted := make([]string, len(skillNames))
	copy(sorted, skillNames)
	slices.SortFunc(sorted, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
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

// discoverCodexSkills returns the union of skill names sybra knows about for
// the codex skill rewriter. A "known" skill is one whose name shows up in
// any of: ~/.codex/skills/, ~/.claude/skills/, ~/.codex/plugins/cache/*/*/*/skills/,
// or ~/.claude/plugins/cache/*/*/*/skills/. Pulling from both providers and
// their plugin caches matters because /staff-code-review can be installed
// for claude via the sai plugin marketplace but absent from ~/.codex/skills/
// — without that name in the set, the rewriter leaves /staff-code-review in
// the prompt and codex tries to exec it as a shell path.
//
// Returns a deduped, deterministically-ordered slice. nil on any home-dir
// error — caller treats that as "no rewrite".
func discoverCodexSkills() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, 64)
	for _, name := range listSkillDirs(filepath.Join(home, ".codex", "skills")) {
		seen[name] = struct{}{}
	}
	for _, name := range listSkillDirs(filepath.Join(home, ".claude", "skills")) {
		seen[name] = struct{}{}
	}
	for _, root := range []string{
		filepath.Join(home, ".codex", "plugins", "cache"),
		filepath.Join(home, ".claude", "plugins", "cache"),
	} {
		for _, name := range listPluginSkillDirs(root) {
			seen[name] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
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

// listPluginSkillDirs walks the plugin-cache layout used by both claude and
// codex: <root>/<marketplace>/<plugin>/<version>/skills/<skill-name>/SKILL.md.
// Returns every skill-name with a SKILL.md, deduping across versions.
func listPluginSkillDirs(root string) []string {
	marketplaces, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, mp := range marketplaces {
		if !mp.IsDir() || strings.HasPrefix(mp.Name(), ".") {
			continue
		}
		plugins, err := os.ReadDir(filepath.Join(root, mp.Name()))
		if err != nil {
			continue
		}
		for _, pl := range plugins {
			if !pl.IsDir() || strings.HasPrefix(pl.Name(), ".") {
				continue
			}
			versions, err := os.ReadDir(filepath.Join(root, mp.Name(), pl.Name()))
			if err != nil {
				continue
			}
			for _, ver := range versions {
				if !ver.IsDir() || strings.HasPrefix(ver.Name(), ".") {
					continue
				}
				skillsDir := filepath.Join(root, mp.Name(), pl.Name(), ver.Name(), "skills")
				for _, name := range listSkillDirs(skillsDir) {
					seen[name] = struct{}{}
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out
}
