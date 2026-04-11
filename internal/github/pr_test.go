package github

import (
	"fmt"
	"testing"
)

func TestFetchPRStateWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		execErr error
		want    PRState
		wantErr bool
	}{
		{
			name:   "merged PR",
			output: `{"state":"MERGED","mergedAt":"2026-04-01T12:00:00Z"}`,
			want:   PRState{State: "MERGED", MergedAt: "2026-04-01T12:00:00Z"},
		},
		{
			name:   "closed PR",
			output: `{"state":"CLOSED","mergedAt":""}`,
			want:   PRState{State: "CLOSED"},
		},
		{
			name:   "open PR",
			output: `{"state":"OPEN","mergedAt":""}`,
			want:   PRState{State: "OPEN"},
		},
		{
			name:    "exec error",
			output:  "gh: not found",
			execErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			output:  "not json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			got, err := fetchPRStateWith(fe, "o/r", 42)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.State != tt.want.State {
				t.Errorf("State = %q, want %q", got.State, tt.want.State)
			}
			if got.MergedAt != tt.want.MergedAt {
				t.Errorf("MergedAt = %q, want %q", got.MergedAt, tt.want.MergedAt)
			}
		})
	}
}

func TestPRState_CIStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		checks []struct{ State string }
		want   string
	}{
		{"no checks", nil, ""},
		{"all success", []struct{ State string }{{"SUCCESS"}, {"NEUTRAL"}}, "SUCCESS"},
		{"has failure", []struct{ State string }{{"SUCCESS"}, {"FAILURE"}}, "FAILURE"},
		{"has error", []struct{ State string }{{"ERROR"}}, "FAILURE"},
		{"has pending", []struct{ State string }{{"SUCCESS"}, {"PENDING"}}, "PENDING"},
		{"failure beats pending", []struct{ State string }{{"PENDING"}, {"FAILURE"}}, "FAILURE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checks := make([]struct {
				State string `json:"state"`
			}, len(tt.checks))
			for i, c := range tt.checks {
				checks[i].State = c.State
			}
			s := PRState{StatusCheckRollup: checks}
			if got := s.CIStatus(); got != tt.want {
				t.Errorf("CIStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRState_ReadyToMerge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		state     PRState
		wantReady bool
	}{
		{"open mergeable no ci", PRState{State: "OPEN", Mergeable: "MERGEABLE"}, true},
		{"open mergeable ci success", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []struct {
			State string `json:"state"`
		}{{"SUCCESS"}}}, true},
		{"not open", PRState{State: "MERGED", Mergeable: "MERGEABLE"}, false},
		{"conflicting", PRState{State: "OPEN", Mergeable: "CONFLICTING"}, false},
		{"unknown mergeable", PRState{State: "OPEN", Mergeable: "UNKNOWN"}, false},
		{"ci failing", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []struct {
			State string `json:"state"`
		}{{"FAILURE"}}}, false},
		{"ci pending", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []struct {
			State string `json:"state"`
		}{{"PENDING"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.ReadyToMerge(); got != tt.wantReady {
				t.Errorf("ReadyToMerge() = %v, want %v", got, tt.wantReady)
			}
		})
	}
}

func TestFetchPRFilesWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		execErr error
		want    []string
		wantErr bool
	}{
		{
			name:   "multiple files",
			output: `{"files":[{"path":"app.go"},{"path":"internal/task/store.go"},{"path":"main.go"}]}`,
			want:   []string{"app.go", "internal/task/store.go", "main.go"},
		},
		{
			name:   "single file",
			output: `{"files":[{"path":"README.md"}]}`,
			want:   []string{"README.md"},
		},
		{
			name:   "no files",
			output: `{"files":[]}`,
			want:   []string{},
		},
		{
			name:    "exec error",
			output:  "gh: not found",
			execErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			output:  "not json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			got, err := fetchPRFilesWith(fe, "o/r", 42)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d files, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("file[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFetchPRWith_success(t *testing.T) {
	t.Parallel()
	response := `{
		"number": 42,
		"title": "feat: add thing",
		"body": "description",
		"url": "https://github.com/owner/repo/pull/42",
		"headRefName": "feat/add-thing",
		"author": {"login": "dev"},
		"labels": [{"name": "backend"}, {"name": "feature"}]
	}`
	fe := &fakeExecer{output: []byte(response)}
	pr, err := fetchPRWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.Title != "feat: add thing" {
		t.Errorf("Title = %q, want %q", pr.Title, "feat: add thing")
	}
	if pr.HeadRefName != "feat/add-thing" {
		t.Errorf("HeadRefName = %q, want %q", pr.HeadRefName, "feat/add-thing")
	}
	if pr.Author != "dev" {
		t.Errorf("Author = %q, want %q", pr.Author, "dev")
	}
	if pr.Repository != "owner/repo" {
		t.Errorf("Repository = %q, want %q", pr.Repository, "owner/repo")
	}
	if pr.RepoName != "repo" {
		t.Errorf("RepoName = %q, want %q", pr.RepoName, "repo")
	}
	if len(pr.Labels) != 2 || pr.Labels[0] != "backend" || pr.Labels[1] != "feature" {
		t.Errorf("Labels = %v, want [backend feature]", pr.Labels)
	}
}

func TestFetchPRWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{
		output: []byte("not found"),
		err:    fmt.Errorf("exit 1"),
	}
	_, err := fetchPRWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchPRWith_invalidJSON(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("not json")}
	_, err := fetchPRWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFetchPRClosingIssuesWith_sameRepo(t *testing.T) {
	t.Parallel()
	response := `{
		"body": "Initial body",
		"closingIssuesReferences": [
			{"number": 7, "repository": {"name": "repo", "owner": {"login": "owner"}}}
		]
	}`
	fe := &fakeExecer{output: []byte(response)}
	issues, body, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 7 {
		t.Errorf("issues = %v, want [7]", issues)
	}
	if body != "Initial body" {
		t.Errorf("body = %q, want %q", body, "Initial body")
	}
}

func TestFetchPRClosingIssuesWith_filtersCrossRepo(t *testing.T) {
	t.Parallel()
	response := `{
		"body": "",
		"closingIssuesReferences": [
			{"number": 1, "repository": {"name": "repo", "owner": {"login": "owner"}}},
			{"number": 99, "repository": {"name": "other", "owner": {"login": "elsewhere"}}}
		]
	}`
	fe := &fakeExecer{output: []byte(response)}
	issues, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 1 {
		t.Errorf("issues = %v, want [1] (99 belongs to elsewhere/other and must be filtered)", issues)
	}
}

func TestFetchPRClosingIssuesWith_empty(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{"body": "", "closingIssuesReferences": []}`)}
	issues, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("issues = %v, want empty", issues)
	}
}

func TestFetchPRClosingIssuesWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("boom"), err: fmt.Errorf("exit 1")}
	_, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchPRClosingIssuesWith_invalidJSON(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("not json")}
	_, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEditPRBodyWith_passesArgs(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{}
	if err := editPRBodyWith(fe, "owner/repo", 42, "new body with\nnewline"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Args should be: pr edit 42 --repo owner/repo --body <body>
	want := []string{"pr", "edit", "42", "--repo", "owner/repo", "--body", "new body with\nnewline"}
	if len(fe.lastArgs) != len(want) {
		t.Fatalf("args = %v, want %v", fe.lastArgs, want)
	}
	for i, a := range fe.lastArgs {
		if a != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, a, want[i])
		}
	}
}

func TestEditPRBodyWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("forbidden"), err: fmt.Errorf("exit 1")}
	if err := editPRBodyWith(fe, "owner/repo", 42, "body"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMergePRWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		execErr error
		output  string
		wantErr bool
	}{
		{
			name:   "success",
			output: "",
		},
		{
			name:    "exec error",
			output:  "gh: not found",
			execErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			err := mergePRWith(fe, "owner/repo", 42)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
