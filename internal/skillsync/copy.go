package skillsync

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// copyDirTree recursively copies src into dst. Uses os.Root to confine
// reads to the source subtree, preventing symlink TOCTOU escape.
func copyDirTree(src, dst string) error {
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcRoot.Close() }()

	rootFS := srcRoot.FS()
	return fs.WalkDir(rootFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return os.MkdirAll(dst, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		target := filepath.Join(dst, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(rootFS, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// copySkillDir validates and recursively copies a directory-style skill
// (src/<name>/SKILL.md + siblings) to dst/<name>/. Returns true if the copy
// succeeded. Missing/malformed SKILL.md and traversal/IO errors are logged
// and return false.
func (s *Syncer) copySkillDir(src, dst, cleanSrc, cleanDst, name, logPrefix string) bool {
	srcSkillDir := filepath.Join(filepath.Clean(src), name)
	data, err := os.ReadFile(filepath.Join(srcSkillDir, "SKILL.md"))
	if err != nil {
		return false
	}
	if !HasYAMLFrontmatter(data) {
		s.warn(logPrefix+".skip.invalid", "name", name, "reason", "missing YAML frontmatter")
		return false
	}
	dstSkillDir := filepath.Join(filepath.Clean(dst), name)
	if !strings.HasPrefix(srcSkillDir+string(filepath.Separator), cleanSrc) ||
		!strings.HasPrefix(dstSkillDir+string(filepath.Separator), cleanDst) {
		s.warn(logPrefix+".skip.traversal", "name", name)
		return false
	}
	if err := os.RemoveAll(dstSkillDir); err != nil {
		s.warn(logPrefix+".clean.fail", "name", name, "err", err)
		return false
	}
	if err := copyDirTree(srcSkillDir, dstSkillDir); err != nil {
		s.error(logPrefix+".copy.tree", "name", name, "err", err)
		return false
	}
	return true
}
