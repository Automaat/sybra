package task

import (
	"regexp"
	"strings"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a title into a filesystem-safe slug.
// Returns "task" for empty or all-special-character inputs.
func Slugify(title string) string {
	s := strings.ToLower(title)
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	if s == "" {
		return "task"
	}

	const maxLen = 40
	if len(s) <= maxLen {
		return s
	}

	s = s[:maxLen]
	if i := strings.LastIndex(s, "-"); i > 0 {
		s = s[:i]
	}
	return s
}
