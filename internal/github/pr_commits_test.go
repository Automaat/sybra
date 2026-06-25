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
