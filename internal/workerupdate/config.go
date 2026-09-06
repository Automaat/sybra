// Package workerupdate applies leader-selected, CI-attested worker releases.
// It owns deployment mechanics only; it opens no canonical board or task store.
package workerupdate

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LeaderURL    string `yaml:"leader_url"`
	LeaderHomeID string `yaml:"leader_home_id"`
	TokenEnv     string `yaml:"token_env"`
	WorkerID     string `yaml:"worker_id"`
	Repository   string `yaml:"repository"`
	ReleaseRoot  string `yaml:"release_root"`
	CurrentLink  string `yaml:"current_link"`
	StateDir     string `yaml:"state_dir"`
	AgentConfig  string `yaml:"agent_config"`
	ServiceUser  string `yaml:"service_user"`
}

func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Config{}, errors.New("worker updater: config path must be explicit and absolute")
	}
	if err := trustedPath(path); err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, cfg.Validate()
}

var (
	repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	serviceUser    = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	homeIdentity   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func (c Config) Validate() error {
	u, err := url.Parse(c.LeaderURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("worker updater: leader_url must be an origin without credentials")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" && u.Hostname() != "localhost")) {
		return errors.New("worker updater: non-loopback leader requires HTTPS")
	}
	if !homeIdentity.MatchString(c.LeaderHomeID) || c.WorkerID == "" || c.TokenEnv == "" || os.Getenv(c.TokenEnv) == "" {
		return errors.New("worker updater: board identity, worker ID, and populated token_env are required")
	}
	if !repositoryName.MatchString(c.Repository) || !serviceUser.MatchString(c.ServiceUser) || c.ServiceUser == "root" {
		return errors.New("worker updater: trusted repository and non-root service_user are required")
	}
	for _, path := range []string{c.ReleaseRoot, c.CurrentLink, c.StateDir, c.AgentConfig} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
			return errors.New("worker updater: deployment paths must be explicit, clean, non-root absolute paths")
		}
	}
	if c.StateDir == c.ReleaseRoot || within(c.StateDir, c.ReleaseRoot) || within(c.ReleaseRoot, c.StateDir) || within(c.ReleaseRoot, c.CurrentLink) || within(c.StateDir, c.CurrentLink) {
		return errors.New("worker updater: state, releases, and current link must be separate")
	}
	return nil
}

func within(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
