package github

import (
	"strings"
	"testing"
)

// chunkExecer returns a snapshot body for the combined assigned+labeled query
// and a search body for each labeled-only chunk, recording every query string.
type chunkExecer struct {
	queries []string
}

func (c *chunkExecer) run(args ...string) ([]byte, error) {
	var q string
	combined := false
	for i, a := range args {
		if strings.HasPrefix(a, "labeledQ=") || strings.HasPrefix(a, "assignedQ=") {
			combined = true
		}
		if a == "-f" && i+1 < len(args) {
			if v, ok := strings.CutPrefix(args[i+1], "q="); ok {
				q = v
			}
		}
		if v, ok := strings.CutPrefix(a, "labeledQ="); ok {
			q = v
		}
	}
	c.queries = append(c.queries, q)
	if combined {
		// assigned empty; labeled has one issue from the first chunk.
		return []byte(`{"data":{"assigned":{"nodes":[]},"labeled":{"nodes":[` +
			issueNode("https://github.com/o/r0/issues/1") + `]}}}`), nil
	}
	// Each labeled-only chunk returns one distinct issue.
	return []byte(`{"data":{"search":{"nodes":[` +
		issueNode("https://github.com/o/chunk/issues/"+itoaLen(c.queries)) + `]}}}`), nil
}

func issueNode(url string) string {
	return `{"number":1,"title":"t","url":"` + url + `","labels":{"nodes":[{"name":"sybra"}]}}`
}

func itoaLen(s []string) string {
	return string(rune('0' + len(s)))
}

func TestFetchIssueSnapshot_ChunksLabeledRepos(t *testing.T) {
	repos := make([]string, 32) // > 2 chunks of 15
	for i := range repos {
		repos[i] = "o/r" + string(rune('a'+i))
	}
	ce := &chunkExecer{}
	snap, err := fetchIssueSnapshotWith(ce, repos, "sybra")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// 32 repos: combined(15) + labeled chunks for repos[15:32] => 15 + 2 = 2 calls.
	// Total gh calls = 1 combined + 2 labeled = 3.
	if len(ce.queries) != 3 {
		t.Fatalf("gh calls = %d, want 3 (1 combined + 2 labeled chunks)", len(ce.queries))
	}
	// First combined query must carry only the first 15 repo qualifiers.
	if got := strings.Count(ce.queries[0], "repo:"); got != 15 {
		t.Fatalf("combined query repo count = %d, want 15", got)
	}
	// Labeled chunks cover the remaining 17 repos (15 + 2).
	if got := strings.Count(ce.queries[1], "repo:"); got != 15 {
		t.Fatalf("chunk 2 repo count = %d, want 15", got)
	}
	if got := strings.Count(ce.queries[2], "repo:"); got != 2 {
		t.Fatalf("chunk 3 repo count = %d, want 2", got)
	}
	// Labeled results merged across chunks (1 from combined + 2 from chunks).
	if len(snap.Labeled) != 3 {
		t.Fatalf("merged labeled = %d, want 3", len(snap.Labeled))
	}
}
