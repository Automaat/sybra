package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
)

const (
	ClusterRoleStandalone = "standalone"
	ClusterRoleLeader     = "leader"
	ClusterRoleFollower   = "follower"
)

// ClusterConfig configures leader-follower mode (umbrella #1803). The leader
// owns the canonical task store, polls Todoist/GitHub, and assigns work to
// followers by per-project homing; followers execute assigned tasks and stream
// state back. Default role "standalone" preserves single-node behavior, so the
// block requires zero migration. Role is "standalone", "leader", or "follower"
// (invalid falls back to standalone). BindAddr overrides a follower's
// control-plane bind. LocalHomes pins project ids to this node; TLS carries a
// follower's server cert/key for the TLS + cert-pin transport tier.
type ClusterConfig struct {
	Role       string     `yaml:"role,omitempty" json:"role"`
	BindAddr   string     `yaml:"bind_addr,omitempty" json:"bindAddr"`
	BindAddrs  []string   `yaml:"bind_addrs,omitempty" json:"bindAddrs"`
	Followers  []Follower `yaml:"followers,omitempty" json:"followers"`
	LocalHomes []string   `yaml:"local_homes,omitempty" json:"localHomes"`
	TLS        ClusterTLS `yaml:"tls,omitempty" json:"tls"`
}

// ListenAddrs returns every address the control plane should listen on, most
// significant first. Explicit env (SYBRA_HOST/SYBRA_PORT) wins so an operator
// can always override a bad config from the unit file; then bind_addrs (one
// listener per entry, sharing a single handler, for interface-level lockdown);
// then bind_addr; then the default. Never returns empty.
func (c *Config) ListenAddrs(envHost, envPort string) []string {
	envHost, envPort = strings.TrimSpace(envHost), strings.TrimSpace(envPort)
	if envHost != "" || envPort != "" {
		if envPort == "" {
			envPort = DefaultServerPort
		}
		return []string{net.JoinHostPort(envHost, envPort)}
	}
	if c != nil {
		if addrs := nonBlank(c.Cluster.BindAddrs); len(addrs) > 0 {
			return addrs
		}
		if addr := strings.TrimSpace(c.Cluster.BindAddr); addr != "" {
			return []string{addr}
		}
	}
	return []string{net.JoinHostPort("", DefaultServerPort)}
}

// ServesTLS reports whether the control plane should terminate TLS itself. This
// is the LAN tier: no CA, no ACME — the leader pins the certificate's SHA-256
// fingerprint (tls_pin) instead of validating a chain.
func (c *Config) ServesTLS() bool {
	return c != nil && strings.TrimSpace(c.Cluster.TLS.CertFile) != "" && strings.TrimSpace(c.Cluster.TLS.KeyFile) != ""
}

func nonBlank(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// ClusterTLS holds a follower's server certificate/key paths for the TLS +
// cert-pin transport tier (P7 provides cert-gen). Empty = plain HTTP.
type ClusterTLS struct {
	CertFile string `yaml:"cert_file,omitempty" json:"certFile"`
	KeyFile  string `yaml:"key_file,omitempty" json:"keyFile"`
}

// Follower is one entry in the leader's roster: a node the leader may assign
// projects to over the control plane. Name is stamped into task.AssignedNode.
// Endpoints are ordered most-preferred-first (e.g. a tailnet URL then a LAN IP)
// so tailnet flakiness falls back to LAN; failover across them lands in P1.
// AuthTokenEnv names the env var holding the bearer token so the secret never
// lands in config.yaml. Trusted marks a node cleared for work-typed tasks;
// Homes pins project ids to this follower; TLSPin is the expected SHA-256
// fingerprint of its server certificate.
type Follower struct {
	Name         string   `yaml:"name" json:"name"`
	Endpoints    []string `yaml:"endpoints" json:"endpoints"`
	AuthTokenEnv string   `yaml:"auth_token_env,omitempty" json:"authTokenEnv"`
	Trusted      bool     `yaml:"trusted,omitempty" json:"trusted"`
	Homes        []string `yaml:"homes,omitempty" json:"homes"`
	TLSPin       string   `yaml:"tls_pin,omitempty" json:"tlsPin"`
}

// NormalizeClusterRole canonicalizes a cluster role. Trim+lowercase first so a
// formatting slip ("Leader", "follower ") never silently changes posture; empty
// maps to standalone; unknown values are rejected.
func NormalizeClusterRole(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", ClusterRoleStandalone:
		return ClusterRoleStandalone, nil
	case ClusterRoleLeader:
		return ClusterRoleLeader, nil
	case ClusterRoleFollower:
		return ClusterRoleFollower, nil
	default:
		return "", fmt.Errorf("invalid cluster.role %q (valid: standalone, leader, follower)", s)
	}
}

// ClusterRole returns this node's resolved cluster role. An invalid config
// value is logged and treated as standalone so a misconfigured node never
// silently becomes a leader or follower.
func (c *Config) ClusterRole() string {
	if c == nil {
		return ClusterRoleStandalone
	}
	role, err := NormalizeClusterRole(c.Cluster.Role)
	if err != nil {
		slog.Warn("config: invalid cluster.role; falling back to standalone", "value", c.Cluster.Role)
		return ClusterRoleStandalone
	}
	return role
}

// IsLeader reports whether this node is the cluster leader.
func (c *Config) IsLeader() bool { return c.ClusterRole() == ClusterRoleLeader }

// IsFollower reports whether this node is a cluster follower. Followers
// hard-disable every inbound poller regardless of per-feature flags.
func (c *Config) IsFollower() bool { return c.ClusterRole() == ClusterRoleFollower }

// HomeNode describes the node that owns execution for a project. Name/URL/Token
// are empty for local execution; Trusted and Encrypted are true locally (no
// transport to secure); Local reports whether the project runs on this node.
type HomeNode struct {
	Name      string
	URL       string
	Token     string
	Trusted   bool
	Encrypted bool
	Local     bool
}

// LocalNodeName is the reserved node name for the leader itself. Manual
// reassignment accepts it to bring a task home when every follower is down.
const LocalNodeName = "local"

// HomeNodeForTask resolves which node owns execution for a task. An operator's
// manual override wins over the project's configured home — otherwise the
// assigner would recompute the config home on its next tick and drag the task
// straight back to the node it was evacuated from. An override naming a node
// that no longer exists in the roster falls back to the config home, so a
// removed follower cannot strand a task on a name nothing resolves.
func (c *Config) HomeNodeForTask(projectID, override string) HomeNode {
	if override == "" {
		return c.HomeNodeFor(projectID)
	}
	if home, ok := c.HomeNodeByName(override); ok {
		return home
	}
	return c.HomeNodeFor(projectID)
}

// HomeNodeByName resolves a node by roster name, for operations that target a
// node explicitly instead of following the project's configured home (manual
// reassignment). Reports ok=false for an unknown name, so a typo can never be
// mistaken for the leader and silently run work meant for a follower.
func (c *Config) HomeNodeByName(name string) (HomeNode, bool) {
	if name == LocalNodeName {
		return HomeNode{Trusted: true, Encrypted: true, Local: true}, true
	}
	if c == nil {
		return HomeNode{}, false
	}
	for i := range c.Cluster.Followers {
		f := c.Cluster.Followers[i]
		if f.Name != name {
			continue
		}
		return HomeNode{
			Name:      f.Name,
			URL:       f.PrimaryEndpoint(),
			Token:     f.ResolveToken(),
			Trusted:   f.Trusted,
			Encrypted: f.Encrypted(),
		}, true
	}
	return HomeNode{}, false
}

// HomeNodeFor resolves which node owns execution for projectID. A project in a
// follower's Homes routes to that follower; a project in LocalHomes, in no
// roster, or on a non-leader node routes local. Per-project homing means the
// first matching follower is authoritative.
func (c *Config) HomeNodeFor(projectID string) HomeNode {
	local := HomeNode{Trusted: true, Encrypted: true, Local: true}
	if c == nil || !c.IsLeader() || projectID == "" {
		return local
	}
	if slices.Contains(c.Cluster.LocalHomes, projectID) {
		return local
	}
	for i := range c.Cluster.Followers {
		f := c.Cluster.Followers[i]
		if !slices.Contains(f.Homes, projectID) {
			continue
		}
		return HomeNode{
			Name:      f.Name,
			URL:       f.PrimaryEndpoint(),
			Token:     f.ResolveToken(),
			Trusted:   f.Trusted,
			Encrypted: f.Encrypted(),
		}
	}
	return local
}

// PrimaryEndpoint returns the first configured endpoint, empty when none are
// set. Failover across the remaining endpoints is the client's job (P1).
func (f Follower) PrimaryEndpoint() string {
	for _, ep := range f.Endpoints {
		if trimmed := strings.TrimSpace(ep); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ResolveToken reads the follower's bearer token from AuthTokenEnv. Empty when
// AuthTokenEnv is unset or the named variable is not present.
func (f Follower) ResolveToken() string {
	if f.AuthTokenEnv == "" {
		return ""
	}
	return os.Getenv(f.AuthTokenEnv)
}

// Encrypted reports whether every path to this follower is confidential in
// transit. It requires at least one endpoint and that all endpoints ride an
// encrypted transport, since failover could route a work task over any of
// them. The confidentiality guard (P4) reads this to keep work tasks off
// cleartext links.
func (f Follower) Encrypted() bool {
	seen := false
	for _, ep := range f.Endpoints {
		if strings.TrimSpace(ep) == "" {
			continue
		}
		seen = true
		if !endpointEncrypted(ep) {
			return false
		}
	}
	return seen
}

func endpointEncrypted(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	return isTailscaleHost(u.Hostname())
}

func isTailscaleHost(host string) bool {
	return host != "" && strings.HasSuffix(strings.ToLower(host), ".ts.net")
}
