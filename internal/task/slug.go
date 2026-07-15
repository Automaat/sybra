package task

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// validSlugRe matches slugs in the exact alphabet that Slugify produces:
// lowercase letters/digits, interior hyphens only, no leading/trailing hyphens.
var validSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateSlug rejects slugs that would corrupt worktree paths or git branch
// refs. Accepts the same alphabet that Slugify produces: lowercase letters,
// digits, and interior hyphens; max 40 chars.
func ValidateSlug(s string) error {
	if s == "" {
		return errors.New("slug must not be empty")
	}
	if len(s) > 40 {
		return fmt.Errorf("slug %q exceeds 40 chars", s)
	}
	if !validSlugRe.MatchString(s) {
		return fmt.Errorf("invalid slug %q: use only lowercase letters, digits, and interior hyphens", s)
	}
	return nil
}

var validIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ValidateID rejects task IDs that could traverse or corrupt the tasks
// directory when written as "<id>.md". This matters for externally-supplied
// IDs — notably a leader-pushed task in cluster mode (TaskService.AssignTask) —
// since Store.Create's own IDs are always safe UUID prefixes.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("task id must not be empty")
	}
	if len(id) > 64 {
		return fmt.Errorf("task id %q exceeds 64 chars", id)
	}
	if !validIDRe.MatchString(id) {
		return fmt.Errorf("invalid task id %q: use only letters, digits, hyphens, and underscores", id)
	}
	return nil
}

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
