package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/cluster"
)

const (
	nodeCallTimeout = 30 * time.Second
	nodeListTimeout = 5 * time.Second
)

// ClusterNodeDTO is the aggregated-board view of one follower node: its roster
// name and live health (online/degraded/offline/unknown) as last observed by
// the leader.
type ClusterNodeDTO struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	ActiveEndpoint string `json:"activeEndpoint,omitempty"`
	LastError      string `json:"lastError,omitempty"`
}

// ClusterService exposes the leader's follower roster to the aggregated board
// and proxies agent reads and control actions for tasks executing on a remote
// follower. A follower's auth token never leaves the leader — every remote call
// is tunnelled through the leader's already-authenticated cluster client (the
// board does see a node's endpoint, surfaced for health diagnostics). The roster
// is nil (all methods degrade to empty/errors) on a non-leader node.
type ClusterService struct {
	logger *slog.Logger
	mu     sync.RWMutex
	roster *cluster.Roster
}

func (s *ClusterService) setRoster(r *cluster.Roster) {
	s.mu.Lock()
	s.roster = r
	s.mu.Unlock()
}

func (s *ClusterService) getRoster() *cluster.Roster {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.roster
}

// GetNodes returns the follower roster with live health for the aggregated
// board. Empty on a standalone/follower node or before any follower is
// configured.
func (s *ClusterService) GetNodes() ([]ClusterNodeDTO, error) {
	r := s.getRoster()
	if r == nil {
		return []ClusterNodeDTO{}, nil
	}
	health := r.Health()
	out := make([]ClusterNodeDTO, 0, len(health))
	for _, h := range health {
		out = append(out, ClusterNodeDTO{
			Name:           h.Name,
			Status:         string(h.Status),
			ActiveEndpoint: h.ActiveEndpoint,
			LastError:      h.LastError,
		})
	}
	return out, nil
}

// StopAgentOnNode proxies a stop request for an agent running on the named
// follower.
func (s *ClusterService) StopAgentOnNode(node, agentID string) error {
	return s.withClient(node, func(ctx context.Context, c *cluster.Client) error {
		return c.StopAgent(ctx, agentID)
	})
}

// SendMessageToNode proxies a steering message to an interactive agent on the
// named follower.
func (s *ClusterService) SendMessageToNode(node, agentID, text string) error {
	return s.withClient(node, func(ctx context.Context, c *cluster.Client) error {
		return c.SendMessage(ctx, agentID, text)
	})
}

// RespondApprovalOnNode proxies a tool-approval decision to the named follower.
func (s *ClusterService) RespondApprovalOnNode(node, toolUseID string, approved bool) error {
	return s.withClient(node, func(ctx context.Context, c *cluster.Client) error {
		return c.RespondApproval(ctx, toolUseID, approved)
	})
}

// ApprovePlanOnNode proxies a plan approval to the named follower.
func (s *ClusterService) ApprovePlanOnNode(node, taskID string) error {
	return s.withClient(node, func(ctx context.Context, c *cluster.Client) error {
		_, err := c.ApprovePlan(ctx, taskID)
		return err
	})
}

// RejectPlanOnNode proxies a plan rejection (with feedback) to the named
// follower.
func (s *ClusterService) RejectPlanOnNode(node, taskID, feedback string) error {
	return s.withClient(node, func(ctx context.Context, c *cluster.Client) error {
		_, err := c.RejectPlan(ctx, taskID, feedback)
		return err
	})
}

func (s *ClusterService) withClient(node string, fn func(context.Context, *cluster.Client) error) error {
	r := s.getRoster()
	if r == nil {
		return validationError("this node is not a cluster leader")
	}
	c, ok := r.Client(node)
	if !ok || c == nil {
		return validationError("unknown cluster node: " + node)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nodeCallTimeout)
	defer cancel()
	return relayFollowerError(node, fn(ctx, c))
}

func relayFollowerError(node string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *cluster.APIError
	if errors.As(err, &apiErr) && apiErr.IsClientError() {
		return &clientError{status: apiErr.Status, msg: apiErr.Message}
	}
	return fmt.Errorf("cluster call to node %s failed: %w", node, err)
}

// ListNodeAgents returns every follower's live agents, each stamped with the
// node it runs on. The leader's own agent manager only ever holds local agents,
// so without this the board cannot see — and therefore cannot stop, steer, or
// approve — a run executing on a follower. An unreachable node is skipped
// rather than failing the whole aggregation: one offline follower must not
// blank out the board's agent list.
func (s *ClusterService) ListNodeAgents() ([]*agent.Agent, error) {
	r := s.getRoster()
	if r == nil {
		return []*agent.Agent{}, nil
	}
	out := []*agent.Agent{}
	for _, name := range r.Names() {
		c, ok := r.Client(name)
		if !ok || c == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), nodeListTimeout)
		agents, err := c.ListAgents(ctx)
		cancel()
		if err != nil {
			s.logger.Warn("cluster.list_agents.failed", "node", name, "err", err)
			continue
		}
		for _, a := range agents {
			if a == nil {
				continue
			}
			a.Node = name
			out = append(out, a)
		}
	}
	return out, nil
}

// GetAgentOutputOnNode reads a follower agent's headless stream buffer so the
// board can render a remote run's output.
func (s *ClusterService) GetAgentOutputOnNode(node, agentID string) ([]agent.StreamEvent, error) {
	var events []agent.StreamEvent
	err := s.withClient(node, func(ctx context.Context, c *cluster.Client) error {
		var callErr error
		events, callErr = c.GetAgentOutput(ctx, agentID)
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// GetConvoOutputOnNode reads a follower agent's conversational transcript so the
// board can render a remote interactive session.
func (s *ClusterService) GetConvoOutputOnNode(node, agentID string) ([]agent.ConvoEvent, error) {
	var events []agent.ConvoEvent
	err := s.withClient(node, func(ctx context.Context, c *cluster.Client) error {
		var callErr error
		events, callErr = c.GetConvoOutput(ctx, agentID)
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}
