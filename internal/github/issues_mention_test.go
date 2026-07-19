package github

import (
	"strings"
	"testing"
)

func TestMentionedRepoQuery(t *testing.T) {
	t.Parallel()
	q := mentionedRepoQuery([]string{"o/a", "o/b"}, "@sybra")
	want := `is:issue is:open in:comments "@sybra" repo:o/a repo:o/b sort:updated-desc`
	if q != want {
		t.Fatalf("query = %q, want %q", q, want)
	}
}

func TestFetchMentionedIssuesForReposWith_EmptyInputs(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{"data":{"search":{"nodes":[]}}}`)}

	if issues, err := fetchMentionedIssuesForReposWith(fe, nil, "@sybra"); err != nil || issues != nil {
		t.Fatalf("no repos: issues=%v err=%v, want nil, nil", issues, err)
	}
	if issues, err := fetchMentionedIssuesForReposWith(fe, []string{"o/a"}, ""); err != nil || issues != nil {
		t.Fatalf("empty phrase: issues=%v err=%v, want nil, nil", issues, err)
	}
}

func TestFetchMentionedIssuesForReposWith_ChunksAndDedupes(t *testing.T) {
	t.Parallel()
	repos := make([]string, 20) // > 1 chunk of 15
	for i := range repos {
		repos[i] = "o/r" + string(rune('a'+i))
	}
	me := &mentionChunkExecer{}

	issues, err := fetchMentionedIssuesForReposWith(me, repos, "@sybra")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(me.queries) != 2 {
		t.Fatalf("gh calls = %d, want 2 (chunked at 15)", len(me.queries))
	}
	if !strings.Contains(me.queries[0], `in:comments "@sybra"`) {
		t.Fatalf("query missing mention qualifier: %q", me.queries[0])
	}
	if got := strings.Count(me.queries[0], "repo:"); got != 15 {
		t.Fatalf("chunk 1 repo count = %d, want 15", got)
	}
	if got := strings.Count(me.queries[1], "repo:"); got != 5 {
		t.Fatalf("chunk 2 repo count = %d, want 5", got)
	}
	if len(issues) != 2 {
		t.Fatalf("issues = %d, want 2 (one per chunk)", len(issues))
	}
}

// mentionChunkExecer returns one distinct issue per search chunk and records
// every query string, mirroring chunkExecer in issues_chunk_test.go.
type mentionChunkExecer struct {
	queries []string
}

func (m *mentionChunkExecer) run(args ...string) ([]byte, error) {
	var q string
	for i, a := range args {
		if a == "-f" && i+1 < len(args) {
			if v, ok := strings.CutPrefix(args[i+1], "q="); ok {
				q = v
			}
		}
	}
	m.queries = append(m.queries, q)
	return []byte(`{"data":{"search":{"nodes":[` +
		issueNode("https://github.com/o/r/issues/"+itoaLen(m.queries)) + `]}}}`), nil
}
