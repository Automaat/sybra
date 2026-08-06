package errclass

import (
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want Class
	}{
		{name: "empty", text: "", want: Unknown},
		{name: "unrecognized fails closed to unknown", text: "the flux capacitor is misaligned", want: Unknown},

		{name: "dial tcp", text: "Get \"https://api.github.com\": dial tcp 140.82.121.6:443: connect: connection refused", want: Transient},
		{name: "i/o timeout", text: "read tcp: i/o timeout", want: Transient},
		{name: "context deadline exceeded", text: "context deadline exceeded", want: Transient},
		{name: "dns", text: "fatal: could not resolve host: github.com", want: Transient},
		{name: "gateway 502", text: "gh: HTTP 502", want: Transient},
		{name: "gateway 503", text: "gh: HTTP 503 Service Unavailable", want: Transient},
		{name: "git sideband", text: "fatal: unexpected disconnect while reading sideband packet", want: Transient},
		{name: "blocked merge timeout is not transient", text: "required status check timed out", want: Unknown},

		{name: "secondary rate limit", text: "You have exceeded a secondary rate limit", want: RateLimited},
		{name: "api rate limit exceeded", text: "API rate limit exceeded for user ID 1", want: RateLimited},

		{name: "bad credentials", text: "gh: Bad credentials (HTTP 401)", want: Auth},
		{name: "http 401", text: "gh: HTTP 401", want: Auth},
		{name: "401 unauthorized", text: "remote: 401 Unauthorized", want: Auth},
		{name: "token expired", text: "the token has expired", want: Auth},

		{name: "auth outranks rate limit", text: "Bad credentials; also rate limit exceeded", want: Auth},
		{name: "rate limit outranks transient", text: "connection reset; api rate limit exceeded", want: RateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.text); got != tc.want {
				t.Fatalf("Classify(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestClassifyIsCaseInsensitive pins the defect that made internal/monitor
// miss GitHub's own capitalization and retry against an exhausted token.
func TestClassifyIsCaseInsensitive(t *testing.T) {
	for _, text := range []string{
		"Secondary rate limit",
		"SECONDARY RATE LIMIT",
		"API rate limit exceeded",
		"api rate limit exceeded",
	} {
		if got := Classify(text); got != RateLimited {
			t.Fatalf("Classify(%q) = %q, want %q", text, got, RateLimited)
		}
	}
}

// TestStreamPhrasesAreNotNetwork pins why truncated-response phrases sit in
// their own family: a GraphQL parse defect must escalate, not retry.
func TestStreamPhrasesAreNotNetwork(t *testing.T) {
	const parseDefect = "parse graphql response: unexpected EOF"
	if IsNetwork(parseDefect) {
		t.Fatal("IsNetwork matched a parse defect, which would stop it escalating")
	}
	if Classify(parseDefect) != Unknown {
		t.Fatalf("Classify(%q) = %q, want %q", parseDefect, Classify(parseDefect), Unknown)
	}
	if !Matches(parseDefect, StreamPhrases) {
		t.Fatal("StreamPhrases should still match for callers reading raw transport output")
	}
}

// TestPlainHTTP500IsNotGateway pins the narrower of the two answers the merged
// sites gave: a bare 500 is usually a real server-side bug.
func TestPlainHTTP500IsNotGateway(t *testing.T) {
	if IsGateway("gh: HTTP 500") {
		t.Fatal("IsGateway matched a plain 500")
	}
}

// TestIsBadRef pins the twelve needles that lived byte-identically in
// internal/project and internal/workflow.
func TestIsBadRef(t *testing.T) {
	for _, text := range []string{
		"fatal: bad object HEAD",
		"error: object file .git/objects/ab/cdef is empty",
		"fatal: ambiguous argument 'origin/main...HEAD': unknown revision or path not in the working tree",
		"fatal: Invalid revision range origin/main...HEAD",
	} {
		if !IsBadRef(text) {
			t.Fatalf("IsBadRef(%q) = false, want true", text)
		}
	}
	if IsBadRef("connection refused") {
		t.Fatal("IsBadRef matched a network error")
	}
}

func TestClassifyErr(t *testing.T) {
	if got := ClassifyErr(nil); got != Unknown {
		t.Fatalf("ClassifyErr(nil) = %q, want %q", got, Unknown)
	}
	if got := ClassifyErr(errors.New("connection refused")); got != Transient {
		t.Fatalf("ClassifyErr = %q, want %q", got, Transient)
	}
	if got := ClassifyErr(errors.New("Bad credentials")); got != Auth {
		t.Fatalf("ClassifyErr = %q, want %q", got, Auth)
	}
}
