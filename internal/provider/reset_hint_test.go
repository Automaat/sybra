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
			// A yearless date already past this year rolls forward rather
			// than producing a negative duration that would be discarded —
			// and the clamp then caps the resulting months-long park.
			name: "yearless date already past rolls forward then clamps",
			text: "resets Jan 3 at 9am",
			want: maxResetHint,
			ok:   true,
		},
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
			got, ok := parseResetHint(tc.text)
			if ok != tc.ok {
				t.Fatalf("parseResetHint(%q) ok = %v, want %v (got %v)", tc.text, ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("parseResetHint(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// A wrong-century or otherwise absurd parse must not take the provider offline
// for years.
func TestParseResetHint_ClampsFarFutureInstant(t *testing.T) {
	now := time.Date(2026, time.August, 5, 15, 4, 0, 0, time.Local)
	pinNow(t, now)

	got, ok := parseResetHint("try again at Aug 8th, 2099 9:41 AM")
	if !ok {
		t.Fatal("parseResetHint returned ok=false for a far-future instant, want clamped")
	}
	if got != maxResetHint {
		t.Errorf("parseResetHint = %v, want clamp to %v", got, maxResetHint)
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
		sig, reason, after := ClassifyCodexError(ErrorSample{
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
		sig, reason, after := ClassifyClaudeError(ErrorSample{
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
		_, reason, after := ClassifyClaudeError(ErrorSample{
			Stderr: "You've hit your weekly limit",
		})
		if reason != "weekly_limit" || after != weeklyLimitCooldown {
			t.Errorf("got (%q, %v), want (weekly_limit, %v)", reason, after, weeklyLimitCooldown)
		}
	})
}
