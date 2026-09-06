package workerupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/workercontrol"
)

type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("worker updater: leader HTTP %d", e.status) }

type leaderClient struct {
	cfg  Config
	http *http.Client
}

func newLeaderClient(cfg Config) *leaderClient {
	return &leaderClient{cfg: cfg, http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("worker updater: redirects refused") },
	}}
}

func (c *leaderClient) call(ctx context.Context, method, path string, input, output any, authenticated bool) error {
	if authenticated {
		if err := c.identify(ctx); err != nil {
			return err
		}
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.LeaderURL, "/")+path, body)
	if err != nil {
		return err
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+os.Getenv(c.cfg.TokenEnv))
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &httpError{status: response.StatusCode}
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
}

// identify is unauthenticated: never send a board credential to a listener
// merely because it occupies the configured loopback port after a restart.
func (c *leaderClient) identify(ctx context.Context) error {
	var health struct {
		Service string `json:"service"`
		HomeID  string `json:"home_id"`
		Status  string `json:"status"`
	}
	if err := c.call(ctx, http.MethodGet, "/health", nil, &health, false); err != nil {
		return err
	}
	if health.Service != "sybra" || health.HomeID != c.cfg.LeaderHomeID || health.Status != "ok" {
		return errors.New("worker updater: unexpected leader identity; no credential sent")
	}
	return nil
}

func (c *leaderClient) current(ctx context.Context) (workercontrol.Diagnostics, error) {
	var rows []workercontrol.Diagnostics
	if err := c.call(ctx, http.MethodGet, "/worker/v1/diagnostics?workerId="+url.QueryEscape(c.cfg.WorkerID), nil, &rows, true); err != nil {
		return workercontrol.Diagnostics{}, err
	}
	var current []workercontrol.Diagnostics
	for i := range rows {
		row := &rows[i]
		if row.WorkerID == c.cfg.WorkerID && row.LeaseExpiresAt.After(time.Now()) && (row.State == "active" || row.State == "disabled" || row.State == "draining") {
			current = append(current, *row)
		}
	}
	if len(current) != 1 {
		return workercontrol.Diagnostics{}, errors.New("worker updater: expected exactly one live worker session")
	}
	return current[0], nil
}

func (c *leaderClient) update(ctx context.Context, action string, journal journal, output any) error {
	request := workercontrol.UpdateRequest{WorkerID: c.cfg.WorkerID, ID: journal.ID, Revision: journal.Revision}
	if action == "held" {
		return c.call(ctx, http.MethodPost, "/worker/v1/update/held", request, output, true)
	}
	current, err := c.current(ctx)
	if err != nil {
		return err
	}
	request.SessionID = current.SessionID
	return c.call(ctx, http.MethodPost, "/worker/v1/update/"+action, request, output, true)
}
