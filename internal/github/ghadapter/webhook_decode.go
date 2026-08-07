package ghadapter

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v88/github"
)

// DecodeIssueComment validates an inbound webhook request's HMAC signature
// and decodes it as an issue_comment event, as an evaluation replacement for
// cmd/sybra-server/webhook_github.go's hand-rolled githubIssueCommentPayload
// struct + validWebhookSignature check.
//
// Differences from the current handler worth calling out in the adopt
// decision:
//   - ValidatePayload requires a recognized Content-Type header
//     (application/json or application/x-www-form-urlencoded); the current
//     handler doesn't inspect Content-Type at all before json.Unmarshal.
//   - go-github enforces its own maximum payload size (25MB) inside
//     ValidatePayload, independent of and in addition to the
//     http.MaxBytesReader(w, r.Body, httpapi.MaxRequestBody) the current
//     handler already applies at the transport layer.
//   - IssueCommentEvent decodes the full upstream schema (many more fields
//     than the current handler's narrow payload struct), which is more
//     future-proof but also a larger trust surface per payload.
//   - ValidatePayload skips signature validation entirely when the secret is
//     empty, so an unsigned payload would be accepted. The current handler
//     rejects a blank secret outright (webhook_github.go's
//     strings.TrimSpace(cfg.Webhook.Secret) == "" check), so the blank-secret
//     guard below is mandatory in any adoption — it is not a Sybra-specific
//     nicety the library covers for you.
func DecodeIssueComment(r *http.Request, secret string) (*github.IssueCommentEvent, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("ghadapter: validate webhook payload: empty webhook secret")
	}

	payload, err := github.ValidatePayload(r, []byte(secret))
	if err != nil {
		return nil, fmt.Errorf("ghadapter: validate webhook payload: %w", err)
	}

	eventType := github.WebHookType(r)
	if eventType != "issue_comment" {
		return nil, fmt.Errorf("ghadapter: decode issue_comment: unexpected event type %q", eventType)
	}

	event, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		return nil, fmt.Errorf("ghadapter: parse issue_comment webhook: %w", err)
	}
	ic, ok := event.(*github.IssueCommentEvent)
	if !ok {
		return nil, fmt.Errorf("ghadapter: decode issue_comment: unexpected payload type %T", event)
	}
	return ic, nil
}
