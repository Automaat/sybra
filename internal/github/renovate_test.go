package github

import (
	"testing"
)

func TestConvertRenovatePRs(t *testing.T) {
	t.Parallel()

	makeNode := func(login, typeName string) gqlPR {
		n := gqlPR{Number: 1, Title: "chore: update dep", URL: "https://github.com/o/r/pull/1"}
		n.Author.Login = login
		n.Author.Type = typeName
		n.Repository.Name = "r"
		n.Repository.NameWithOwner = "o/r"
		return n
	}

	t.Run("includes renovate bot PR", func(t *testing.T) {
		t.Parallel()
		node := makeNode("renovate[bot]", "Bot")
		prs := convertRenovatePRs([]gqlPR{node}, "")
		if len(prs) != 1 {
			t.Fatalf("got %d PRs, want 1", len(prs))
		}
		if prs[0].Author != "renovate[bot]" {
			t.Errorf("Author = %q, want renovate[bot]", prs[0].Author)
		}
	})

	t.Run("extracts check runs from contexts", func(t *testing.T) {
		t.Parallel()
		node := makeNode("renovate[bot]", "Bot")
		node.Commits.Nodes = []struct {
			Commit struct {
				StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
			} `json:"commit"`
		}{{Commit: struct {
			StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
		}{StatusCheckRollup: &gqlStatusCheckRollup{
			State: "FAILURE",
			Contexts: struct {
				Nodes []gqlCheckContext `json:"nodes"`
			}{Nodes: []gqlCheckContext{
				{Name: "ci/lint", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "", Status: "COMPLETED", Conclusion: "SUCCESS"}, // empty name skipped
			}},
		}}}}
		prs := convertRenovatePRs([]gqlPR{node}, "")
		if len(prs) != 1 {
			t.Fatalf("got %d PRs, want 1", len(prs))
		}
		if prs[0].CIStatus != "FAILURE" {
			t.Errorf("CIStatus = %q, want FAILURE", prs[0].CIStatus)
		}
		if len(prs[0].CheckRuns) != 1 {
			t.Fatalf("CheckRuns len = %d, want 1 (empty name should be skipped)", len(prs[0].CheckRuns))
		}
		if prs[0].CheckRuns[0].Name != "ci/lint" {
			t.Errorf("CheckRuns[0].Name = %q, want ci/lint", prs[0].CheckRuns[0].Name)
		}
	})
}
