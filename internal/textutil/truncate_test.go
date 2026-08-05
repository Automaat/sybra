package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// multibyte spells out the runes that actually appear in agent output and
// caused the corruption: ellipsis, arrow, box-drawing border, emoji.
const multibyte = "…→│🚀"

func TestTruncateBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		s      string
		limit  int
		suffix string
		want   string
	}{
		{name: "under limit is untouched", s: "abc", limit: 10, suffix: "…", want: "abc"},
		{name: "exactly at limit is untouched", s: "abc", limit: 3, suffix: "…", want: "abc"},
		{name: "ascii cut appends suffix", s: "abcdef", limit: 3, suffix: "...", want: "abc..."},
		{name: "cut inside a rune backs up", s: "a" + multibyte, limit: 2, suffix: "!", want: "a!"},
		{name: "cut on a rune boundary keeps the rune", s: "a" + multibyte, limit: 4, suffix: "!", want: "a…!"},
		{name: "zero limit yields the suffix alone", s: "abc", limit: 0, suffix: "...", want: "..."},
		{name: "negative limit yields the suffix alone", s: "abc", limit: -5, suffix: "...", want: "..."},
		{name: "empty input is untouched", s: "", limit: 5, suffix: "...", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := TruncateBytes(tt.s, tt.limit, tt.suffix); got != tt.want {
				t.Errorf("TruncateBytes(%q, %d, %q) = %q, want %q", tt.s, tt.limit, tt.suffix, got, tt.want)
			}
		})
	}
}

func TestTruncateBytesTotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		s      string
		limit  int
		suffix string
		want   string
	}{
		{name: "under limit is untouched", s: "abc", limit: 10, suffix: "...", want: "abc"},
		{name: "result fits the limit", s: "abcdefghij", limit: 6, suffix: "...", want: "abc..."},
		{name: "cut inside a rune backs up", s: "ab" + multibyte, limit: 6, suffix: "...", want: "ab..."},
		{name: "limit equal to the suffix drops content", s: "abcdef", limit: 3, suffix: "...", want: "..."},
		{name: "limit below the suffix cuts the suffix", s: "abcdef", limit: 2, suffix: "...", want: ".."},
		{name: "limit splitting a multibyte suffix yields nothing", s: "abcdef", limit: 2, suffix: "…", want: ""},
		{name: "zero limit yields nothing", s: "abc", limit: 0, suffix: "...", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateBytesTotal(tt.s, tt.limit, tt.suffix)
			if got != tt.want {
				t.Errorf("TruncateBytesTotal(%q, %d, %q) = %q, want %q", tt.s, tt.limit, tt.suffix, got, tt.want)
			}
			if len(got) > max(tt.limit, 0) && len(tt.s) > tt.limit {
				t.Errorf("result %q is %d bytes, over the %d limit", got, len(got), tt.limit)
			}
		})
	}
}

func TestTruncateRunesTotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		s      string
		limit  int
		suffix string
		want   string
	}{
		{name: "under limit is untouched", s: multibyte, limit: 10, suffix: "...", want: multibyte},
		{name: "counts runes not bytes", s: multibyte, limit: 4, suffix: "...", want: multibyte},
		{name: "cut leaves room for the suffix", s: multibyte, limit: 3, suffix: "...", want: "..."},
		{name: "ascii cut", s: "abcdefgh", limit: 6, suffix: "...", want: "abc..."},
		{name: "limit below the suffix cuts the suffix", s: "abcdef", limit: 2, suffix: "...", want: ".."},
		{name: "zero limit yields nothing", s: "abc", limit: 0, suffix: "...", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateRunesTotal(tt.s, tt.limit, tt.suffix)
			if got != tt.want {
				t.Errorf("TruncateRunesTotal(%q, %d, %q) = %q, want %q", tt.s, tt.limit, tt.suffix, got, tt.want)
			}
			if n := utf8.RuneCountInString(got); n > max(tt.limit, 0) {
				t.Errorf("result %q is %d runes, over the %d limit", got, n, tt.limit)
			}
		})
	}
}

func TestTruncateMiddle(t *testing.T) {
	t.Parallel()
	const marker = "..."
	tests := []struct {
		name  string
		s     string
		limit int
		want  string
	}{
		{name: "under limit is untouched", s: "abc", limit: 10, want: "abc"},
		{name: "keeps head and tail", s: "abcdefghij", limit: 7, want: "ab...ij"},
		{name: "limit too small for the marker cuts the head", s: "abcdefghij", limit: 4, want: "abcd"},
		{name: "zero limit yields nothing", s: "abcdef", limit: 0, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateMiddle(tt.s, tt.limit, marker)
			if got != tt.want {
				t.Errorf("TruncateMiddle(%q, %d, %q) = %q, want %q", tt.s, tt.limit, marker, got, tt.want)
			}
		})
	}
}

func TestTruncateMiddleKeepsTheTail(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("x", 200) + "EXIT_STATUS=1"
	got := TruncateMiddle(s, 60, "\n... (truncated) ...\n")
	if !strings.HasSuffix(got, "EXIT_STATUS=1") {
		t.Errorf("middle truncation dropped the tail: %q", got)
	}
}

func TestTruncateBytesTrimmed(t *testing.T) {
	t.Parallel()
	if got := TruncateBytesTrimmed("abc   def", 6, "..."); got != "abc..." {
		t.Errorf("TruncateBytesTrimmed = %q, want %q", got, "abc...")
	}
	if got := TruncateBytesTrimmed("abc", 10, "..."); got != "abc" {
		t.Errorf("under-limit input was modified: %q", got)
	}
}

// Every function must return valid UTF-8 at every possible cut point. This is
// the invariant the six byte-slicing helpers this package replaced all missed.
func TestEveryCutPointStaysValidUTF8(t *testing.T) {
	t.Parallel()
	// Mixed-width runes so some limits necessarily land mid-rune.
	s := "ok " + multibyte + " tail " + multibyte
	for limit := -2; limit <= len(s)+2; limit++ {
		for _, suffix := range []string{"...", "…", "\n... (truncated)", ""} {
			for name, got := range map[string]string{
				"TruncateBytes":       TruncateBytes(s, limit, suffix),
				"TruncateBytesTotal":  TruncateBytesTotal(s, limit, suffix),
				"TruncateRunesTotal":  TruncateRunesTotal(s, limit, suffix),
				"TruncateMiddle":      TruncateMiddle(s, limit, suffix),
				"TruncateBytesTrimed": TruncateBytesTrimmed(s, limit, suffix),
			} {
				if !utf8.ValidString(got) {
					t.Errorf("%s(%q, %d, %q) = %q: invalid UTF-8", name, s, limit, suffix, got)
				}
			}
		}
	}
}
