package config

import (
	"slices"
	"testing"
)

func TestListenAddrs(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		envHost string
		envPort string
		want    []string
	}{
		{
			name: "default binds all interfaces on 8080",
			cfg:  &Config{},
			want: []string{":8080"},
		},
		{
			name: "bind_addr is honoured",
			cfg:  &Config{Cluster: ClusterConfig{BindAddr: "100.64.0.2:8080"}},
			want: []string{"100.64.0.2:8080"},
		},
		{
			name: "bind_addrs opens one listener per interface",
			cfg:  &Config{Cluster: ClusterConfig{BindAddrs: []string{"100.64.0.2:8080", "192.168.20.9:8080"}}},
			want: []string{"100.64.0.2:8080", "192.168.20.9:8080"},
		},
		{
			name: "bind_addrs wins over bind_addr",
			cfg:  &Config{Cluster: ClusterConfig{BindAddr: "127.0.0.1:8080", BindAddrs: []string{"100.64.0.2:8080"}}},
			want: []string{"100.64.0.2:8080"},
		},
		{
			name:    "a configured bind wins over SYBRA_PORT, which the shipped unit file always sets",
			cfg:     &Config{Cluster: ClusterConfig{BindAddrs: []string{"100.64.0.2:8080"}}},
			envPort: "8080",
			want:    []string{"100.64.0.2:8080"},
		},
		{
			name:    "a configured bind wins over SYBRA_HOST too",
			cfg:     &Config{Cluster: ClusterConfig{BindAddr: "100.64.0.2:8080"}},
			envHost: "0.0.0.0",
			envPort: "9999",
			want:    []string{"100.64.0.2:8080"},
		},
		{
			name:    "env still applies when no bind is configured",
			cfg:     &Config{},
			envHost: "127.0.0.1",
			envPort: "9999",
			want:    []string{"127.0.0.1:9999"},
		},
		{
			name:    "env port alone binds all interfaces",
			cfg:     &Config{},
			envPort: "9000",
			want:    []string{":9000"},
		},
		{
			name: "blank entries are dropped rather than binding every interface by accident",
			cfg:  &Config{Cluster: ClusterConfig{BindAddrs: []string{"  ", "100.64.0.2:8080"}}},
			want: []string{"100.64.0.2:8080"},
		},
		{
			name: "nil config still yields a usable address",
			cfg:  nil,
			want: []string{":8080"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.ListenAddrs(tc.envHost, tc.envPort)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ListenAddrs() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListenAddrsBindAddrEnvOverridesConfig(t *testing.T) {
	t.Setenv("SYBRA_BIND_ADDR", "127.0.0.1:7777")
	cfg := &Config{Cluster: ClusterConfig{BindAddrs: []string{"100.64.0.2:8080"}}}
	got := cfg.ListenAddrs("", "8080")
	if !slices.Equal(got, []string{"127.0.0.1:7777"}) {
		t.Fatalf("SYBRA_BIND_ADDR is the rescue hatch and must beat config, got %v", got)
	}
}

func TestValidateCluster(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{name: "no tls block", cfg: &Config{}, wantErr: false},
		{
			name:    "both set",
			cfg:     &Config{Cluster: ClusterConfig{TLS: ClusterTLS{CertFile: "/f.crt", KeyFile: "/f.key"}}},
			wantErr: false,
		},
		{
			name:    "cert without key would come up cleartext while the leader believes it is https",
			cfg:     &Config{Cluster: ClusterConfig{TLS: ClusterTLS{CertFile: "/f.crt"}}},
			wantErr: true,
		},
		{
			name:    "key without cert",
			cfg:     &Config{Cluster: ClusterConfig{TLS: ClusterTLS{KeyFile: "/f.key"}}},
			wantErr: true,
		},
		{name: "nil config", cfg: nil, wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateCluster()
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCluster() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestServesTLS(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "no tls block", cfg: &Config{}, want: false},
		{
			name: "cert without key does not serve TLS",
			cfg:  &Config{Cluster: ClusterConfig{TLS: ClusterTLS{CertFile: "/tls/f.crt"}}},
			want: false,
		},
		{
			name: "key without cert does not serve TLS",
			cfg:  &Config{Cluster: ClusterConfig{TLS: ClusterTLS{KeyFile: "/tls/f.key"}}},
			want: false,
		},
		{
			name: "both present serves TLS",
			cfg:  &Config{Cluster: ClusterConfig{TLS: ClusterTLS{CertFile: "/tls/f.crt", KeyFile: "/tls/f.key"}}},
			want: true,
		},
		{name: "nil config", cfg: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ServesTLS(); got != tc.want {
				t.Fatalf("ServesTLS() = %v, want %v", got, tc.want)
			}
		})
	}
}
