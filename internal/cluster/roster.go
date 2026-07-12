package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Status is a follower's liveness as seen by the leader.
type Status string

const (
	StatusOnline   Status = "online"
	StatusDegraded Status = "degraded"
	StatusOffline  Status = "offline"
	StatusUnknown  Status = "unknown"
)

// NodeHealth is a point-in-time liveness snapshot for one follower. Status is
// online when the preferred endpoint answers /health, degraded when only a
// fallback endpoint answers, and offline when none do.
type NodeHealth struct {
	Name           string
	Status         Status
	ActiveEndpoint string
	LastChecked    time.Time
	LastError      string
}

// Roster holds the leader's follower Clients and their last observed health.
type Roster struct {
	logger *slog.Logger

	mu      sync.RWMutex
	order   []string
	clients map[string]*Client
	health  map[string]NodeHealth
}

// NewRoster builds a Roster of Clients from the configured follower nodes.
// Duplicate node names are rejected. A nil logger falls back to slog.Default().
func NewRoster(nodes []Node, logger *slog.Logger) (*Roster, error) {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Roster{
		logger:  logger,
		clients: make(map[string]*Client, len(nodes)),
		health:  make(map[string]NodeHealth, len(nodes)),
	}
	for _, n := range nodes {
		if n.Name == "" {
			return nil, fmt.Errorf("cluster: follower node has no name")
		}
		if _, dup := r.clients[n.Name]; dup {
			return nil, fmt.Errorf("cluster: duplicate follower node name %q", n.Name)
		}
		client, err := NewClient(n, logger)
		if err != nil {
			return nil, fmt.Errorf("cluster: node %q: %w", n.Name, err)
		}
		r.order = append(r.order, n.Name)
		r.clients[n.Name] = client
		r.health[n.Name] = NodeHealth{Name: n.Name, Status: StatusUnknown}
	}
	return r, nil
}

// Names returns the follower node names in roster order.
func (r *Roster) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Client returns the Client for the named follower.
func (r *Roster) Client(name string) (*Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.clients[name]
	return c, ok
}

// Health returns the last observed health for every follower, in roster order.
func (r *Roster) Health() []NodeHealth {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeHealth, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.health[name])
	}
	return out
}

// ProbeAll health-probes every follower concurrently and updates the roster's
// health snapshot, returning the fresh snapshots in roster order.
func (r *Roster) ProbeAll(ctx context.Context, now time.Time) []NodeHealth {
	names := r.Names()
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			r.ProbeNode(ctx, name, now)
		}(name)
	}
	wg.Wait()
	return r.Health()
}

// ProbeNode health-probes a single follower and updates its health snapshot.
func (r *Roster) ProbeNode(ctx context.Context, name string, now time.Time) NodeHealth {
	client, ok := r.Client(name)
	if !ok || client == nil {
		return NodeHealth{Name: name, Status: StatusUnknown}
	}
	endpoint, degraded, err := client.ProbeHealth(ctx)
	h := NodeHealth{Name: name, LastChecked: now}
	switch {
	case err != nil:
		h.Status = StatusOffline
		h.LastError = err.Error()
	case degraded:
		h.Status = StatusDegraded
		h.ActiveEndpoint = endpoint
	default:
		h.Status = StatusOnline
		h.ActiveEndpoint = endpoint
	}
	r.mu.Lock()
	prev := r.health[name]
	r.health[name] = h
	r.mu.Unlock()
	if prev.Status != h.Status {
		r.logger.Info("cluster.node.health", "node", name, "status", string(h.Status), "endpoint", h.ActiveEndpoint, "err", h.LastError)
	}
	return h
}

// SetHealthFromEvent lets a live SSE liveness signal update a node's status
// between /health polls: a received event lifts an offline/unknown node to
// online (a degraded node stays degraded, since an event only proves *some*
// endpoint answered, not that the preferred one recovered), and a stream drop
// marks the node offline until the next successful probe reclassifies it.
func (r *Roster) SetHealthFromEvent(name string, online bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.health[name]
	if !ok {
		return
	}
	h.LastChecked = now
	if online {
		if h.Status == StatusOffline || h.Status == StatusUnknown {
			h.Status = StatusOnline
		}
	} else {
		h.Status = StatusOffline
	}
	r.health[name] = h
}

// OnlineNames returns the names of followers currently online or degraded
// (reachable), sorted for stable output.
func (r *Roster) OnlineNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for name, h := range r.health {
		if h.Status == StatusOnline || h.Status == StatusDegraded {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
