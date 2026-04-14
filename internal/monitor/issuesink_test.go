package monitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeExecer records every gh invocation and returns canned responses keyed
// by subcommand ("label", "issue list", "issue comment", "issue create").
// Tests mutate the response table per scenario.
type fakeExecer struct {
	mu         sync.Mutex
	calls      [][]string
	listResp   []byte
	listErr    error
	createErr  error
	commentErr error
	labelErr   error
}

func (f *fakeExecer) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "label":
		return nil, f.labelErr
	case "issue":
		if len(args) < 2 {
			return nil, nil
		}
		switch args[1] {
		case "list":
			return f.listResp, f.listErr
		case "comment":
			return nil, f.commentErr
		case "create":
			return nil, f.createErr
		}
	}
	return nil, nil
}

func (f *fakeExecer) callsMatching(prefix ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for i := range prefix {
			if c[i] != prefix[i] {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

func newTestSink(e ghExecer) *GHIssueSink {
	return &GHIssueSink{exec: e, label: "monitor"}
}

func TestGHIssueSink_DedupMissCreates(t *testing.T) {
	fe := &fakeExecer{listResp: []byte(`[]`)}
	s := newTestSink(fe)

	a := Anomaly{
		Kind:        KindOverDispatchLimit,
		Fingerprint: "over_dispatch_limit",
	}
	created, err := s.Submit(context.Background(), a, "body")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on dedup miss")
	}

	creates := fe.callsMatching("issue", "create")
	if len(creates) != 1 {
		t.Fatalf("want 1 issue create call, got %d", len(creates))
	}
	got := creates[0]
	if !containsPair(got, "--title", "[monitor] over_dispatch_limit") {
		t.Errorf("wrong title: %v", got)
	}
	if !containsPair(got, "--body", "body") {
		t.Errorf("missing body: %v", got)
	}
	if !containsPair(got, "--label", "monitor,bug") {
		t.Errorf("missing label pair: %v", got)
	}
	if len(fe.callsMatching("issue", "comment")) != 0 {
		t.Errorf("should not comment on dedup miss")
	}
}

func TestGHIssueSink_DedupHitComments(t *testing.T) {
	fe := &fakeExecer{
		listResp: []byte(`[{"number":87,"title":"[monitor] failure_spike"}]`),
	}
	s := newTestSink(fe)

	a := Anomaly{
		Kind:        KindFailureSpike,
		Fingerprint: "failure_spike",
	}
	created, err := s.Submit(context.Background(), a, "new evidence")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if created {
		t.Fatal("expected created=false on dedup hit")
	}

	comments := fe.callsMatching("issue", "comment")
	if len(comments) != 1 {
		t.Fatalf("want 1 comment call, got %d", len(comments))
	}
	if comments[0][2] != "87" {
		t.Errorf("commented on wrong issue: %v", comments[0])
	}
	if !containsPair(comments[0], "--body", "new evidence") {
		t.Errorf("missing body in comment: %v", comments[0])
	}
	if len(fe.callsMatching("issue", "create")) != 0 {
		t.Errorf("should not create on dedup hit")
	}
}

func TestGHIssueSink_LabelsEnsuredOnce(t *testing.T) {
	fe := &fakeExecer{listResp: []byte(`[]`)}
	s := newTestSink(fe)

	for i := range 3 {
		a := Anomaly{
			Kind:        KindOverDispatchLimit,
			Fingerprint: "over_dispatch_limit",
		}
		if _, err := s.Submit(context.Background(), a, "body"); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	labelCreates := fe.callsMatching("label", "create")
	// Two labels (monitor + bug) are created once, not three times.
	if len(labelCreates) != 2 {
		t.Fatalf("want 2 label create calls (once), got %d", len(labelCreates))
	}
}

func TestGHIssueSink_ClassifiesRateLimit(t *testing.T) {
	fe := &fakeExecer{
		listResp:  []byte(`[]`),
		createErr: errors.New("HTTP 403: API rate limit exceeded for 1.2.3.4"),
	}
	s := newTestSink(fe)

	a := Anomaly{Kind: KindOverDispatchLimit, Fingerprint: "over_dispatch_limit"}
	_, err := s.Submit(context.Background(), a, "body")
	if !errors.Is(err, ErrGHRateLimit) {
		t.Fatalf("want ErrGHRateLimit, got %v", err)
	}
}

func TestGHIssueSink_ListErrorPropagates(t *testing.T) {
	fe := &fakeExecer{
		listResp: nil,
		listErr:  errors.New("gh api failed"),
	}
	s := newTestSink(fe)

	a := Anomaly{Kind: KindOverDispatchLimit, Fingerprint: "over_dispatch_limit"}
	_, err := s.Submit(context.Background(), a, "body")
	if err == nil {
		t.Fatal("expected error on list failure")
	}
	if errors.Is(err, ErrGHRateLimit) {
		t.Fatal("unexpected rate-limit classification for non-rate-limit error")
	}
}

func TestGHIssueSink_FingerprintTitleExactMatch(t *testing.T) {
	// Two issues in the response: one that shares a prefix but is not the
	// exact title, one that is. Sink must return the exact match only.
	fe := &fakeExecer{
		listResp: []byte(`[{"number":10,"title":"[monitor] over_dispatch_limit: other"},{"number":42,"title":"[monitor] over_dispatch_limit"}]`),
	}
	s := newTestSink(fe)

	a := Anomaly{Kind: KindOverDispatchLimit, Fingerprint: "over_dispatch_limit"}
	created, err := s.Submit(context.Background(), a, "body")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if created {
		t.Fatal("expected dedup hit on exact title match")
	}
	comments := fe.callsMatching("issue", "comment")
	if len(comments) != 1 {
		t.Fatalf("want 1 comment, got %d", len(comments))
	}
	if comments[0][2] != "42" {
		t.Errorf("wrong issue number commented: got %s want 42", comments[0][2])
	}
}

// containsPair returns true if args contains key followed immediately by val.
func containsPair(args []string, key, val string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == val {
			return true
		}
	}
	return false
}

// verify the fakeExecer actually satisfies the ghExecer interface at compile
// time (the test file compiles even if the production file renames the
// interface, catching drift early).
var _ ghExecer = (*fakeExecer)(nil)

// ensure strings import is used on systems where the file is stripped down.
var _ = strings.Contains
