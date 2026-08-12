package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/workercontrol"
)

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
		return fmt.Errorf("leader %s %s: status %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(limited)))
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
		AcknowledgedThrough uint64 `json:"acknowledgedThrough"`
	}
	err := c.call(ctx, http.MethodPost, "/worker/v1/events", batch, &response)
	return response.AcknowledgedThrough, err
}

func (c *leaderClient) artifact(ctx context.Context, upload workercontrol.ArtifactUpload) error {
	return c.call(ctx, http.MethodPost, "/worker/v1/artifacts", upload, &map[string]any{})
}
