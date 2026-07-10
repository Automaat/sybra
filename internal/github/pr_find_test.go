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
