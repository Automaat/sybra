package github

import (
	"fmt"
	"testing"
)

func TestFetchPRCompare(t *testing.T) {
	resp := `{"total_commits":2,"files":[{"additions":10,"deletions":3},{"additions":5,"deletions":1}]}`
	fe := &fakeExecer{output: []byte(resp)}
	got, err := fetchPRCompareWith(fe, "o/r", "base", "head")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Commits != 2 {
		t.Errorf("Commits = %d, want 2", got.Commits)
	}
	if got.Additions != 15 || got.Deletions != 4 {
		t.Errorf("Additions/Deletions = %d/%d, want 15/4", got.Additions, got.Deletions)
	}
}

func TestFetchPRCompare_Error(t *testing.T) {
	fe := &fakeExecer{output: []byte("not found"), err: fmt.Errorf("exit 1")}
	if _, err := fetchPRCompareWith(fe, "o/r", "base", "head"); err == nil {
		t.Fatal("want error")
	}
}
