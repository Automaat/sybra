package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/skillinvoke"
	"gopkg.in/yaml.v3"
)

// rewriteSkillInvocations converts Claude-style `/skill-name` invocations
// into Codex-style `$skill-name` invocations for prompts routed to codex.
// Only exact matches against the given skill names are rewritten, and the
// invocation must be preceded by start-of-line or a non-word, non-`/`
// character, and followed by a non-identifier char or end — so path
// segments like `/tmp/sybra-plan-xxx.md` are never touched.
func rewriteSkillInvocations(prompt string, skillNames []string) string {
	return skillinvoke.RewriteInvocations(prompt, skillNames)
}

// discoverCodexSkills returns the union of skill names sybra knows about for
// the codex skill rewriter. A "known" skill is one whose name shows up in
// any of: ~/.codex/skills/, ~/.claude/skills/, `codex plugin list --json`,
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
	now := time.Now()
	codexSkillsCacheMu.Lock()
	if home == codexSkillsCacheHome && now.Before(codexSkillsCacheExpires) {
		out := cloneSkillNames(codexSkillsCacheNames)
		codexSkillsCacheMu.Unlock()
		return out
	}
	codexSkillsCacheMu.Unlock()

	names := discoverCodexSkillsInHome(home)

	codexSkillsCacheMu.Lock()
	codexSkillsCacheHome = home
	codexSkillsCacheNames = cloneSkillNames(names)
	codexSkillsCacheExpires = now.Add(codexSkillsCacheTTL)
	codexSkillsCacheMu.Unlock()
	return names
}

const codexSkillsCacheTTL = 30 * time.Second

var (
	codexSkillsCacheMu      sync.Mutex
	codexSkillsCacheHome    string
	codexSkillsCacheNames   []string
	codexSkillsCacheExpires time.Time
)

func cloneSkillNames(names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, len(names))
	copy(out, names)
	return out
}

func discoverCodexSkillsInHome(home string) []string {
	seen := make(map[string]struct{}, 64)
	for _, name := range listSkillDirs(filepath.Join(home, ".codex", "skills")) {
		seen[name] = struct{}{}
	}
	for _, name := range listSkillDirs(filepath.Join(home, ".claude", "skills")) {
		seen[name] = struct{}{}
	}
	codexPluginSkills, fallbackPlugins, fallbackAll := listCodexPluginListSkills()
	switch {
	case fallbackAll:
		codexPluginSkills = append(codexPluginSkills, listPluginSkillDirs(filepath.Join(home, ".codex", "plugins", "cache"))...)
	case len(fallbackPlugins) > 0:
		codexPluginSkills = append(codexPluginSkills, listPluginSkillDirsFiltered(filepath.Join(home, ".codex", "plugins", "cache"), fallbackPlugins)...)
	}
	for _, name := range codexPluginSkills {
		seen[name] = struct{}{}
	}
	for _, name := range listPluginSkillDirs(filepath.Join(home, ".claude", "plugins", "cache")) {
		seen[name] = struct{}{}
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

var (
	codexPluginListRunnerMu sync.RWMutex
	runCodexPluginListJSON  = func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "codex", "plugin", "list", "--json").Output()
	}
)

func runCodexPluginListJSONSafe() ([]byte, error) {
	codexPluginListRunnerMu.RLock()
	runner := runCodexPluginListJSON
	codexPluginListRunnerMu.RUnlock()
	return runner()
}

type codexPluginList struct {
	Installed []codexPlugin `json:"installed"`
}

type codexPlugin struct {
	Name            string `json:"name"`
	MarketplaceName string `json:"marketplaceName"`
	Enabled         *bool  `json:"enabled"`
	Source          struct {
		Path string `json:"path"`
	} `json:"source"`
}

type codexPluginManifest struct {
	Skills json.RawMessage `json:"skills"`
}

func listCodexPluginListSkills() (names, fallbackPlugins []string, fallbackAll bool) {
	out, err := runCodexPluginListJSONSafe()
	if err != nil && len(out) == 0 {
		return nil, nil, true
	}
	names, fallbackPlugins, err = parseCodexPluginListSkills(out)
	if err != nil {
		return nil, nil, true
	}
	return names, fallbackPlugins, false
}

func parseCodexPluginListSkills(data []byte) (names, fallbackPlugins []string, err error) {
	var plugins codexPluginList
	if err := json.Unmarshal(data, &plugins); err != nil {
		return nil, nil, err
	}
	seen := map[string]struct{}{}
	for _, plugin := range plugins.Installed {
		if plugin.Enabled != nil && !*plugin.Enabled {
			continue
		}
		if plugin.Source.Path == "" {
			fallbackPlugins = append(fallbackPlugins, codexPluginCacheKey(plugin))
			continue
		}
		names, ok := listCodexPluginSourceSkills(plugin.Source.Path)
		for _, name := range names {
			seen[name] = struct{}{}
		}
		if !ok {
			fallbackPlugins = append(fallbackPlugins, codexPluginCacheKey(plugin))
			continue
		}
	}
	if len(seen) == 0 {
		return []string{}, fallbackPlugins, nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out, fallbackPlugins, nil
}

func codexPluginCacheKey(plugin codexPlugin) string {
	if plugin.Name == "" {
		return ""
	}
	if plugin.MarketplaceName == "" {
		return plugin.Name
	}
	return plugin.MarketplaceName + "/" + plugin.Name
}

func listCodexPluginSourceSkills(pluginRoot string) (names []string, ok bool) {
	if _, err := os.Stat(pluginRoot); err != nil {
		return nil, false
	}
	seen := map[string]struct{}{}
	needsFallback := false
	sawManifest := false
	addSkillNames(seen, filepath.Join(pluginRoot, "skills"))
	for _, manifestPath := range []string{
		filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"),
		filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"),
		filepath.Join(pluginRoot, "plugin.json"),
	} {
		if _, err := os.Stat(manifestPath); err == nil {
			sawManifest = true
		}
		skillPathCandidates, ok := codexPluginManifestSkillPaths(pluginRoot, manifestPath)
		if !ok {
			needsFallback = true
			continue
		}
		for _, candidates := range skillPathCandidates {
			found := 0
			for _, skillPath := range candidates {
				found += addSkillNames(seen, skillPath)
			}
			if len(candidates) > 0 && found == 0 {
				needsFallback = true
			}
		}
	}
	if len(seen) == 0 {
		return nil, sawManifest && !needsFallback
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out, !needsFallback
}

func codexPluginManifestSkillPaths(pluginRoot, manifestPath string) ([][]string, bool) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, os.IsNotExist(err)
	}
	paths, err := parseCodexPluginManifestSkillPaths(data)
	if err != nil {
		return nil, false
	}
	out := make([][]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		candidates := safeCodexManifestSkillPathCandidates(pluginRoot, manifestPath, p)
		if len(candidates) == 0 {
			return nil, false
		}
		out = append(out, candidates)
	}
	return out, true
}

func safeCodexManifestSkillPathCandidates(pluginRoot, manifestPath, skillPath string) []string {
	allowedRoots := []string{
		filepath.Clean(pluginRoot),
	}
	if filepath.Base(filepath.Dir(pluginRoot)) == "plugins" {
		allowedRoots = append(allowedRoots, filepath.Clean(filepath.Dir(filepath.Dir(pluginRoot))))
	}
	if filepath.IsAbs(skillPath) {
		clean := filepath.Clean(skillPath)
		if pathInsideAny(clean, allowedRoots) {
			return []string{clean}
		}
		return nil
	}
	candidates := []string{filepath.Clean(filepath.Join(pluginRoot, skillPath))}
	manifestRelative := filepath.Clean(filepath.Join(filepath.Dir(manifestPath), skillPath))
	if manifestRelative != candidates[0] {
		candidates = append(candidates, manifestRelative)
	}
	out := candidates[:0]
	for _, candidate := range candidates {
		if pathInsideAny(candidate, allowedRoots) {
			out = append(out, candidate)
		}
	}
	return out
}

func pathInsideAny(path string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}

func parseCodexPluginManifestSkillPaths(data []byte) ([]string, error) {
	var manifest codexPluginManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	if len(manifest.Skills) == 0 || string(manifest.Skills) == "null" {
		return nil, nil
	}
	var skillPath string
	if err := json.Unmarshal(manifest.Skills, &skillPath); err == nil {
		if skillPath == "" {
			return nil, nil
		}
		return []string{skillPath}, nil
	}
	var skillPaths []string
	if err := json.Unmarshal(manifest.Skills, &skillPaths); err == nil {
		return skillPaths, nil
	}
	return nil, errors.New("unsupported codex plugin skills value")
}

func addSkillNames(seen map[string]struct{}, root string) int {
	found := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		skillDir := filepath.Dir(path)
		name := skillNameFromSkillMD(path)
		if !validSkillName(name) {
			name = filepath.Base(skillDir)
		}
		if validSkillName(name) {
			found++
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
			}
		}
		return nil
	})
	return found
}

func validSkillName(name string) bool {
	if name == "" || name == "." || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.ContainsAny(name, "/\\ \t\r\n")
}

type skillFrontmatter struct {
	Name string `yaml:"name"`
}

func skillNameFromSkillMD(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	frontmatter := skillFrontmatterBlock(data)
	if len(frontmatter) == 0 {
		return ""
	}
	var meta skillFrontmatter
	if err := yaml.Unmarshal(frontmatter, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Name)
}

func skillFrontmatterBlock(data []byte) []byte {
	text := strings.TrimLeft(string(data), " \t\r\n\ufeff")
	var rest string
	switch {
	case strings.HasPrefix(text, "---\n"):
		rest = text[len("---\n"):]
	case strings.HasPrefix(text, "---\r\n"):
		rest = text[len("---\r\n"):]
	default:
		return nil
	}
	frontmatter, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil
	}
	return []byte(frontmatter)
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

// listPluginSkillDirs walks the plugin-cache layout used by claude and older
// codex versions: <root>/<marketplace>/<plugin>/<version>/.
// Returns every skill-name with a SKILL.md, deduping across versions.
func listPluginSkillDirs(root string) []string {
	return listPluginSkillDirsFiltered(root, nil)
}

func listPluginSkillDirsFiltered(root string, pluginNames []string) []string {
	marketplaces, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var pluginFilter map[string]struct{}
	if len(pluginNames) > 0 {
		pluginFilter = map[string]struct{}{}
		for _, name := range pluginNames {
			if name == "" {
				pluginFilter = nil
				break
			}
			pluginFilter[name] = struct{}{}
		}
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
			if pluginFilter != nil {
				_, byName := pluginFilter[pl.Name()]
				_, byMarketplaceName := pluginFilter[mp.Name()+"/"+pl.Name()]
				if !byName && !byMarketplaceName {
					continue
				}
			}
			versions, err := os.ReadDir(filepath.Join(root, mp.Name(), pl.Name()))
			if err != nil {
				continue
			}
			for _, ver := range versions {
				if !ver.IsDir() || strings.HasPrefix(ver.Name(), ".") {
					continue
				}
				versionDir := filepath.Join(root, mp.Name(), pl.Name(), ver.Name())
				names, _ := listCodexPluginSourceSkills(versionDir)
				if len(names) == 0 {
					names = listSkillDirs(filepath.Join(versionDir, "skills"))
				}
				for _, name := range names {
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
