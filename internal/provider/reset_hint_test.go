package provider

import (
	"testing"
	"time"
)

// pinNow freezes the clock reset parsing resolves against, so yearless dates
// and "already past" cases are deterministic rather than depending on when CI
// happens to run.
func pinNow(t *testing.T, when time.Time) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return when }
	t.Cleanup(func() { nowFunc = prev })
}

func TestParseResetHint(t *testing.T) {
	now := time.Date(2026, time.August, 5, 15, 4, 0, 0, time.Local)

	tests := []struct {
		name string
		text string
		want time.Duration
		ok   bool
	}{
		{
			// The exact string codex printed on the deploy host.
			name: "codex usage limit with year",
			text: "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 8th, 2026 9:41 AM.",
			want: time.Date(2026, time.August, 8, 9, 41, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			name: "codex without year resolves to this year",
			text: "try again at Aug 6, 9:41 AM",
			want: time.Date(2026, time.August, 6, 9, 41, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			name: "claude weekly limit",
			text: "You've hit your weekly limit · resets Aug 7 at 5pm",
			want: time.Date(2026, time.August, 7, 17, 0, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			name: "claude 24-hour clock",
			text: "resets Aug 7 at 17:30",
			want: time.Date(2026, time.August, 7, 17, 30, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			name: "midnight am is hour zero",
			text: "resets Aug 6 at 12:00am",
			want: time.Date(2026, time.August, 6, 0, 0, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			name: "noon pm stays hour twelve",
			text: "resets Aug 6 at 12:00pm",
			want: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			// Barely past means the limit has already reset, not that it
			// resets next year. Rolling forward here would park the provider
			// for a week where the fixed cooldown parks it for an hour —
			// strictly worse than doing nothing.
			name: "yearless date barely past falls back",
			text: "resets Aug 5 at 3pm",
			ok:   false,
		},
		{
			// Far past is genuinely next year, but that lands beyond the
			// ceiling and is rejected rather than capped.
			name: "yearless date far past exceeds the ceiling",
			text: "resets Jan 3 at 9am",
			ok:   false,
		},
		{
			// A decoy date in agent prose or tool output must not shadow or
			// corrupt the real hint that follows it.
			name: "earlier date-shaped decoy does not win",
			text: "tool output: git reset origin 1 at 3pm baseline\nYou've hit your weekly limit · resets Aug 7 at 5pm",
			want: time.Date(2026, time.August, 7, 17, 0, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			name: "past decoy does not win",
			text: "note: reset May 1 at 9am was the last incident\nYou've hit your weekly limit · resets Aug 7 at 5pm",
			want: time.Date(2026, time.August, 7, 17, 0, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{
			// A malformed year must not have a two-digit prefix reinterpreted
			// as the hour.
			name: "malformed year does not become an hour",
			text: "try again at Aug 8th, 20261 9:41 AM",
			ok:   false,
		},
		{
			name: "date-at-time phrasing parses",
			text: "try again at Aug 6, 2026 at 9:41 AM",
			want: time.Date(2026, time.August, 6, 9, 41, 0, 0, time.Local).Sub(now),
			ok:   true,
		},
		{"impossible pm hour", "resets Aug 6 at 23pm", 0, false},
		{
			// A stale message quoted from an earlier failure must not park
			// the provider at all.
			name: "explicit past instant is rejected",
			text: "try again at Aug 1st, 2026 9:41 AM",
			ok:   false,
		},
		{"no date at all", "You've hit your weekly limit", 0, false},
		{"empty", "", 0, false},
		{"garbage month", "try again at Foo 8th, 2026 9:41 AM", 0, false},
		{"impossible day", "resets Aug 47 at 5pm", 0, false},
		{"impossible minute", "resets Aug 6 at 9:99am", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pinNow(t, now)
			got, outcome := parseResetHint(tc.text)
			if parsed := outcome == HintParsed; parsed != tc.ok {
				t.Fatalf("parseResetHint(%q) outcome = %v, want parsed=%v (got %v)", tc.text, outcome, tc.ok, got)
			}
			if outcome == HintParsed && got != tc.want {
				t.Errorf("parseResetHint(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// A wrong-century parse must fall back to the configured cooldown, not park
// the provider for the ceiling. Capping a misparse turns it into the worst
// available outcome; rejecting it is never worse than today.
func TestParseResetHint_RejectsFarFutureInstant(t *testing.T) {
	pinNow(t, time.Date(2026, time.August, 5, 15, 4, 0, 0, time.Local))

	got, outcome := parseResetHint("try again at Aug 8th, 2099 9:41 AM")
	if outcome != HintRejected {
		t.Errorf("parseResetHint = (%v, %v), want HintRejected so the caller falls back", got, outcome)
	}
}

// A hint parsed moments before its own instant must not produce a sub-second
// park, which buys exactly one doomed dispatch.
func TestParseResetHint_FloorsImminentInstant(t *testing.T) {
	pinNow(t, time.Date(2026, time.August, 5, 16, 59, 59, 0, time.Local))

	got, outcome := parseResetHint("resets Aug 5 at 5pm")
	if outcome != HintParsed {
		t.Fatalf("parseResetHint outcome = %v, want HintParsed", outcome)
	}
	if got != minResetHint {
		t.Errorf("parseResetHint = %v, want floor of %v", got, minResetHint)
	}
}

// The parsed instant must beat the fixed cooldown, which is the whole point:
// a 15-minute default retried against a three-day window burns a dispatch
// every cycle.
func TestClassifiers_PreferProviderResetOverFixedCooldown(t *testing.T) {
	now := time.Date(2026, time.August, 5, 15, 4, 0, 0, time.Local)
	wantCodex := time.Date(2026, time.August, 8, 9, 41, 0, 0, time.Local).Sub(now)

	t.Run("codex", func(t *testing.T) {
		pinNow(t, now)
		sig, reason, after, _ := ClassifyCodexError(ErrorSample{
			Stderr: "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 8th, 2026 9:41 AM.",
		})
		if sig != SignalRateLimit {
			t.Fatalf("signal = %v, want SignalRateLimit", sig)
		}
		if reason != "rate_limited" {
			t.Errorf("reason = %q, want rate_limited", reason)
		}
		if after != wantCodex {
			t.Errorf("retryAfter = %v, want %v", after, wantCodex)
		}
	})

	t.Run("claude weekly limit overrides the hour default", func(t *testing.T) {
		pinNow(t, now)
		wantClaude := time.Date(2026, time.August, 7, 17, 0, 0, 0, time.Local).Sub(now)
		sig, reason, after, _ := ClassifyClaudeError(ErrorSample{
			Stderr: "You've hit your weekly limit · resets Aug 7 at 5pm",
		})
		if sig != SignalRateLimit || reason != "weekly_limit" {
			t.Fatalf("got (%v, %q), want (SignalRateLimit, weekly_limit)", sig, reason)
		}
		if after == weeklyLimitCooldown {
			t.Fatalf("retryAfter fell back to the fixed %v instead of the parsed instant", weeklyLimitCooldown)
		}
		if after != wantClaude {
			t.Errorf("retryAfter = %v, want %v", after, wantClaude)
		}
	})

	t.Run("no parseable instant keeps the fixed cooldown", func(t *testing.T) {
		pinNow(t, now)
		_, reason, after, _ := ClassifyClaudeError(ErrorSample{
			Stderr: "You've hit your weekly limit",
		})
		if reason != "weekly_limit" || after != weeklyLimitCooldown {
			t.Errorf("got (%q, %v), want (weekly_limit, %v)", reason, after, weeklyLimitCooldown)
		}
	})
}

// A clean, exit-0 run's content is the agent's own prose, which routinely
// discusses rate limits and dates — the text below is close to what an agent
// working on this very file would write. Parsing it let an agent park its own
// provider for days off a successful run.
func TestCleanResultContentNeverSuppliesAResetHint(t *testing.T) {
	now := time.Date(2026, time.August, 5, 15, 4, 0, 0, time.Local)
	pinNow(t, now)

	prose := "Done. I implemented the reset-hint parser for the case where codex says " +
		"you've hit your usage limit and prints: try again at Aug 12th, 2026 9:00 AM."

	_, _, after, src := ClassifyClaudeError(ErrorSample{Content: prose, ContentIsCleanResult: true})
	if src == CooldownFromProvider {
		t.Errorf("clean result content supplied a provider hint (%v); agent prose must never park its own provider", after)
	}
	if after > weeklyLimitCooldown {
		t.Errorf("clean result content produced a %v park, longer than the fixed fallback %v", after, weeklyLimitCooldown)
	}
}

// A decoy that parses cleanly but sits further out must not beat the real
// hint: the ceiling only bounds the damage, it does not prevent an over-park.
func TestEarliestValidInstantWins(t *testing.T) {
	now := time.Date(2026, time.August, 5, 15, 4, 0, 0, time.Local)
	pinNow(t, now)

	text := "INFO: scheduler note: try again at Aug 12th, 2026 9:00 AM if backoff persists\n" +
		"ERROR: You've hit your usage limit. Please try again at Aug 8th, 2026 9:41 AM."
	got, outcome := parseResetHint(text)
	want := time.Date(2026, time.August, 8, 9, 41, 0, 0, time.Local).Sub(now)
	if outcome != HintParsed || got != want {
		t.Errorf("parseResetHint = (%v, %v), want (%v, HintParsed) — the earlier instant", got, outcome, want)
	}
}

// A park whose duration came from a constant must not claim the provider
// stated it, and a rejected hint must be distinguishable from no hint at all.
func TestCooldownSourceIsHonest(t *testing.T) {
	now := time.Date(2026, time.August, 5, 15, 4, 0, 0, time.Local)

	t.Run("constant fallback is not a provider hint", func(t *testing.T) {
		pinNow(t, now)
		_, reason, after, src := ClassifyCodexError(ErrorSample{
			Stderr: "ERROR: You've hit your weekly limit. Please try again later.",
		})
		if reason != "weekly_limit" || after != weeklyLimitCooldown {
			t.Fatalf("got (%q, %v), want the weekly constant", reason, after)
		}
		if src != CooldownFromConfig {
			t.Errorf("source = %q, want %q — nothing was parsed from the provider", src, CooldownFromConfig)
		}
	})

	t.Run("out-of-range hint is distinguishable from no hint", func(t *testing.T) {
		pinNow(t, now)
		_, _, _, src := ClassifyCodexError(ErrorSample{
			Stderr: "ERROR: You've hit your usage limit. Please try again at Dec 25th, 2026 9:41 AM.",
		})
		if src != CooldownHintRejected {
			t.Errorf("source = %q, want %q so an out-of-range parse leaves a trace", src, CooldownHintRejected)
		}
	})

	t.Run("ansi coloured message still parses", func(t *testing.T) {
		pinNow(t, now)
		_, _, after, src := ClassifyCodexError(ErrorSample{
			Stderr: "\x1b[1;31mERROR:\x1b[0m usage limit. Please try again at \x1b[1mAug 8th, 2026 9:41 AM\x1b[0m.",
		})
		want := time.Date(2026, time.August, 8, 9, 41, 0, 0, time.Local).Sub(now)
		if src != CooldownFromProvider || after != want {
			t.Errorf("got (%v, %q), want (%v, %q)", after, src, want, CooldownFromProvider)
		}
	})
}
