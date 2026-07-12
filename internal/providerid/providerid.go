package providerid

import "slices"

var all = []string{"claude", "codex", "copilot"}

// All returns the ordered provider universe (failover priority: claude, codex,
// copilot). Adding a fourth provider means appending it here plus registering
// its agent.Provider impl and config entry. Callers must not mutate the result.
func All() []string { return append([]string(nil), all...) }

// IsKnown reports whether name is a provider id Sybra can dispatch to.
func IsKnown(name string) bool { return slices.Contains(all, name) }
