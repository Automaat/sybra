package github

import "testing"

func TestParseCommitMessages(t *testing.T) {
	out := []byte("feat: a\n\nThis reverts commit abc.\nfix: b\n  \n")
	got := parseCommitMessages(out)
	want := []string{"feat: a", "This reverts commit abc.", "fix: b"}
	if len(got) != len(want) {
		t.Fatalf("got %d msgs %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("msg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseCommitMessages_Empty(t *testing.T) {
	if got := parseCommitMessages([]byte("")); len(got) != 0 {
		t.Errorf("empty output should yield no messages, got %v", got)
	}
}

func TestFetchCommitParentSHAsWith(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{output: []byte("parent1\nparent2\n")}

	got, err := fetchCommitParentSHAsWith(fe, "o/r", "head")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0] != "parent1" || got[1] != "parent2" {
		t.Fatalf("parents = %v, want [parent1 parent2]", got)
	}
	wantPath := "repos/o/r/commits/head"
	foundPath := false
	foundJQ := false
	for _, arg := range fe.lastArgs {
		if arg == wantPath {
			foundPath = true
		}
		if arg == ".parents[].sha" {
			foundJQ = true
		}
	}
	if !foundPath || !foundJQ {
		t.Fatalf("args = %v, want commit endpoint and parent jq", fe.lastArgs)
	}
}

func TestParseCommitParentSHAs(t *testing.T) {
	got := parseCommitParentSHAs([]byte(" parent1 \n\nparent2\n"))
	if len(got) != 2 || got[0] != "parent1" || got[1] != "parent2" {
		t.Fatalf("parents = %v, want [parent1 parent2]", got)
	}
}
