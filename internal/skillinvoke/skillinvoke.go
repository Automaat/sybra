package skillinvoke

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var validNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// NormalizeName returns the canonical skill invocation name without a leading
// slash. Invalid names return ok=false.
func NormalizeName(name string) (normalized string, ok bool) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "/")
	if name == "" || strings.HasPrefix(name, "$") || strings.Contains(name, "/") {
		return "", false
	}
	if !validNameRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// RewriteInvocations converts known slash skill invocations to Codex's
// dollar-prefixed syntax.
func RewriteInvocations(prompt string, skillNames []string) string {
	return rewriteWithPrefix(prompt, skillNames, "$")
}

// StripInvocations converts known slash skill invocations to a bare skill name.
// Copilot has no invocation prefix — skills are discovered from SKILL.md and
// triggered by name/description — so a leading slash would name a command
// Copilot cannot run; the bare name is the strongest trigger signal.
func StripInvocations(prompt string, skillNames []string) string {
	return rewriteWithPrefix(prompt, skillNames, "")
}

func rewriteWithPrefix(prompt string, skillNames []string, prefix string) string {
	if prompt == "" || len(skillNames) == 0 {
		return prompt
	}
	names := normalizedUniqueNames(skillNames)
	if len(names) == 0 {
		return prompt
	}
	matches := findMatches(prompt, names)
	if len(matches) == 0 {
		return prompt
	}
	var out strings.Builder
	out.Grow(len(prompt))
	last := 0
	for _, m := range matches {
		out.WriteString(prompt[last:m.start])
		out.WriteString(prefix)
		out.WriteString(m.name)
		last = m.end
	}
	out.WriteString(prompt[last:])
	return out.String()
}

// ContainsInvocation reports whether prompt contains a slash invocation for
// the named skill. Path-like text such as /tmp/skill.md is ignored.
func ContainsInvocation(prompt, skillName string) bool {
	normalized, ok := NormalizeName(skillName)
	if !ok || prompt == "" {
		return false
	}
	return len(findMatches(prompt, []string{normalized})) > 0
}

// ApplyAliases rewrites known slash skill invocations to another slash skill
// name. The pass is non-chaining: all matches are found against the original
// prompt before replacements are emitted.
func ApplyAliases(prompt string, aliases map[string]string) string {
	if prompt == "" || len(aliases) == 0 {
		return prompt
	}
	normalized := make(map[string]string, len(aliases))
	names := make([]string, 0, len(aliases))
	for src, dst := range aliases {
		from, ok := NormalizeName(src)
		if !ok {
			continue
		}
		to, ok := NormalizeName(dst)
		if !ok {
			continue
		}
		normalized[from] = to
		names = append(names, from)
	}
	names = normalizedUniqueNames(names)
	if len(names) == 0 {
		return prompt
	}
	matches := findMatches(prompt, names)
	if len(matches) == 0 {
		return prompt
	}
	var out strings.Builder
	out.Grow(len(prompt))
	last := 0
	for _, m := range matches {
		out.WriteString(prompt[last:m.start])
		out.WriteByte('/')
		out.WriteString(normalized[m.name])
		last = m.end
	}
	out.WriteString(prompt[last:])
	return out.String()
}

type invocationMatch struct {
	start int
	end   int
	name  string
}

func normalizedUniqueNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		normalized, ok := NormalizeName(name)
		if !ok {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	slices.SortFunc(out, func(a, b string) int {
		if byLen := cmp.Compare(len(b), len(a)); byLen != 0 {
			return byLen
		}
		return cmp.Compare(a, b)
	})
	return out
}

func findMatches(prompt string, names []string) []invocationMatch {
	var matches []invocationMatch
	for i := 0; i < len(prompt); i++ {
		if prompt[i] != '/' || !validBefore(prompt, i) {
			continue
		}
		afterSlash := i + 1
		for _, name := range names {
			end := afterSlash + len(name)
			if end > len(prompt) || prompt[afterSlash:end] != name || !validAfter(prompt, end) {
				continue
			}
			matches = append(matches, invocationMatch{start: i, end: end, name: name})
			i = end - 1
			break
		}
	}
	return matches
}

func validBefore(s string, idx int) bool {
	if idx == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:idx])
	return !isWord(r) && r != '/'
}

func validAfter(s string, idx int) bool {
	if idx >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[idx:])
	return !isNameRune(r)
}

func isWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isNameRune(r rune) bool {
	return r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
