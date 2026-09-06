package github

import (
	"fmt"
	"slices"
	"testing"
)

func TestPRCIGateBindsRequiredChecksToSnapshotHead(t *testing.T) {
	for _, outcome := range []string{"SUCCESS", "FAILURE", "SKIPPED", "NEUTRAL", "CANCELLED", ""} {
		t.Run(outcome, func(t *testing.T) {
			e := &fakeExecer{output: fmt.Appendf(nil, `{"state":"OPEN","headRefOid":"current-sha","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":%q}]}`, outcome)}
			gate, err := fetchPRCIGateWith(t.Context(), e, "o/r", 42, []string{"test"})
			if err != nil || gate.SHA != "current-sha" || gate.Approved() != (outcome == "SUCCESS") {
				t.Fatalf("CI gate = %+v, %v", gate, err)
			}
		})
	}
}

func TestProtectedMergePinsVerifiedRevision(t *testing.T) {
	e := &recordingExecer{}
	if err := mergePRWith(e, "o/r", 42, "verified-head"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.lastArgs, []string{"pr", "merge", "42", "--repo", "o/r", "--squash", "--match-head-commit", "verified-head"}) {
		t.Fatalf("merge did not pin verified revision: %v", e.lastArgs)
	}
	if err := MergePRAtHead("o/r", 42, ""); err == nil {
		t.Fatal("empty verified revision accepted")
	}
}

func TestPRCIGateMissingAndEmptyChecksNeverPass(t *testing.T) {
	e := &fakeExecer{output: []byte(`{"state":"OPEN","headRefOid":"new-sha","statusCheckRollup":[]}`)}
	gate, err := fetchPRCIGateWith(t.Context(), e, "o/r", 42, []string{"required"})
	if err != nil || gate.Approved() || len(gate.Missing) != 1 {
		t.Fatalf("missing check gate = %+v, %v", gate, err)
	}
	if _, err := fetchPRCIGateWith(t.Context(), e, "o/r", 42, nil); err == nil {
		t.Fatal("empty required checks accepted")
	}
}
