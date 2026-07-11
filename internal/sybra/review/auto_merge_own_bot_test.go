package review

import (
	"testing"

	"github.com/Automaat/sybra/internal/github"
)

func TestReadyForOwnBotAutoMerge(t *testing.T) {
	t.Parallel()
	base := github.PullRequest{SelfAuthoredBot: true, Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}
	with := func(mut func(*github.PullRequest)) github.PullRequest {
		p := base
		mut(&p)
		return p
	}
	cases := []struct {
		name string
		pr   github.PullRequest
		want bool
	}{
		{"self bot green", base, true},
		{"empty ci counts green", with(func(p *github.PullRequest) { p.CIStatus = "" }), true},
		{"not self authored bot", with(func(p *github.PullRequest) { p.SelfAuthoredBot = false }), false},
		{"ci failure", with(func(p *github.PullRequest) { p.CIStatus = "FAILURE" }), false},
		{"conflicting", with(func(p *github.PullRequest) { p.Mergeable = "CONFLICTING" }), false},
		{"changes requested", with(func(p *github.PullRequest) { p.ReviewDecision = "CHANGES_REQUESTED" }), false},
		{"unresolved threads", with(func(p *github.PullRequest) { p.UnresolvedCount = 1 }), false},
		{"rest sourced", with(func(p *github.PullRequest) { p.SourcedViaREST = true }), false},
		{"draft", with(func(p *github.PullRequest) { p.IsDraft = true }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := readyForOwnBotAutoMerge(tc.pr); got != tc.want {
				t.Errorf("readyForOwnBotAutoMerge = %v, want %v", got, tc.want)
			}
		})
	}
}
