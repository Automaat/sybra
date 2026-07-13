package sybra

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/cluster"
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
// and proxies control actions for a task executing on a remote follower. The
// browser never sees a follower's URL or token — every remote action is
// tunnelled through the leader's already-authenticated cluster client. The
// roster is nil (all methods degrade to empty/errors) on a non-leader node.
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return fn(ctx, c)
}
