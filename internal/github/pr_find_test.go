package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFindPRForBranch_Found(t *testing.T) {
	orig := findPRRunner
	t.Cleanup(func() { findPRRunner = orig })

	var gotArgs []string
	findPRRunner = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`[{"number":77}]`), nil
	}

	number, found, err := FindPRForBranch(context.Background(), "acme/widgets", "myfork:my-branch")
	if err != nil {
		t.Fatalf("FindPRForBranch: %v", err)
	}
	if !found || number != 77 {
		t.Fatalf("got (%d, %v), want (77, true)", number, found)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"pr list", "--repo acme/widgets", "--head myfork:my-branch", "--state open"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}

func TestFindPRForBranch_NoneFound(t *testing.T) {
	orig := findPRRunner
	t.Cleanup(func() { findPRRunner = orig })
	findPRRunner = func(context.Context, ...string) ([]byte, error) { return []byte(`[]`), nil }

	number, found, err := FindPRForBranch(context.Background(), "acme/widgets", "b")
	if err != nil {
		t.Fatalf("FindPRForBranch: %v", err)
	}
	if found || number != 0 {
		t.Fatalf("got (%d, %v), want (0, false)", number, found)
	}
}

func TestFindPRForBranch_RunError(t *testing.T) {
	orig := findPRRunner
	t.Cleanup(func() { findPRRunner = orig })
	findPRRunner = func(context.Context, ...string) ([]byte, error) {
		return []byte("gh: rate limit"), errors.New("exit status 1")
	}

	if _, _, err := FindPRForBranch(context.Background(), "acme/widgets", "b"); err == nil {
		t.Fatal("expected error propagated to caller")
	}
}

func TestFindPRForBranchAnyState(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantNumber int
		wantState  string
		wantFound  bool
	}{
		{"merged wins over closed", `[{"number":10,"state":"CLOSED"},{"number":11,"state":"MERGED"}]`, 11, "MERGED", true},
		{"single open backfills", `[{"number":42,"state":"OPEN"}]`, 42, "OPEN", true},
		{"only closed leaves none", `[{"number":9,"state":"CLOSED"}]`, 0, "", false},
		{"ambiguous open leaves none", `[{"number":1,"state":"OPEN"},{"number":2,"state":"OPEN"}]`, 0, "", false},
		{"empty leaves none", `[]`, 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := findPRRunner
			t.Cleanup(func() { findPRRunner = orig })
			var gotArgs []string
			findPRRunner = func(_ context.Context, args ...string) ([]byte, error) {
				gotArgs = args
				return []byte(tc.payload), nil
			}
			number, state, found, err := FindPRForBranchAnyState(context.Background(), "acme/widgets", "b")
			if err != nil {
				t.Fatalf("FindPRForBranchAnyState: %v", err)
			}
			if number != tc.wantNumber || state != tc.wantState || found != tc.wantFound {
				t.Fatalf("got (%d, %q, %v), want (%d, %q, %v)", number, state, found, tc.wantNumber, tc.wantState, tc.wantFound)
			}
			if joined := strings.Join(gotArgs, " "); !strings.Contains(joined, "--state all") {
				t.Errorf("args %q missing --state all", joined)
			}
		})
	}
}

func TestFindPRForBranchAnyState_RunError(t *testing.T) {
	orig := findPRRunner
	t.Cleanup(func() { findPRRunner = orig })
	findPRRunner = func(context.Context, ...string) ([]byte, error) {
		return []byte("gh: boom"), errors.New("exit status 1")
	}
	if _, _, _, err := FindPRForBranchAnyState(context.Background(), "acme/widgets", "b"); err == nil {
		t.Fatal("expected error propagated to caller")
	}
}
