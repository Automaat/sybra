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
//   - Jira-shaped keys (2+ letters, dash, digits) tend to be project-specific
//     identifiers; matching short tokens like "A-1" would be noisy, so the
//     lower bound is two letters. Matched only in conventional uppercase form
//     so ordinary lowercase repo/path segments like "repo-2" are not treated
//     as tickets.
//   - GitHub URLs are redacted in their entirety because the path usually
//     reveals owner/repo/branch/SHA. Matched case-insensitively, with an
//     optional "www." prefix. The host must begin with "github." so
//     github.com and GitHub Enterprise hosts (e.g. github.mycorp.com) are
//     covered without redacting unrelated hosts that merely contain "github".
//     A second pattern catches the same shape when percent-encoded (e.g.
//     inside a query string or forwarded proxy log).
//   - Email addresses are redacted to avoid leaking author identity from
//     commit-author lines or @mentions.
var staticPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)https?://(?:www\.)?github\.[a-z0-9.-]+/[^\s)\]]+`),
	regexp.MustCompile(`(?i)https?%3a%2f%2f(?:www(?:\.|%2e))?github(?:\.|%2e)[a-z0-9.\-%]+%2f[^\s)\]]+`),
	regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`),
	regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
}

// Scrub redacts every match of staticPatterns and every occurrence of any
// blocklist literal (case-insensitive) from text. Empty or whitespace-only
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
		var matches int
		out, matches = replaceLiteralOutsidePlaceholders(out, term)
		total += matches
	}
	return out, total
}

func replaceLiteralOutsidePlaceholders(text, term string) (scrubbed string, redactions int) {
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(term))
	var b strings.Builder
	matches := 0
	for len(text) > 0 {
		if strings.HasPrefix(text, Placeholder) {
			b.WriteString(Placeholder)
			text = text[len(Placeholder):]
			continue
		}

		nextPlaceholder := strings.Index(text, Placeholder)
		if nextPlaceholder < 0 {
			nextPlaceholder = len(text)
		}
		segment := text[:nextPlaceholder]
		locs := re.FindAllStringIndex(segment, -1)
		if len(locs) == 0 {
			b.WriteString(segment)
		} else {
			b.WriteString(re.ReplaceAllString(segment, Placeholder))
			matches += len(locs)
		}
		text = text[nextPlaceholder:]
	}
	return b.String(), matches
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
