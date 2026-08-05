package provider

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxResetHint bounds how far out a parsed reset instant may park a provider.
// A misparse would otherwise take it offline effectively forever, and a
// too-long park costs the whole fleet while a too-short one costs one probe.
const maxResetHint = 7 * 24 * time.Hour

// nowFunc is the clock reset-hint parsing resolves relative dates against.
// Overridden in tests; the classifiers are otherwise pure functions with no
// clock of their own.
var nowFunc = time.Now

var (
	// "try again at Aug 8th, 2026 9:41 AM" — the codex usage-limit message.
	codexResetRe = regexp.MustCompile(`(?i)try again at\s+([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s*(\d{4})?[,\s]+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	// "resets Jul 1 at 5pm" / "resets Jul 1 at 17:00" — the claude message.
	claudeResetRe = regexp.MustCompile(`(?i)resets?\s+(?:on\s+)?([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
)

var monthByPrefix = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// parseResetHint extracts a provider-supplied reset instant from text and
// returns how long from now that is.
//
// Providers state exactly when they will serve again — codex prints "try again
// at Aug 8th, 2026 9:41 AM" — and without this the fixed cooldown retries every
// 15 minutes for the three days until that instant, each retry burning a
// dispatch. ok is false when nothing parses or when the instant is already
// past, e.g. a stale message quoted from an earlier failure.
//
// Times are read in the local zone: providers print them in the account's zone
// with no offset. A zone mismatch skews the park by hours at most, never
// unbounded, since maxResetHint still applies.
func parseResetHint(text string) (time.Duration, bool) {
	if text == "" {
		return 0, false
	}
	now := nowFunc()
	for _, re := range []*regexp.Regexp{codexResetRe, claudeResetRe} {
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		when, ok := resetInstant(re, m, now)
		if !ok {
			continue
		}
		d := when.Sub(now)
		if d <= 0 {
			continue
		}
		return min(d, maxResetHint), true
	}
	return 0, false
}

// resetInstant assembles the matched fields into a concrete instant. The two
// expressions differ in whether a year group is present, so the year and time
// groups are read positionally per pattern rather than by a shared index.
func resetInstant(re *regexp.Regexp, m []string, now time.Time) (time.Time, bool) {
	name := strings.ToLower(m[1])
	month, ok := monthByPrefix[name[:min(3, len(name))]]
	if !ok {
		return time.Time{}, false
	}
	day, err := strconv.Atoi(m[2])
	if err != nil || day < 1 || day > 31 {
		return time.Time{}, false
	}

	var year int
	hourIdx := 3
	if re == codexResetRe {
		hourIdx = 4
		if m[3] != "" {
			if year, err = strconv.Atoi(m[3]); err != nil {
				return time.Time{}, false
			}
		}
	}

	hour, err := strconv.Atoi(m[hourIdx])
	if err != nil || hour > 23 {
		return time.Time{}, false
	}
	minute := 0
	if v := m[hourIdx+1]; v != "" {
		if minute, err = strconv.Atoi(v); err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	switch strings.ToLower(m[hourIdx+2]) {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}

	if year == 0 {
		// A yearless "resets Jan 3" read in December means next January, not
		// eleven months ago.
		year = now.Year()
		if time.Date(year, month, day, hour, minute, 0, 0, now.Location()).Before(now) {
			year++
		}
	}
	return time.Date(year, month, day, hour, minute, 0, 0, now.Location()), true
}
