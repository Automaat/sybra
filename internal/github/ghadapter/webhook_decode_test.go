package ghadapter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const testWebhookSecret = "test-secret"

func issueCommentFixture(command string) []byte {
	return []byte(`{
  "action": "created",
  "comment": {
    "id": 42,
    "body": "` + command + `",
    "author_association": "OWNER",
    "user": {"login": "octocat", "type": "User"}
  },
  "issue": {
    "number": 7,
    "title": "Something broke",
    "body": "steps to repro",
    "html_url": "https://github.com/Automaat/sybra/issues/7"
  },
  "repository": {"full_name": "Automaat/sybra"},
  "installation": {"id": 99}
}`)
}

func signedRequest(t *testing.T, body []byte, secret string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("X-Hub-Signature-256", sig)
	return r
}

func TestDecodeIssueComment_ValidFixture(t *testing.T) {
	body := issueCommentFixture("sybra ship")
	r := signedRequest(t, body, testWebhookSecret)

	event, err := DecodeIssueComment(r, testWebhookSecret)
	if err != nil {
		t.Fatalf("DecodeIssueComment: %v", err)
	}
	if event.GetAction() != "created" {
		t.Fatalf("Action = %q, want created", event.GetAction())
	}
	if event.GetComment().GetBody() != "sybra ship" {
		t.Fatalf("Comment.Body = %q, want %q", event.GetComment().GetBody(), "sybra ship")
	}
	if event.GetIssue().GetNumber() != 7 {
		t.Fatalf("Issue.Number = %d, want 7", event.GetIssue().GetNumber())
	}
	if event.GetRepo().GetFullName() != "Automaat/sybra" {
		t.Fatalf("Repo.FullName = %q, want Automaat/sybra", event.GetRepo().GetFullName())
	}
	if event.GetInstallation().GetID() != 99 {
		t.Fatalf("Installation.ID = %d, want 99", event.GetInstallation().GetID())
	}
}

func TestDecodeIssueComment_BadSignatureRejected(t *testing.T) {
	body := issueCommentFixture("sybra ship")
	r := signedRequest(t, body, "wrong-secret")

	if _, err := DecodeIssueComment(r, testWebhookSecret); err == nil {
		t.Fatalf("DecodeIssueComment with mismatched secret = nil error, want failure")
	}
}

func TestDecodeIssueComment_TamperedBodyRejected(t *testing.T) {
	body := issueCommentFixture("sybra ship")
	r := signedRequest(t, body, testWebhookSecret)
	// Tamper with the body after signing, simulating an on-path payload edit.
	r.Body = http.NoBody
	tampered := append(bytes.Clone(body), []byte("x")...)
	r2 := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(tampered))
	r2.Header = r.Header

	if _, err := DecodeIssueComment(r2, testWebhookSecret); err == nil {
		t.Fatalf("DecodeIssueComment with tampered body = nil error, want failure")
	}
}

func TestDecodeIssueComment_OversizedPayloadRejected(t *testing.T) {
	// go-github enforces its own 25MB ceiling inside ValidatePayload,
	// independent of any transport-level http.MaxBytesReader — build a body
	// past that ceiling and confirm it's rejected before signature check
	// would even matter.
	huge := bytes.Repeat([]byte("a"), 25*1024*1024+1)
	body := []byte(`{"action":"created","comment":{"body":"` + string(huge) + `"}}`)
	r := signedRequest(t, body, testWebhookSecret)

	_, err := DecodeIssueComment(r, testWebhookSecret)
	if err == nil {
		t.Fatalf("DecodeIssueComment with oversized payload = nil error, want failure")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("error = %v, want a maximum-size error", err)
	}
}

// go-github's ValidatePayload skips signature validation altogether when the
// secret is empty, so a blank/whitespace secret would make an unsigned
// payload decode cleanly. The current handler rejects a blank secret before
// looking at the signature; the adapter must too, or adopting it would widen
// the webhook trust boundary.
func TestDecodeIssueComment_BlankSecretRejected(t *testing.T) {
	for _, secret := range []string{"", " ", "\t\n"} {
		t.Run("secret="+strconv.Quote(secret), func(t *testing.T) {
			body := issueCommentFixture("sybra ship")

			// Unsigned: no X-Hub-Signature-256 at all, the case go-github
			// would otherwise wave through.
			unsigned := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
			unsigned.Header.Set("Content-Type", "application/json")
			unsigned.Header.Set("X-GitHub-Event", "issue_comment")
			if _, err := DecodeIssueComment(unsigned, secret); err == nil {
				t.Fatalf("DecodeIssueComment(unsigned, %q) = nil error, want failure", secret)
			}

			// Signed with the blank secret: still rejected, so an attacker
			// who knows the secret is unset gains nothing by signing.
			if _, err := DecodeIssueComment(signedRequest(t, body, secret), secret); err == nil {
				t.Fatalf("DecodeIssueComment(signed with %q) = nil error, want failure", secret)
			}
		})
	}
}

func TestDecodeIssueComment_WrongEventTypeRejected(t *testing.T) {
	body := issueCommentFixture("sybra ship")
	r := signedRequest(t, body, testWebhookSecret)
	r.Header.Set("X-GitHub-Event", "push")

	if _, err := DecodeIssueComment(r, testWebhookSecret); err == nil {
		t.Fatalf("DecodeIssueComment for a push event = nil error, want failure")
	}
}

func TestDecodeIssueComment_MissingContentTypeRejected(t *testing.T) {
	body := issueCommentFixture("sybra ship")
	r := signedRequest(t, body, testWebhookSecret)
	r.Header.Del("Content-Type")

	if _, err := DecodeIssueComment(r, testWebhookSecret); err == nil {
		t.Fatalf("DecodeIssueComment with no Content-Type = nil error, want failure")
	}
}
