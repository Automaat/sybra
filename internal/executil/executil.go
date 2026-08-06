package executil

import (
	"strings"
)

// EscapeAppleScript escapes a string for safe embedding inside an AppleScript
// double-quoted string literal. It escapes backslashes first, then double quotes.
func EscapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
