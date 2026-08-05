package provider

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxResetHint bounds a parsed reset instant. Beyond it the parse is treated
// as wrong and rejected outright rather than capped: capping would turn a
// misparse into the worst possible park, while rejecting falls back to the
// configured cooldown, which is never worse than today's behavior.
const maxResetHint = 7 * 24 * time.Hour

// minResetHint floors a parsed instant. A message classified moments before
// its own reset yields a sub-second park, which buys one guaranteed-doomed
// dispatch — the exact cost this parsing exists to remove.
const minResetHint = 30 * time.Second

// staleYearlessCutoff decides when a yearless date that already passed means
// "next year" rather than "just now". "resets Jan 3" read in December is next
// January; "resets Jul 1 at 5pm" read at 17:00:30 is a limit that has already
// reset, not one eleven months out.
const staleYearlessCutoff = 30 * 24 * time.Hour

// nowFunc is the clock reset-hint parsing resolves relative dates against.
// Overridden in tests; the classifiers are otherwise pure functions with no
// clock of their own.
var nowFunc = time.Now

var (
	// "try again at Aug 8th, 2026 9:41 AM" — the codex usage-limit message.
	// The year is \b-anchored and the time is trailed by \b so a malformed
	// number ("20261") cannot have a two-digit prefix reinterpreted as the
	// hour; the optional "at" covers the "<date> at <time>" phrasing.
	codexResetRe = regexp.MustCompile(`(?i)try again at\s+([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s*(?:(\d{4})\b)?[,\s]*(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\b`)
	// "resets Jul 1 at 5pm" / "resets Jul 1 at 17:00" — the claude message.
	claudeResetRe = regexp.MustCompile(`(?i)resets?\s+(?:on\s+)?([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\b`)
)

var monthByPrefix = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// HintOutcome distinguishes "the provider stated a time" from "nothing usable
// was there" and from "something parsed but was out of range". The last case
// must be visible: a rejected hint and an absent one produce identical parks,
// so without this an over-long or nonsensical provider message is silently
// indistinguishable from a message that never carried a date.
type HintOutcome int

const (
	HintNone HintOutcome = iota
	HintParsed
	HintRejected
)

// ansiEscape matches SGR colour sequences, which would otherwise sit between
// "try again at" and the month name and defeat the patterns.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// parseResetHint extracts a provider-supplied reset instant from text and
// returns how long from now that is.
//
// Providers state exactly when they will serve again — codex prints "try again
// at Aug 8th, 2026 9:41 AM" — and without this the fixed cooldown retries every
// 15 minutes for the three days until that instant, each retry burning a
// dispatch.
//
// The outcome is HintParsed only when a concrete, in-range, future instant was
// found. Anything else falls back to the caller's configured cooldown, so this
// can only shorten the gap between a park and the truth, never lengthen it
// past what the provider itself stated.
//
// Times are read in the local zone: providers print them in the account's zone
// with no offset. A zone mismatch skews the park by hours, and because an
// over-long parse is rejected rather than capped, the skew cannot compound
// into a multi-day park.
func parseResetHint(text string) (time.Duration, HintOutcome) {
	if text == "" {
		return 0, HintNone
	}
	text = ansiEscape.ReplaceAllString(text, "")
	now := nowFunc()

	// The EARLIEST valid instant, not the first match. All matches are scanned
	// because agent prose and tool output can carry a date-shaped fragment
	// that would otherwise shadow the real hint — but a decoy that parses
	// cleanly and sits further out would then over-park the provider, so the
	// most conservative candidate wins.
	var best time.Duration
	found := false
	rejected := false
	for _, re := range []*regexp.Regexp{codexResetRe, claudeResetRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			when, ok := resetInstant(re, m, now)
			if !ok {
				continue
			}
			d := when.Sub(now)
			if d <= 0 {
				continue
			}
			if d > maxResetHint {
				rejected = true
				continue
			}
			if !found || d < best {
				best, found = d, true
			}
		}
	}
	if found {
		return max(best, minResetHint), HintParsed
	}
	if rejected {
		return 0, HintRejected
	}
	return 0, HintNone
}

// resetInstant assembles the matched fields into a concrete instant. The two
// expressions differ in whether a year group is present, so the year and time
// groups are read positionally per pattern rather than by a shared index.
func resetInstant(re *regexp.Regexp, m []string, now time.Time) (time.Time, bool) {
	name := strings.ToLower(m[1])
	month, ok := monthByPrefix[name[:3]]
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
		if hour > 12 {
			return time.Time{}, false
		}
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour > 12 {
			return time.Time{}, false
		}
		if hour == 12 {
			hour = 0
		}
	}

	if year == 0 {
		year = now.Year()
		candidate := time.Date(year, month, day, hour, minute, 0, 0, now.Location())
		// Only a date far in the past means "next year". A few seconds or
		// hours past means the limit has already reset, and rolling it
		// forward would park the provider for a year.
		if now.Sub(candidate) > staleYearlessCutoff {
			year++
		}
	}
	return time.Date(year, month, day, hour, minute, 0, 0, now.Location()), true
}
