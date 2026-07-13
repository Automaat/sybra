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
			name:    "env overrides config so a bad bind can always be rescued from the unit file",
			cfg:     &Config{Cluster: ClusterConfig{BindAddrs: []string{"100.64.0.2:8080"}}},
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
