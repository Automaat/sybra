// Package textutil holds the one rune-safe truncation implementation.
//
// Every cut lands on a UTF-8 rune boundary. Slicing raw bytes instead splits
// multibyte runes in agent output — `…`, `→`, box-drawing borders, emoji are
// ordinary there — and the invalid UTF-8 that produces is not inert: yaml.v3
// re-encodes it as an unreadable `!!binary` block in the task file, and
// encoding/json silently swaps in U+FFFD.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// TruncateBytes keeps at most limit bytes of s and appends suffix when it
// cuts, so the result can exceed limit by len(suffix). Callers that need the
// whole result bounded want TruncateBytesTotal.
func TruncateBytes(s string, limit int, suffix string) string {
	if len(s) <= limit {
		return s
	}
	if limit < 0 {
		limit = 0
	}
	return s[:runeBoundaryAtOrBefore(s, limit)] + suffix
}

// TruncateBytesValid caps s like TruncateBytes and additionally guarantees the
// whole result is valid UTF-8, repairing malformed bytes anywhere in the
// retained text rather than only aligning the cut. Reach for it when the text
// comes from outside the process — tool output, remote stderr, file contents —
// where a bad byte can sit well before the cut point.
func TruncateBytesValid(s string, limit int, suffix string) string {
	return TruncateBytes(strings.ToValidUTF8(s, "\uFFFD"), limit, suffix)
}

// TruncateBytesTotal bounds the whole result — content plus suffix — to limit
// bytes. A limit too small for the suffix yields the suffix alone, itself cut
// on a rune boundary.
func TruncateBytesTotal(s string, limit int, suffix string) string {
	if len(s) <= limit {
		return s
	}
	if limit <= len(suffix) {
		return suffix[:runeBoundaryAtOrBefore(suffix, max(limit, 0))]
	}
	return TruncateBytes(s, limit-len(suffix), suffix)
}

// TruncateRunesTotal bounds the whole result to limit runes. Use it for
// budgets a human reads as a character count; the byte variants are for
// storage and protocol limits.
func TruncateRunesTotal(s string, limit int, suffix string) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	suffixRunes := []rune(suffix)
	if limit <= len(suffixRunes) {
		return string(suffixRunes[:limit])
	}
	return string([]rune(s)[:limit-len(suffixRunes)]) + suffix
}

// TruncateMiddle keeps the head and tail of s and replaces the middle with
// marker, bounding the whole result to limit bytes. Prefer it when the end of
// the text carries the signal — a stack trace's innermost frame, a command's
// exit status — and a head-only cut would drop it.
func TruncateMiddle(s string, limit int, marker string) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	if limit <= len(marker)+2 {
		return s[:runeBoundaryAtOrBefore(s, limit)]
	}
	keep := limit - len(marker)
	head := runeBoundaryAtOrBefore(s, keep/2)
	tail := runeBoundaryAtOrAfter(s, len(s)-(keep-keep/2))
	return s[:head] + marker + s[tail:]
}

// TruncateBytesTrimmed is TruncateBytes with surrounding whitespace stripped
// from the cut content, so a cut landing mid-indentation does not leave the
// suffix dangling off a run of spaces.
func TruncateBytesTrimmed(s string, limit int, suffix string) string {
	if len(s) <= limit {
		return s
	}
	return strings.TrimSpace(TruncateBytesValid(s, limit, "")) + suffix
}

// TrimPartialRunePrefix drops a leading partial rune so s begins on a rune
// boundary. Use it when slicing from the middle of a buffer, where the cut
// point is not under your control.
func TrimPartialRunePrefix(s string) string {
	return s[runeBoundaryAtOrAfter(s, 0):]
}

// TrimPartialRuneSuffix drops a trailing partial rune so s ends on a rune
// boundary. A legitimately-present U+FFFD is kept — only a 1-byte RuneError,
// which can only come from a broken encoding, is trimmed.
func TrimPartialRuneSuffix(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// TailBytes keeps at most the last limit bytes of s and starts at a UTF-8 rune
// boundary. Use it when the end carries the useful signal, such as the final
// lines of a command log.
func TailBytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	return s[runeBoundaryAtOrAfter(s, len(s)-limit):]
}

// runeBoundaryAtOrBefore returns the largest index <= i that starts a rune.
func runeBoundaryAtOrBefore(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeBoundaryAtOrAfter returns the smallest index >= i that starts a rune.
func runeBoundaryAtOrAfter(s string, i int) int {
	if i < 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
