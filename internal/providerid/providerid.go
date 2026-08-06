package providerid

import (
	"slices"
	"strings"
)

// The provider ids Sybra can dispatch to. These are for Go comparisons only —
// the wire format is unchanged, so config files, persisted records and test
// fixtures keep using the same bare strings.
const (
	Claude   = "claude"
	Codex    = "codex"
	Copilot  = "copilot"
	OpenCode = "opencode"
)

var all = []string{Claude, Codex, Copilot, OpenCode}

// List returns the provider universe as a comma-separated string for use in
// human-readable validation messages, so those messages stay single-sourced.
func List() string { return strings.Join(all, ", ") }

// All returns the ordered provider universe (failover priority: claude, codex,
// copilot, opencode). Callers must not mutate the result.
func All() []string { return append([]string(nil), all...) }

// IsKnown reports whether name is a provider id Sybra can dispatch to.
func IsKnown(name string) bool { return slices.Contains(all, name) }
