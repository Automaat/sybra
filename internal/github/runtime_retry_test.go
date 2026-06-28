package github

import (
	"fmt"
	"testing"
)

func stubRetrySleep(t *testing.T) {
	t.Helper()
	orig := ghRetrySleep
	ghRetrySleep = func(int) {}
	t.Cleanup(func() { ghRetrySleep = orig })
}

func TestRunGHAPIWith_RetriesTransientThenSucceeds(t *testing.T) {
	stubRetrySleep(t)

	success := []byte("HTTP/2.0 200 OK\n\n{\"data\":{}}")
	se := &sequenceExecer{
		outputs: [][]byte{[]byte("gh: HTTP 502"), []byte("gh: HTTP 504"), success},
		errs:    []error{fmt.Errorf("exit 1"), fmt.Errorf("exit 1"), nil},
	}

	resp, err := runGHAPIWith(se, "", "graphql")
	if err != nil {
		t.Fatalf("want success after transient retries, got %v", err)
	}
	if se.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 transient + 1 success)", se.calls)
	}
	if resp.statusCode != 200 {
		t.Errorf("statusCode = %d, want 200", resp.statusCode)
	}
}

func TestRunGHAPIWith_GivesUpAfterMaxRetries(t *testing.T) {
	stubRetrySleep(t)

	tr := []byte("gh: HTTP 503")
	se := &sequenceExecer{
		outputs: [][]byte{tr, tr, tr, tr},
		errs:    []error{fmt.Errorf("e"), fmt.Errorf("e"), fmt.Errorf("e"), fmt.Errorf("e")},
	}

	if _, err := runGHAPIWith(se, "", "graphql"); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if se.calls != ghMaxRetries+1 {
		t.Errorf("calls = %d, want %d (initial + %d retries)", se.calls, ghMaxRetries+1, ghMaxRetries)
	}
}

func TestRunGHAPIWith_NoRetryOnNonTransient(t *testing.T) {
	stubRetrySleep(t)

	se := &sequenceExecer{
		outputs: [][]byte{[]byte("gh: HTTP 404")},
		errs:    []error{fmt.Errorf("exit 1")},
	}

	if _, err := runGHAPIWith(se, "", "graphql"); err == nil {
		t.Fatal("want error")
	}
	if se.calls != 1 {
		t.Errorf("calls = %d, want 1 (404 is not retried)", se.calls)
	}
}

func TestRunGHAPIWith_NoRetryOnRateLimit(t *testing.T) {
	stubRetrySleep(t)

	se := &sequenceExecer{
		outputs: [][]byte{[]byte("API rate limit exceeded")},
		errs:    []error{fmt.Errorf("exit 1")},
	}

	if _, err := runGHAPIWith(se, "", "graphql"); err == nil {
		t.Fatal("want error")
	}
	if se.calls != 1 {
		t.Errorf("calls = %d, want 1 (rate limits are paced by the gate, not retried)", se.calls)
	}
}

func TestGHRequestGate_ObservesGraphQLRateLimitBody(t *testing.T) {
	g := newGHRequestGate()

	g.observe(ghHTTPResponse{
		body: []byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for user ID 1."}]}`),
	}, nil)

	if !g.shouldSkipOptional("graphql") {
		t.Fatal("want optional GraphQL calls skipped after rate-limit body")
	}
}

func TestRunGHAPIWith_NoRetryOnWrite(t *testing.T) {
	stubRetrySleep(t)

	tr := []byte("gh: HTTP 502")
	se := &sequenceExecer{
		outputs: [][]byte{tr, tr, tr},
		errs:    []error{fmt.Errorf("e"), fmt.Errorf("e"), fmt.Errorf("e")},
	}

	// A POST mutation must not be retried even on a transient error — it may
	// already have applied server-side.
	if _, err := runGHAPIWith(se, "", "repos/o/r/pulls/1/requested_reviewers", "--method", "POST"); err == nil {
		t.Fatal("want error")
	}
	if se.calls != 1 {
		t.Errorf("calls = %d, want 1 (writes are not retried)", se.calls)
	}
}

func TestRunGHAPIWith_NoRetryOnSuccess(t *testing.T) {
	stubRetrySleep(t)

	se := &sequenceExecer{
		outputs: [][]byte{[]byte("HTTP/2.0 200 OK\n\n{}")},
		errs:    []error{nil},
	}

	if _, err := runGHAPIWith(se, "", "graphql"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if se.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on first-try success)", se.calls)
	}
}
