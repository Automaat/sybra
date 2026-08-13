package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/workercontrol"
)

type leaderHTTPError struct {
	method string
	path   string
	status int
	detail string
}

func (e *leaderHTTPError) Error() string {
	return fmt.Sprintf("leader %s %s: status %d: %s", e.method, e.path, e.status, e.detail)
}

func isRejectedSession(err error) bool {
	var responseErr *leaderHTTPError
	if !errors.As(err, &responseErr) || responseErr.status != http.StatusConflict {
		return false
	}
	var response struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(responseErr.detail), &response) != nil {
		return false
	}
	return response.Error == workercontrol.ErrStaleSession.Error() || response.Error == workercontrol.ErrLeaseExpired.Error()
}

type leaderClient struct {
	base  string
	token string
	http  *http.Client
}

func newLeaderClient(base, token string) *leaderClient {
	return &leaderClient{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 35 * time.Second}}
}

func (c *leaderClient) call(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &leaderHTTPError{method: method, path: path, status: response.StatusCode, detail: strings.TrimSpace(string(limited))}
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(response.Body).Decode(result)
}

func (c *leaderClient) register(ctx context.Context, request workercontrol.RegisterRequest) (workercontrol.Session, error) {
	var session workercontrol.Session
	err := c.call(ctx, http.MethodPost, "/worker/v1/register", request, &session)
	return session, err
}

func (c *leaderClient) heartbeat(ctx context.Context, sessionID string, capabilities []string) (workercontrol.Session, error) {
	var session workercontrol.Session
	err := c.call(ctx, http.MethodPost, "/worker/v1/heartbeat", map[string]any{"sessionId": sessionID, "capabilities": capabilities}, &session)
	return session, err
}

func (c *leaderClient) poll(ctx context.Context, sessionID string, after uint64, wait int) ([]workercontrol.Command, error) {
	query := url.Values{"session": {sessionID}, "after": {strconv.FormatUint(after, 10)}, "wait": {strconv.Itoa(wait)}}
	var commands []workercontrol.Command
	err := c.call(ctx, http.MethodGet, "/worker/v1/commands?"+query.Encode(), nil, &commands)
	return commands, err
}

func (c *leaderClient) ackCommands(ctx context.Context, sessionID string, through uint64) error {
	return c.call(ctx, http.MethodPost, "/worker/v1/commands/ack", map[string]any{"sessionId": sessionID, "through": through}, &map[string]any{})
}

func (c *leaderClient) events(ctx context.Context, batch workercontrol.EventBatch) (uint64, error) {
	var response struct {
		AcknowledgedThrough map[string]uint64 `json:"acknowledgedThrough"`
	}
	err := c.call(ctx, http.MethodPost, "/worker/v1/events", batch, &response)
	if err != nil {
		return 0, err
	}
	var acknowledged uint64
	for i := range batch.Events {
		event := &batch.Events[i]
		if through := response.AcknowledgedThrough[event.RunID]; through > acknowledged {
			acknowledged = through
		}
	}
	return acknowledged, nil
}

func (c *leaderClient) artifact(ctx context.Context, upload workercontrol.ArtifactUpload) error {
	return c.call(ctx, http.MethodPost, "/worker/v1/artifacts", upload, &map[string]any{})
}

func (c *leaderClient) runGrant(ctx context.Context, sessionID, runID string) (workercontrol.RunGrant, error) {
	var grant workercontrol.RunGrant
	err := c.call(ctx, http.MethodPost, "/worker/v1/runs/"+url.PathEscape(runID)+"/grant", map[string]string{"sessionId": sessionID}, &grant)
	return grant, err
}
