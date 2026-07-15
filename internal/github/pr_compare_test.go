package github

import "testing"

func TestParsePRCompare(t *testing.T) {
	resp := `{"total_commits":2,"status":"ahead","files":[{"additions":10,"deletions":3},{"additions":5,"deletions":1}]}`
	got, err := parsePRCompare([]byte(resp))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Commits != 2 {
		t.Errorf("Commits = %d, want 2", got.Commits)
	}
	if got.Status != "ahead" {
		t.Errorf("Status = %q, want ahead", got.Status)
	}
	if got.Additions != 15 || got.Deletions != 4 {
		t.Errorf("Additions/Deletions = %d/%d, want 15/4", got.Additions, got.Deletions)
	}
}

func TestParsePRCompare_NoEdits(t *testing.T) {
	got, err := parsePRCompare([]byte(`{"total_commits":0,"files":[]}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Commits != 0 || got.Additions != 0 {
		t.Errorf("want zero churn, got %+v", got)
	}
}

func TestParsePRCompare_BadJSON(t *testing.T) {
	if _, err := parsePRCompare([]byte("not json")); err == nil {
		t.Fatal("want parse error")
	}
}
