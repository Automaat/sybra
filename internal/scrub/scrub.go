// Package scrub redacts work-repo identifiers from agent- and detector-
// authored text before it lands in a sybra artifact. It is the regex-floor
// of the Work-Data Confidentiality invariant (see project CLAUDE.md): even
// if a prompt instruction fails, the scrubber prevents known identifiers
// from leaking.
//
// Two layers of patterns:
//
//  1. Dynamic — strings derived from a project record (owner, repo, full
//     project ID, repo URL). These are the strongest signals that the text
//     references a specific work source.
//  2. Static — generic patterns common to corporate workflows: Jira-shaped
//     ticket keys, full github.com URLs (path bodies redacted, the host kept
//     so readers can tell the scrub ran), email addresses.
//
// Output uses a fixed `[redacted]` placeholder so scrubbed text remains
// human-readable. The returned count lets callers log/test that something
// was actually removed.
package scrub

import (
	"regexp"
	"sort"
	"strings"
)

// Placeholder is the substitution used for every redacted match.
const Placeholder = "[redacted]"

// staticPatterns are content-shaped redactions applied regardless of the
// blocklist. They aim for high precision over recall:
//   - Jira-shaped keys (2+ uppercase letters, dash, digits) tend to be
//     project-specific identifiers; matching short tokens like "A-1" would
//     be noisy, so the lower bound is two letters.
//   - github.com URLs are redacted in their entirety because the path
//     usually reveals owner/repo/branch/SHA. The host word survives so the
//     scrub is visible.
//   - Email addresses are redacted to avoid leaking author identity from
//     commit-author lines or @mentions.
var staticPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`),
	regexp.MustCompile(`https?://github\.com/[^\s)\]]+`),
	regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
}

// Scrub redacts every match of staticPatterns and every occurrence of any
// blocklist literal (case-sensitive) from text. Empty or whitespace-only
// entries in blocklist are ignored. Returns the redacted text and the total
// number of substitutions performed.
//
// Static patterns run BEFORE blocklist substitution so URLs and emails are
// redacted as whole tokens. Otherwise blocklist hits inside a URL would
// fragment it (e.g. "https://github.com/[redacted]/repo/pull/9") and leave
// path tails dangling. Blocklist matching then prefers longer strings first
// so that "owner/repo" is redacted as one token rather than leaving "owner"
// or "repo" naked.
func Scrub(text string, blocklist []string) (scrubbed string, redactions int) {
	if text == "" {
		return text, 0
	}
	out := text
	total := 0
	for _, re := range staticPatterns {
		matches := re.FindAllStringIndex(out, -1)
		if len(matches) == 0 {
			continue
		}
		out = re.ReplaceAllString(out, Placeholder)
		total += len(matches)
	}

	cleaned := dedupNonEmpty(blocklist)
	sort.Slice(cleaned, func(i, j int) bool {
		return len(cleaned[i]) > len(cleaned[j])
	})
	for _, term := range cleaned {
		count := strings.Count(out, term)
		if count == 0 {
			continue
		}
		out = strings.ReplaceAll(out, term, Placeholder)
		total += count
	}
	return out, total
}

// dedupNonEmpty trims whitespace, drops empty entries, and removes
// duplicates while preserving the first occurrence's spelling.
func dedupNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
