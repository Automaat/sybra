// Package agentd implements the thin, outbound-only remote execution daemon.
// It intentionally depends on the execution and worker protocols, not on the
// Sybra application package or any canonical board store.
package agentd

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Automaat/sybra/internal/providerid"
	"gopkg.in/yaml.v3"
)

type Config struct {
	LeaderURL     string            `yaml:"leader_url"`
	TokenEnv      string            `yaml:"token_env"`
	NodeID        string            `yaml:"node_id,omitempty"`
	Labels        map[string]string `yaml:"labels,omitempty"`
	Capacity      int               `yaml:"capacity"`
	Providers     []string          `yaml:"providers"`
	Models        []string          `yaml:"models,omitempty"`
	SecretEnv     map[string]string `yaml:"secret_env,omitempty"`
	TrustedWork   bool              `yaml:"trusted_work,omitempty"`
	SandboxMode   string            `yaml:"sandbox_mode"`
	WorkspaceRoot string            `yaml:"workspace_root"`
	StateRoot     string            `yaml:"state_root"`
	SpoolMaxBytes int64             `yaml:"spool_max_bytes"`
	LeaseSeconds  int               `yaml:"lease_seconds,omitempty"`
	PollSeconds   int               `yaml:"poll_seconds,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("agentd config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	u, err := url.Parse(strings.TrimSpace(c.LeaderURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("agentd config: leader_url must be an http(s) origin")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return errors.New("agentd config: non-loopback leader_url must use https")
	}
	if strings.TrimSpace(c.TokenEnv) == "" || strings.TrimSpace(os.Getenv(c.TokenEnv)) == "" {
		return errors.New("agentd config: token_env must name a populated environment variable")
	}
	if c.Capacity <= 0 {
		return errors.New("agentd config: capacity must be positive")
	}
	if len(c.Providers) == 0 {
		return errors.New("agentd config: at least one provider is required")
	}
	for i, provider := range c.Providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if !providerid.IsKnown(provider) {
			return fmt.Errorf("agentd config: unknown provider %q (expected one of %s)", c.Providers[i], providerid.List())
		}
		c.Providers[i] = provider
	}
	if c.SandboxMode != "report" && c.SandboxMode != "enforce" {
		return errors.New("agentd config: sandbox_mode must be report or enforce")
	}
	if !filepath.IsAbs(c.WorkspaceRoot) || !filepath.IsAbs(c.StateRoot) {
		return errors.New("agentd config: workspace_root and state_root must be absolute")
	}
	minimumSpool := terminalEventBudgetBytes * int64(c.Capacity+1)
	if c.SpoolMaxBytes < minimumSpool {
		return fmt.Errorf("agentd config: spool_max_bytes must be at least %d for capacity %d", minimumSpool, c.Capacity)
	}
	for ref, envName := range c.SecretEnv {
		if strings.TrimSpace(ref) == "" || strings.TrimSpace(envName) == "" || strings.TrimSpace(os.Getenv(envName)) == "" {
			return fmt.Errorf("agentd config: secret_env %q must name a populated environment variable", ref)
		}
		if envName == c.TokenEnv {
			return fmt.Errorf("agentd config: leader token environment cannot be exposed as run secret %q", ref)
		}
	}
	if c.LeaseSeconds <= 0 {
		c.LeaseSeconds = 45
	}
	if c.PollSeconds <= 0 || c.PollSeconds > 25 {
		c.PollSeconds = 20
	}
	return nil
}

func (c *Config) Capabilities(build string) []string {
	values := []string{
		"protocol=1", "build=" + build, "os=" + runtime.GOOS, "arch=" + runtime.GOARCH,
		fmt.Sprintf("capacity=%d", c.Capacity), "sandbox=" + c.SandboxMode,
		fmt.Sprintf("trusted_work=%t", c.TrustedWork),
	}
	labelKeys := make([]string, 0, len(c.Labels))
	for key := range c.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	for _, key := range labelKeys {
		value := c.Labels[key]
		values = append(values, "label:"+key+"="+value)
	}
	for _, provider := range c.Providers {
		values = append(values, "provider="+provider)
		health := "unavailable"
		if _, err := exec.LookPath(providerExecutable(provider)); err == nil {
			health = "healthy"
		}
		values = append(values, "provider_health:"+provider+"="+health)
	}
	for _, model := range c.Models {
		values = append(values, "model="+model)
	}
	return values
}

func providerExecutable(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerid.Copilot:
		return "github-copilot"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}
