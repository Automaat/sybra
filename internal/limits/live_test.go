package limits

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestFetchClaudeLiveSnapshot_RetriesAfterCredentialReread(t *testing.T) {
	prevReader := claudeCredentialsReader
	prevClient := claudeUsageHTTPClient
	t.Cleanup(func() {
		claudeCredentialsReader = prevReader
		claudeUsageHTTPClient = prevClient
	})

	var reads int
	claudeCredentialsReader = func(context.Context) (claudeCredentials, bool, error) {
		reads++
		token := "old-token"
		if reads > 1 {
			token = "new-token"
		}
		var credentials claudeCredentials
		credentials.ClaudeAIOAuth.AccessToken = token
		credentials.ClaudeAIOAuth.SubscriptionType = "max"
		return credentials, true, nil
	}

	var authHeaders []string
	claudeUsageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		status := http.StatusUnauthorized
		body := `{"error":"stale token"}`
		if req.Header.Get("Authorization") == "Bearer new-token" {
			status = http.StatusOK
			body = `{"five_hour":{"utilization":42,"resets_at":"2026-07-15T01:00:00Z"}}`
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}

	now := time.Date(2026, 7, 14, 21, 0, 0, 0, time.UTC)
	snapshot, ok, err := fetchClaudeLiveSnapshot(context.Background(), now)
	if err != nil {
		t.Fatalf("fetchClaudeLiveSnapshot error = %v", err)
	}
	if !ok {
		t.Fatal("fetchClaudeLiveSnapshot returned ok=false")
	}
	if reads != 2 {
		t.Fatalf("credential reads = %d, want 2 after auth retry", reads)
	}
	if got := strings.Join(authHeaders, ","); got != "Bearer old-token,Bearer new-token" {
		t.Fatalf("authorization headers = %q", got)
	}
	if snapshot.PlanType != "max" || snapshot.Primary == nil || snapshot.Primary.UsedPercent != 42 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestFetchClaudeLiveSnapshot_ClassifiesAuthFailure(t *testing.T) {
	prevReader := claudeCredentialsReader
	prevClient := claudeUsageHTTPClient
	t.Cleanup(func() {
		claudeCredentialsReader = prevReader
		claudeUsageHTTPClient = prevClient
	})

	claudeCredentialsReader = func(context.Context) (claudeCredentials, bool, error) {
		var credentials claudeCredentials
		credentials.ClaudeAIOAuth.AccessToken = "same-token"
		return credentials, true, nil
	}
	claudeUsageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader(`{"error":"forbidden"}`)),
			Header:     make(http.Header),
		}, nil
	})}

	_, ok, err := fetchClaudeLiveSnapshot(context.Background(), time.Now().UTC())
	if ok {
		t.Fatal("fetchClaudeLiveSnapshot returned ok=true on auth rejection")
	}
	if !IsLivePollAuthError(err, ProviderClaude) {
		t.Fatalf("expected Claude auth classification, got %v", err)
	}
	var liveErr *LivePollError
	if !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "fetch Claude usage snapshot") {
		t.Fatalf("error string = %q", err)
	}
	if !errors.As(err, &liveErr) {
		t.Fatalf("error did not unwrap to LivePollError: %v", err)
	}
	if liveErr.Kind != LivePollErrorKindAuth || liveErr.StatusCode != http.StatusForbidden {
		t.Fatalf("live error = %+v", liveErr)
	}
}
