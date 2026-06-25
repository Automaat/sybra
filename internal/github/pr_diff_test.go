package github

import (
	"fmt"
	"strings"
	"testing"
)

func TestFetchPRDiff(t *testing.T) {
	fe := &fakeExecer{output: []byte("diff --git a/x b/x\n+hi\n")}
	got, err := fetchPRDiffWith(fe, "o/r", 7)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "diff --git") {
		t.Errorf("got %q", got)
	}
}

func TestFetchPRDiff_Error(t *testing.T) {
	fe := &fakeExecer{output: []byte("boom"), err: fmt.Errorf("exit 1")}
	if _, err := fetchPRDiffWith(fe, "o/r", 7); err == nil {
		t.Fatal("want error")
	}
}
