// Package skillsync mirrors Claude Code skill files from a source
// (repository checkout or embedded fs.FS) into one or more destinations
// (the app's skills dir, ~/.claude/skills, ~/.codex/skills). Validates
// YAML frontmatter on each .md file and prunes orphan skill subdirs
// from the destination.
package skillsync

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Options configures a high-level Run. The selector picks between the disk
// source rooted at RepoDir/.claude/skills and the embedded SkillsFS rooted
// at "data/", using go.mod presence in RepoDir as the marker for "this is
// the sybra source repo, prefer disk."
type Options struct {
	// RepoDir is the source repository root. If empty, Run falls back to
	// os.Getwd() so dev mode (binary launched from the repo) still finds
	// .claude/skills/.
	RepoDir string

	// SkillsFS is the optional embedded skill bundle, rooted at "data/".
	// When set and RepoDir lacks a go.mod, Run prefers SkillsFS — protects
	// Docker deployments where cwd resolves to the user home and treating
	// it as the repo would let a rogue ~/.claude/skills/foo.md poison the
	// destination via orphan cleanup.
	SkillsFS fs.FS

	// PrimaryDst is the app's skills directory (cfg.SkillsDir).
	PrimaryDst string

	// SybraHomeDir is config.HomeDir() — used as the destination for the
	// orchestrator CLAUDE.md / AGENTS.md files copied alongside skills.
	// Only used in disk-source mode (no orchestrator file in the embed
	// bundle).
	SybraHomeDir string

	// UserHomeDir is os.UserHomeDir() — used to derive ~/.claude/skills
	// and ~/.codex/skills as additional destinations. Empty disables
	// user-home destinations.
	UserHomeDir string
}

// Syncer copies skill files into one or more destination directories.
// Zero-value Logger is safe (logs are dropped).
type Syncer struct {
	Logger *slog.Logger
}

// Run executes the full skill-sync flow described in Options: choose
// embedded vs disk source, mirror skills into PrimaryDst plus the
// user-home directories, and (in disk mode) copy orchestrator CLAUDE.md
// alongside.
func (s *Syncer) Run(opts Options) {
	repoDir := opts.RepoDir
	if repoDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			s.error("skills.sync.skip", "reason", "no repo_dir and cannot get cwd")
		} else {
			repoDir = cwd
			s.info("skills.sync.fallback_cwd", "dir", cwd)
		}
	}

	useEmbedded := opts.SkillsFS != nil
	if useEmbedded {
		if _, err := os.Stat(filepath.Join(repoDir, "go.mod")); err == nil {
			useEmbedded = false
		}
	}

	dsts := s.destinations(opts.PrimaryDst, opts.UserHomeDir)

	if useEmbedded {
		s.info("skills.sync.embedded", "dst", opts.PrimaryDst)
		for _, dst := range dsts {
			s.SyncFS(opts.SkillsFS, "data", dst)
		}
		s.info("skills.sync.done")
		return
	}

	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	s.info("skills.sync.start", "src", repoDir, "dst", opts.PrimaryDst)
	for _, dst := range dsts {
		if opts.SkillsFS == nil {
			s.SyncDir(skillsSrc, dst)
			continue
		}
		cleanDst := filepath.Clean(dst) + string(filepath.Separator)
		srcNames := s.syncFS(opts.SkillsFS, "data", dst, false)
		for name := range s.syncDir(skillsSrc, dst, false) {
			srcNames[name] = struct{}{}
		}
		s.removeOrphans(dst, cleanDst, srcNames)
	}

	if opts.SybraHomeDir != "" {
		claudeSrc := filepath.Join(repoDir, "orchestrator", "CLAUDE.md")
		s.SyncFile(claudeSrc, filepath.Join(opts.SybraHomeDir, "CLAUDE.md"))
		s.SyncFile(claudeSrc, filepath.Join(opts.SybraHomeDir, "AGENTS.md"))
	}

	s.info("skills.sync.done")
}

// destinations returns PrimaryDst plus ~/.claude/skills and ~/.codex/skills
// when UserHomeDir is set, deduplicating against PrimaryDst (some test
// setups point PrimaryDst at the user's claude dir).
func (s *Syncer) destinations(primary, userHome string) []string {
	dsts := []string{primary}
	if userHome == "" {
		return dsts
	}
	claudeDst := filepath.Join(userHome, ".claude", "skills")
	codexDst := filepath.Join(userHome, ".codex", "skills")
	if filepath.Clean(primary) != filepath.Clean(claudeDst) {
		dsts = append(dsts, claudeDst)
	}
	dsts = append(dsts, codexDst)
	return dsts
}

// SyncFile copies a single file with mkdir-p on the destination directory.
// Missing src is logged at Debug and silently no-ops.
func (s *Syncer) SyncFile(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil {
		s.debug("sync.read.skip", "src", src, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		s.error("sync.mkdir", "dst", dst, "err", err)
		return
	}
	if err := os.WriteFile(dst, data, fs.FileMode(0o644)); err != nil {
		s.error("sync.write", "dst", dst, "err", err)
		return
	}
	s.info("sync.copied", "file", filepath.Base(dst))
}

// SyncDir reads flat .md skill files (and directory-style skills) from src
// and writes each one as dst/<name>/SKILL.md — the subdirectory layout
// Claude Code and Codex both expect. Flat files in dst are NOT discovered
// by Claude Code's skill loader, so this layout is mandatory. Orphan skill
// subdirs (present in dst but absent from src) are removed.
func (s *Syncer) SyncDir(src, dst string) {
	s.syncDir(src, dst, true)
}

func (s *Syncer) syncDir(src, dst string, prune bool) map[string]struct{} {
	entries, err := os.ReadDir(src)
	if err != nil {
		s.debug("sync.skill.skip", "src", src, "reason", err)
		return map[string]struct{}{}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		s.error("sync.skill.mkdir", "dst", dst, "err", err)
		return map[string]struct{}{}
	}
	cleanSrc := filepath.Clean(src) + string(filepath.Separator)
	cleanDst := filepath.Clean(dst) + string(filepath.Separator)

	srcNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if e.IsDir() {
			if s.copySkillDir(src, dst, cleanSrc, cleanDst, e.Name(), "sync.skill") {
				s.info("sync.skill.copied", "skill", e.Name())
				srcNames[e.Name()] = struct{}{}
			}
			continue
		}
		if filepath.Ext(e.Name()) != ".md" {
			continue
		}
		srcPath := filepath.Join(filepath.Clean(src), e.Name())
		if !strings.HasPrefix(srcPath+string(filepath.Separator), cleanSrc) {
			s.warn("sync.skill.skip.traversal", "name", e.Name())
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		skillDir := filepath.Join(filepath.Clean(dst), name)
		if !strings.HasPrefix(skillDir+string(filepath.Separator), cleanDst) {
			s.warn("sync.skill.skip.traversal", "name", e.Name())
			continue
		}
		dstPath := filepath.Join(skillDir, "SKILL.md")
		data, err := os.ReadFile(srcPath)
		if err != nil {
			s.warn("sync.skill.read.fail", "name", e.Name(), "err", err)
			continue
		}
		if !HasYAMLFrontmatter(data) {
			s.warn("sync.skill.skip.invalid", "name", e.Name(), "reason", "missing YAML frontmatter")
			continue
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			s.error("sync.skill.mkdir.skill", "dir", skillDir, "err", err)
			continue
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			s.error("sync.skill.write", "dst", dstPath, "err", err)
			continue
		}
		s.info("sync.skill.copied", "skill", name)
		srcNames[name] = struct{}{}
	}

	if prune {
		s.removeOrphans(dst, cleanDst, srcNames)
	}
	return srcNames
}

// SyncFS mirrors SyncDir but reads source files from an fs.FS. Used for
// the embedded skill bundle (internal/skills.FS).
func (s *Syncer) SyncFS(fsys fs.FS, srcDir, dst string) {
	s.syncFS(fsys, srcDir, dst, true)
}

func (s *Syncer) syncFS(fsys fs.FS, srcDir, dst string, prune bool) map[string]struct{} {
	entries, err := fs.ReadDir(fsys, srcDir)
	if err != nil {
		s.debug("sync.skill.fs.skip", "src", srcDir, "reason", err)
		return map[string]struct{}{}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		s.error("sync.skill.mkdir", "dst", dst, "err", err)
		return map[string]struct{}{}
	}
	cleanDst := filepath.Clean(dst) + string(filepath.Separator)

	srcNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		skillDir := filepath.Join(filepath.Clean(dst), name)
		if !strings.HasPrefix(skillDir+string(filepath.Separator), cleanDst) {
			s.warn("sync.skill.skip.traversal", "name", e.Name())
			continue
		}
		dstPath := filepath.Join(skillDir, "SKILL.md")
		data, err := fs.ReadFile(fsys, srcDir+"/"+e.Name())
		if err != nil {
			s.warn("sync.skill.fs.read.fail", "name", e.Name(), "err", err)
			continue
		}
		if !HasYAMLFrontmatter(data) {
			s.warn("sync.skill.skip.invalid", "name", e.Name(), "reason", "missing YAML frontmatter")
			continue
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			s.error("sync.skill.mkdir.skill", "dir", skillDir, "err", err)
			continue
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			s.error("sync.skill.write", "dst", dstPath, "err", err)
			continue
		}
		s.info("sync.skill.copied", "skill", name)
		srcNames[name] = struct{}{}
	}

	if prune {
		s.removeOrphans(dst, cleanDst, srcNames)
	}
	return srcNames
}

// removeOrphans deletes dst/<name>/ subdirs whose names are absent from
// srcNames, but only when the subdir holds a SKILL.md (so we never touch
// unrelated content the user dropped there).
func (s *Syncer) removeOrphans(dst, cleanDst string, srcNames map[string]struct{}) {
	dstEntries, err := os.ReadDir(dst)
	if err != nil {
		return
	}
	for _, e := range dstEntries {
		if !e.IsDir() {
			continue
		}
		if _, ok := srcNames[e.Name()]; ok {
			continue
		}
		skillMD := filepath.Join(filepath.Clean(dst), e.Name(), "SKILL.md")
		if !strings.HasPrefix(skillMD, cleanDst) {
			continue
		}
		if _, statErr := os.Stat(skillMD); statErr != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(filepath.Clean(dst), e.Name())); err != nil {
			s.warn("sync.skill.orphan.remove.fail", "skill", e.Name(), "err", err)
		} else {
			s.info("sync.skill.orphan.removed", "skill", e.Name())
		}
	}
}

func (s *Syncer) info(msg string, args ...any)  { s.log(slog.LevelInfo, msg, args...) }
func (s *Syncer) debug(msg string, args ...any) { s.log(slog.LevelDebug, msg, args...) }
func (s *Syncer) warn(msg string, args ...any)  { s.log(slog.LevelWarn, msg, args...) }
func (s *Syncer) error(msg string, args ...any) { s.log(slog.LevelError, msg, args...) }

func (s *Syncer) log(level slog.Level, msg string, args ...any) {
	if s.Logger == nil {
		return
	}
	s.Logger.Log(context.Background(), level, msg, args...)
}
