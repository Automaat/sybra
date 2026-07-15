package skillsync

import "strings"

// HasYAMLFrontmatter reports whether data begins with a YAML frontmatter
// block delimited by ---. Skills without frontmatter are rejected by Codex
// (and unusable by Claude Code), so the syncer skips copying them to avoid
// poisoning the destination.
func HasYAMLFrontmatter(data []byte) bool {
	trimmed := strings.TrimLeft(string(data), " \t\r\n\ufeff")
	return strings.HasPrefix(trimmed, "---\n") || strings.HasPrefix(trimmed, "---\r\n")
}
