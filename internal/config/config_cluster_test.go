package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeClusterRole(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", ClusterRoleStandalone, false},
		{"standalone", ClusterRoleStandalone, false},
		{"leader", ClusterRoleLeader, false},
		{"follower", ClusterRoleFollower, false},
		{"  Leader ", ClusterRoleLeader, false},
		{"FOLLOWER", ClusterRoleFollower, false},
		{"boss", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeClusterRole(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeClusterRole(%q) err = nil, want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeClusterRole(%q) err = %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizeClusterRole(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClusterRoleAndPredicates(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.ClusterRole(); got != ClusterRoleStandalone {
		t.Errorf("nil cfg ClusterRole = %q, want standalone", got)
	}
	if nilCfg.IsLeader() || nilCfg.IsFollower() {
		t.Error("nil cfg must be neither leader nor follower")
	}

	leader := &Config{Cluster: ClusterConfig{Role: "leader"}}
	if !leader.IsLeader() || leader.IsFollower() {
		t.Error("leader config predicates wrong")
	}

	follower := &Config{Cluster: ClusterConfig{Role: "FOLLOWER"}}
	if !follower.IsFollower() || follower.IsLeader() {
		t.Error("follower config predicates wrong (case-insensitive)")
	}

	invalid := &Config{Cluster: ClusterConfig{Role: "nonsense"}}
	if invalid.ClusterRole() != ClusterRoleStandalone {
		t.Error("invalid role must fall back to standalone")
	}
	if invalid.IsLeader() || invalid.IsFollower() {
		t.Error("invalid role must be neither leader nor follower")
	}
}

func TestFollowerEncrypted(t *testing.T) {
	cases := []struct {
		name string
		f    Follower
		want bool
	}{
		{"no endpoints", Follower{}, false},
		{"https", Follower{Endpoints: []string{"https://box.example:8443"}}, true},
		{"tailnet magicdns", Follower{Endpoints: []string{"http://server.tailnet-1234.ts.net:8080"}}, true},
		{"tailnet cgnat ip", Follower{Endpoints: []string{"http://100.101.102.103:8080"}}, true},
		{"plain lan http", Follower{Endpoints: []string{"http://192.168.20.219:8080"}}, false},
		{"mixed https+plain", Follower{Endpoints: []string{"https://box.ts.net", "http://192.168.20.219:8080"}}, false},
		{"both encrypted", Follower{Endpoints: []string{"https://box.example", "http://100.64.0.1:8080"}}, true},
		{"garbage", Follower{Endpoints: []string{"::not a url"}}, false},
	}
	for _, c := range cases {
		if got := c.f.Encrypted(); got != c.want {
			t.Errorf("%s: Encrypted() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFollowerResolveToken(t *testing.T) {
	if got := (Follower{}).ResolveToken(); got != "" {
		t.Errorf("empty AuthTokenEnv ResolveToken = %q, want empty", got)
	}
	t.Setenv("SYBRA_TEST_FOLLOWER_TOKEN", "secret-123")
	f := Follower{AuthTokenEnv: "SYBRA_TEST_FOLLOWER_TOKEN"}
	if got := f.ResolveToken(); got != "secret-123" {
		t.Errorf("ResolveToken = %q, want secret-123", got)
	}
	missing := Follower{AuthTokenEnv: "SYBRA_TEST_FOLLOWER_TOKEN_UNSET"}
	if got := missing.ResolveToken(); got != "" {
		t.Errorf("unset env ResolveToken = %q, want empty", got)
	}
}

func TestHomeNodeForRouting(t *testing.T) {
	t.Setenv("SYBRA_TEST_FOLLOWER_TOKEN", "tok")
	leader := &Config{Cluster: ClusterConfig{
		Role:       "leader",
		LocalHomes: []string{"owner/keep-local"},
		Followers: []Follower{
			{
				Name:         "pet-box",
				Endpoints:    []string{"https://pet.ts.net:8443"},
				AuthTokenEnv: "SYBRA_TEST_FOLLOWER_TOKEN",
				Trusted:      false,
				Homes:        []string{"owner/pet-repo"},
			},
			{
				Name:      "work-box",
				Endpoints: []string{"http://192.168.20.219:8080"},
				Trusted:   true,
				Homes:     []string{"owner/work-repo"},
			},
		},
	}}

	remote := leader.HomeNodeFor("owner/pet-repo")
	if remote.Local || remote.Name != "pet-box" {
		t.Fatalf("pet-repo should route to pet-box, got %+v", remote)
	}
	if remote.URL != "https://pet.ts.net:8443" || remote.Token != "tok" {
		t.Errorf("pet-box endpoint/token wrong: %+v", remote)
	}
	if remote.Trusted || !remote.Encrypted {
		t.Errorf("pet-box should be untrusted + encrypted, got trusted=%v encrypted=%v", remote.Trusted, remote.Encrypted)
	}

	work := leader.HomeNodeFor("owner/work-repo")
	if !work.Trusted || work.Encrypted {
		t.Errorf("work-box should be trusted + unencrypted (plain LAN http), got %+v", work)
	}

	local := leader.HomeNodeFor("owner/keep-local")
	if !local.Local || !local.Trusted || !local.Encrypted {
		t.Errorf("LocalHomes project must route local (trusted+encrypted): %+v", local)
	}

	unknown := leader.HomeNodeFor("owner/never-seen")
	if !unknown.Local {
		t.Errorf("unrostered project must route local, got %+v", unknown)
	}

	empty := leader.HomeNodeFor("")
	if !empty.Local {
		t.Errorf("empty projectID must route local, got %+v", empty)
	}

	follower := &Config{Cluster: ClusterConfig{Role: "follower", Followers: leader.Cluster.Followers}}
	if got := follower.HomeNodeFor("owner/pet-repo"); !got.Local {
		t.Errorf("non-leader node must always route local, got %+v", got)
	}
}

func TestLoadClusterBlockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("cluster:\n" +
		"  role: leader\n" +
		"  bind_addr: \"0.0.0.0:8080\"\n" +
		"  local_homes: [owner/local]\n" +
		"  followers:\n" +
		"    - name: pet-box\n" +
		"      endpoints: [https://pet.ts.net:8443, http://100.64.0.9:8080]\n" +
		"      auth_token_env: PET_TOKEN\n" +
		"      trusted: false\n" +
		"      homes: [owner/pet]\n" +
		"      tls_pin: abc123\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsLeader() {
		t.Fatal("role leader did not load")
	}
	if cfg.Cluster.BindAddr != "0.0.0.0:8080" {
		t.Errorf("bind_addr = %q", cfg.Cluster.BindAddr)
	}
	if len(cfg.Cluster.Followers) != 1 {
		t.Fatalf("followers len = %d", len(cfg.Cluster.Followers))
	}
	f := cfg.Cluster.Followers[0]
	if f.Name != "pet-box" || f.AuthTokenEnv != "PET_TOKEN" || f.TLSPin != "abc123" {
		t.Errorf("follower fields wrong: %+v", f)
	}
	if len(f.Endpoints) != 2 || len(f.Homes) != 1 {
		t.Errorf("follower slices wrong: %+v", f)
	}
}

func TestDefaultConfigClusterStandalone(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Cluster.Role != ClusterRoleStandalone {
		t.Errorf("DefaultConfig cluster role = %q, want standalone", cfg.Cluster.Role)
	}
	if cfg.IsLeader() || cfg.IsFollower() {
		t.Error("default config must be standalone (neither leader nor follower)")
	}
}

func TestLoadOmittedClusterDefaultsStandalone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("logging:\n  level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClusterRole() != ClusterRoleStandalone {
		t.Errorf("omitted cluster block must default standalone, got %q", cfg.ClusterRole())
	}
}
