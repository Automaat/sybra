package scrub

import (
	"strings"
	"testing"
)

func TestScrub(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		text       string
		blocklist  []string
		wantHas    []string
		wantNotHas []string
		wantCount  int
	}{
		{
			name:       "empty input passes through",
			text:       "",
			blocklist:  []string{"work-org"},
			wantCount:  0,
			wantHas:    nil,
			wantNotHas: []string{Placeholder},
		},
		{
			name:       "no matches leaves text intact",
			text:       "totally innocuous diagnostic note",
			blocklist:  []string{"work-org"},
			wantCount:  0,
			wantHas:    []string{"totally innocuous"},
			wantNotHas: []string{Placeholder},
		},
		{
			name:       "blocklist literal redacted",
			text:       "task came from work-org/secret-repo with details",
			blocklist:  []string{"work-org/secret-repo", "work-org"},
			wantCount:  1,
			wantHas:    []string{Placeholder, "with details"},
			wantNotHas: []string{"work-org", "secret-repo"},
		},
		{
			name:       "longer blocklist entry wins",
			text:       "owner/repo and also just owner alone",
			blocklist:  []string{"owner", "owner/repo"},
			wantCount:  2,
			wantHas:    []string{Placeholder},
			wantNotHas: []string{"owner/repo", "owner alone"},
		},
		{
			name:       "github URL redacted",
			text:       "see https://github.com/work-org/secret-repo/pull/42 for context",
			blocklist:  nil,
			wantCount:  1,
			wantHas:    []string{Placeholder, "for context"},
			wantNotHas: []string{"work-org", "secret-repo", "/pull/42"},
		},
		{
			name:       "jira key redacted",
			text:       "this is blocked by KAG-1234 over in tickets",
			blocklist:  nil,
			wantCount:  1,
			wantHas:    []string{Placeholder, "over in tickets"},
			wantNotHas: []string{"KAG-1234"},
		},
		{
			name:       "single-letter prefix is NOT treated as jira",
			text:       "version A-1 ships next week",
			blocklist:  nil,
			wantCount:  0,
			wantHas:    []string{"A-1"},
			wantNotHas: []string{Placeholder},
		},
		{
			name:       "email redacted",
			text:       "ping alice@konghq.com if blocked",
			blocklist:  nil,
			wantCount:  1,
			wantHas:    []string{Placeholder, "if blocked"},
			wantNotHas: []string{"alice@konghq.com"},
		},
		{
			name:       "blocklist + static patterns combine",
			text:       "PR https://github.com/work-org/repo/pull/9 from alice@work-org.com (ticket KAG-1)",
			blocklist:  []string{"work-org"},
			wantCount:  3,
			wantHas:    []string{Placeholder},
			wantNotHas: []string{"work-org", "alice@", "KAG-1"},
		},
		{
			name:       "whitespace-only blocklist entries ignored",
			text:       "innocent text",
			blocklist:  []string{"", "   "},
			wantCount:  0,
			wantHas:    []string{"innocent text"},
			wantNotHas: []string{Placeholder},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, count := Scrub(tc.text, tc.blocklist)
			if count != tc.wantCount {
				t.Errorf("redaction count = %d, want %d (got=%q)", count, tc.wantCount, got)
			}
			for _, needle := range tc.wantHas {
				if !strings.Contains(got, needle) {
					t.Errorf("output should contain %q; got=%q", needle, got)
				}
			}
			for _, needle := range tc.wantNotHas {
				if strings.Contains(got, needle) {
					t.Errorf("output should NOT contain %q; got=%q", needle, got)
				}
			}
		})
	}
}
