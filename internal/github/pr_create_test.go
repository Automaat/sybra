package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCreatePR_Success(t *testing.T) {
	orig := createPRRunner
	origFetch := defaultExecer
	t.Cleanup(func() { createPRRunner = orig; defaultExecer = origFetch })

	var gotArgs []string
	var gotDir string
	createPRRunner = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		gotDir = dir
		gotArgs = args
		return []byte("Warning: ...\nhttps://github.com/acme/widgets/pull/42\n"), nil
	}
	defaultExecer = &fakeExecer{output: []byte(`{"headRefOid":"deadbeef"}`)}

	number, sha, err := CreatePR(context.Background(), "/tmp/wt", CreatePRRequest{
		Repo:  "acme/widgets",
		Head:  "myfork:my-branch",
		Draft: true,
		Title: "feat(x): y",
		Body:  "## Motivation\n\nz",
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if number != 42 {
		t.Fatalf("number = %d, want 42", number)
	}
	if sha != "deadbeef" {
		t.Fatalf("sha = %q, want deadbeef", sha)
	}
	if gotDir != "/tmp/wt" {
		t.Fatalf("dir = %q, want /tmp/wt", gotDir)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"--repo acme/widgets", "--head myfork:my-branch", "--draft"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
}

func TestCreatePR_RunError(t *testing.T) {
	orig := createPRRunner
	t.Cleanup(func() { createPRRunner = orig })
	createPRRunner = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("permission denied"), errors.New("exit status 1")
	}

	_, _, err := CreatePR(context.Background(), "/tmp/wt", CreatePRRequest{Repo: "acme/widgets", Head: "b", Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCreatePR_UnparseableOutput(t *testing.T) {
	orig := createPRRunner
	t.Cleanup(func() { createPRRunner = orig })
	createPRRunner = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("no url here"), nil
	}

	_, _, err := CreatePR(context.Background(), "/tmp/wt", CreatePRRequest{Repo: "acme/widgets", Head: "b", Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"a\nb\n", "b"},
		{"a\n\n\n", "a"},
		{"", ""},
		{"only", "only"},
	}
	for _, tt := range tests {
		if got := lastNonEmptyLine(tt.in); got != tt.want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
