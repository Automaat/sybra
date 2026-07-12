package config

import (
	"fmt"
	"log/slog"
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
	Followers  []Follower `yaml:"followers,omitempty" json:"followers"`
	LocalHomes []string   `yaml:"local_homes,omitempty" json:"localHomes"`
	TLS        ClusterTLS `yaml:"tls,omitempty" json:"tls"`
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
	if len(f.Endpoints) == 0 {
		return ""
	}
	return f.Endpoints[0]
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
	if len(f.Endpoints) == 0 {
		return false
	}
	for _, ep := range f.Endpoints {
		if !endpointEncrypted(ep) {
			return false
		}
	}
	return true
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
