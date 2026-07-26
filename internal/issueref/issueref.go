// Package issueref holds the pure primitives for canonicalizing GitHub issue
// references (URL or "owner/repo#n" shorthand). It is a leaf package with no
// internal dependencies so both low-level packages (internal/task) and the
// umbrella DAG can share one normalization rule without an import cycle.
package issueref

import (
	"net/url"
	"strconv"
	"strings"
)

// Normalize canonicalizes an issue URL or shorthand to "owner/repo#number"
// (lowercased) so a DependsOn entry matches a task's Issue field regardless of
// the spelling used to write it. Only a github.com issue/PR URL collapses to
// that form; anything else (shorthand, or a non-github.com host) is lowercased
// and trimmed so "Automaat/sybra#12" still matches a URL-form Issue. The host
// is matched exactly to avoid a substring like "notgithub.com" being read as
// github.com. Empty in, empty out.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && isGitHubHost(u.Host) {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 4 && (parts[2] == "issues" || parts[2] == "pull") {
			if num := leadingDigits(parts[3]); num != "" {
				return strings.ToLower(parts[0]+"/"+parts[1]) + "#" + num
			}
		}
	}
	return strings.ToLower(s)
}

// ParseRef splits an issue ref into "owner/repo" and its number. It accepts a
// github.com issue/PR URL (preserving the repo's original case for API calls)
// or "owner/repo#n" shorthand. ok is false for anything else.
func ParseRef(ref string) (repo string, number int, ok bool) {
	ref = strings.TrimSpace(ref)
	if u, err := url.Parse(ref); err == nil && isGitHubHost(u.Host) {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 4 && (parts[2] == "issues" || parts[2] == "pull") {
			if n, err := strconv.Atoi(leadingDigits(parts[3])); err == nil {
				return parts[0] + "/" + parts[1], n, true
			}
		}
		return "", 0, false
	}
	r, num, found := strings.Cut(ref, "#")
	r = strings.TrimSpace(r)
	if found && strings.Contains(r, "/") {
		if n, err := strconv.Atoi(strings.TrimSpace(num)); err == nil {
			return r, n, true
		}
	}
	return "", 0, false
}

// isGitHubHost reports whether h is github.com (case-insensitive, with or
// without a www prefix). Anchored so a lookalike host cannot cross-link to an
// unrelated github.com issue.
func isGitHubHost(h string) bool {
	h = strings.ToLower(h)
	return h == "github.com" || h == "www.github.com"
}

// leadingDigits returns the run of ASCII digits at the start of s (empty if
// none). Trims trailing URL noise like "#issuecomment-..." or "?foo" off an
// issue number.
func leadingDigits(s string) string {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}
