package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
)

// maxResponseBody bounds a single API response. Sized with real headroom
// over a large follower's full ListTasks payload (#2188: one follower's task
// store alone reached ~51MB, silently breaking reconcile under the previous
// 32MB cap with no operator-visible error — see the truncation check in do).
// A var, not a const, so tests can shrink it rather than transmit 256MB.
var maxResponseBody int64 = 256 << 20

const defaultCallTimeout = 30 * time.Second

const healthProbeTimeout = 5 * time.Second

// Node is a follower's addressable identity: an ordered endpoint list (most
// preferred first), a bearer token, and an optional cert-pin fingerprint. The
// leader builds it from a config.Follower; cluster keeps its own type so the
// transport never imports internal/config.
type Node struct {
	Name      string
	Endpoints []string
	Token     string
	TLSPin    string
}

// Client is a follower's outbound RPC client with ordered-endpoint failover.
type Client struct {
	node    Node
	http    *http.Client
	sseHTTP *http.Client
	logger  *slog.Logger

	mu     sync.Mutex
	active int
}

// APIError is a structured non-2xx response from a follower's control plane,
// decoded from the {error, code} envelope httpapi emits.
type APIError struct {
	Status  int
	Code    string
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("cluster: follower returned %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("cluster: follower returned %d: %s", e.Status, e.Message)
}

// IsClientError reports whether the follower refused the call for a reason the
// caller can act on (4xx). Deliberately not an httpapi.ClientError itself: the
// handler surfaces a ClientError's Error() verbatim, and a follower's 5xx text
// can describe follower internals. Callers relay a 4xx and sanitize the rest.
func (e *APIError) IsClientError() bool {
	return e.Status >= 400 && e.Status < 500
}

// NewClient constructs a follower Client. When node.TLSPin is set, https
// requests are verified against the pinned leaf fingerprint instead of a CA
// chain (see pinnedTransport). A nil logger falls back to slog.Default().
func NewClient(node Node, logger *slog.Logger) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var transport http.RoundTripper
	if strings.TrimSpace(node.TLSPin) != "" {
		tr, err := pinnedTransport(node.TLSPin)
		if err != nil {
			return nil, err
		}
		transport = tr
	}
	return &Client{
		node:    node,
		http:    &http.Client{Timeout: defaultCallTimeout, Transport: transport},
		sseHTTP: &http.Client{Transport: transport},
		logger:  logger,
	}, nil
}

// Name returns the node's roster name.
func (c *Client) Name() string { return c.node.Name }

// ActiveEndpoint returns the endpoint the client currently prefers, empty when
// the node has no usable endpoints.
func (c *Client) ActiveEndpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	eps := c.usableEndpoints()
	if len(eps) == 0 {
		return ""
	}
	if c.active >= len(eps) {
		c.active = 0
	}
	return eps[c.active]
}

// Call invokes service.method on the follower and is the safe default for
// callers that cannot vouch for idempotency: it fails over to another endpoint
// only when the connection was never established (so the follower never saw the
// request), never after a partial send. Read-only wrappers use callIdempotent,
// which additionally fails over on any transport error and on a gateway-down
// status (502/503/504 — a proxy fronting a dead backend). The reachable
// endpoint becomes the new preferred one.
func (c *Client) Call(ctx context.Context, service, method string, args ...any) (json.RawMessage, error) {
	return c.call(ctx, false, service, method, args...)
}

func (c *Client) callIdempotent(ctx context.Context, service, method string, args ...any) (json.RawMessage, error) {
	return c.call(ctx, true, service, method, args...)
}

func (c *Client) call(ctx context.Context, idempotent bool, service, method string, args ...any) (json.RawMessage, error) {
	body, err := encodeArgs(args)
	if err != nil {
		return nil, fmt.Errorf("cluster: encode args for %s.%s: %w", service, method, err)
	}
	eps := c.snapshotEndpoints()
	if len(eps) == 0 {
		return nil, fmt.Errorf("cluster: node %q has no usable endpoints", c.node.Name)
	}
	start := c.activeIndex(len(eps))
	var failoverErrs []error
	for i := range eps {
		idx := (start + i) % len(eps)
		raw, apiErr, transportErr := c.do(ctx, eps[idx], service, method, body)
		if transportErr != nil {
			failoverErrs = append(failoverErrs, fmt.Errorf("%s: %w", eps[idx], transportErr))
			c.logger.Debug("cluster.endpoint.unreachable", "node", c.node.Name, "endpoint", eps[idx], "err", transportErr)
			if idempotent || isConnectError(transportErr) {
				continue
			}
			return nil, fmt.Errorf("cluster: %s.%s failed on %s and is not safe to retry: %w", service, method, eps[idx], transportErr)
		}
		if apiErr != nil && idempotent && isGatewayDown(apiErr.Status) {
			failoverErrs = append(failoverErrs, fmt.Errorf("%s: %w", eps[idx], apiErr))
			c.logger.Debug("cluster.endpoint.gateway_down", "node", c.node.Name, "endpoint", eps[idx], "status", apiErr.Status)
			continue
		}
		c.setActive(idx)
		if apiErr != nil {
			return nil, apiErr
		}
		return raw, nil
	}
	return nil, fmt.Errorf("cluster: all %d endpoints failed for %s.%s: %w",
		len(eps), service, method, errors.Join(failoverErrs...))
}

func isConnectError(err error) bool {
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return netErr.Op == "dial"
	}
	return false
}

func isGatewayDown(status int) bool {
	return status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

// ProbeHealth checks the follower's GET /health across its endpoints in
// preference order. It returns the first reachable endpoint and whether that
// endpoint was a fallback (degraded: the preferred endpoint was down but a
// later one answered). An error means no endpoint responded (offline). The
// reachable endpoint becomes the new preferred one.
func (c *Client) ProbeHealth(ctx context.Context) (endpoint string, degraded bool, err error) {
	eps := c.snapshotEndpoints()
	if len(eps) == 0 {
		return "", false, fmt.Errorf("cluster: node %q has no usable endpoints", c.node.Name)
	}
	var errs []error
	for i, ep := range eps {
		pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		e := c.probeOne(pctx, ep)
		cancel()
		if e == nil {
			c.setActive(i)
			return ep, i > 0, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", ep, e))
	}
	return "", false, errors.Join(errs...)
}

func (c *Client) probeOne(ctx context.Context, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cluster: /health returned %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) do(ctx context.Context, base, service, method string, body []byte) (json.RawMessage, *APIError, error) {
	url := strings.TrimRight(base, "/") + "/api/" + service + "/" + method
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.node.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.node.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(respBody)) > maxResponseBody {
		return nil, nil, fmt.Errorf("cluster: response from %s exceeds %d byte cap (truncated)", url, maxResponseBody)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, parseAPIError(resp.StatusCode, respBody), nil
	}
	return json.RawMessage(respBody), nil, nil
}

func (c *Client) snapshotEndpoints() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usableEndpoints()
}

func (c *Client) usableEndpoints() []string {
	out := make([]string, 0, len(c.node.Endpoints))
	for _, ep := range c.node.Endpoints {
		if trimmed := strings.TrimSpace(ep); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (c *Client) activeIndex(n int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active >= n {
		c.active = 0
	}
	return c.active
}

func (c *Client) setActive(idx int) {
	c.mu.Lock()
	c.active = idx
	c.mu.Unlock()
}

func encodeArgs(args []any) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	return json.Marshal(args)
}

func parseAPIError(status int, body []byte) *APIError {
	e := &APIError{Status: status, Message: strings.TrimSpace(string(body))}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		e.Message = env.Error
		e.Code = env.Code
	}
	return e
}

// AssignTask hands a task to the follower for execution (follower RPC lands in
// P2 — TaskService.AssignTask).
func (c *Client) AssignTask(ctx context.Context, t task.Task) error {
	_, err := c.Call(ctx, "TaskService", "AssignTask", t)
	return err
}

// RecoverLostAgent asks the follower to resume a specific task after the
// leader has authoritatively detected a lost-agent anomaly for it.
func (c *Client) RecoverLostAgent(ctx context.Context, taskID string) error {
	_, err := c.Call(ctx, "TaskService", "RecoverLostAgent", taskID)
	return err
}

// GetTask fetches a task's current state from the follower.
func (c *Client) GetTask(ctx context.Context, id string) (task.Task, error) {
	raw, err := c.callIdempotent(ctx, "TaskService", "GetTask", id)
	if err != nil {
		return task.Task{}, err
	}
	return decode[task.Task](raw)
}

// ListTasks fetches all of the follower's tasks.
func (c *Client) ListTasks(ctx context.Context) ([]task.Task, error) {
	raw, err := c.callIdempotent(ctx, "TaskService", "ListTasks")
	if err != nil {
		return nil, err
	}
	return decode[[]task.Task](raw)
}

// StopAgent proxies a stop request for a running agent to the follower.
func (c *Client) StopAgent(ctx context.Context, agentID string) error {
	_, err := c.Call(ctx, "AgentService", "StopAgent", agentID)
	return err
}

// ListAgents returns the agents currently live on a follower. The leader's own
// agent manager never holds a follower's agents, so this is the only way the
// aggregated board can see — and therefore control — a remote run.
func (c *Client) ListAgents(ctx context.Context) ([]*agent.Agent, error) {
	raw, err := c.callIdempotent(ctx, "AgentService", "ListAgents")
	if err != nil {
		return nil, err
	}
	return decode[[]*agent.Agent](raw)
}

// GetAgentOutput reads a follower agent's headless stream buffer.
func (c *Client) GetAgentOutput(ctx context.Context, agentID string) ([]agent.StreamEvent, error) {
	raw, err := c.callIdempotent(ctx, "AgentService", "GetAgentOutput", agentID)
	if err != nil {
		return nil, err
	}
	return decode[[]agent.StreamEvent](raw)
}

// GetConvoOutput reads a follower agent's conversational transcript.
func (c *Client) GetConvoOutput(ctx context.Context, agentID string) ([]agent.ConvoEvent, error) {
	raw, err := c.callIdempotent(ctx, "AgentService", "GetConvoOutput", agentID)
	if err != nil {
		return nil, err
	}
	return decode[[]agent.ConvoEvent](raw)
}

// SendMessage proxies a steering message to a follower's interactive agent.
func (c *Client) SendMessage(ctx context.Context, agentID, text string) error {
	_, err := c.Call(ctx, "AgentService", "SendMessage", agentID, text)
	return err
}

// RespondApproval proxies a tool-approval decision to the follower.
func (c *Client) RespondApproval(ctx context.Context, toolUseID string, approved bool) error {
	_, err := c.Call(ctx, "AgentService", "RespondApproval", toolUseID, approved)
	return err
}

// ApprovePlan proxies a plan approval to the follower and returns the updated
// task.
func (c *Client) ApprovePlan(ctx context.Context, id string) (task.Task, error) {
	raw, err := c.Call(ctx, "PlanningService", "ApprovePlan", id)
	if err != nil {
		return task.Task{}, err
	}
	return decode[task.Task](raw)
}

// RejectPlan proxies a plan rejection (with feedback) to the follower and
// returns the updated task.
func (c *Client) RejectPlan(ctx context.Context, id, feedback string) (task.Task, error) {
	raw, err := c.Call(ctx, "PlanningService", "RejectPlan", id, feedback)
	if err != nil {
		return task.Task{}, err
	}
	return decode[task.Task](raw)
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("cluster: decode response: %w", err)
	}
	return v, nil
}
