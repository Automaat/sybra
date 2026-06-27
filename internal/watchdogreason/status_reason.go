package watchdogreason

import "strings"

const Prefix = "watchdog:"

func HasPrefix(reason string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), Prefix)
}
