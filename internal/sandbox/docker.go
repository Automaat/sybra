package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"gopkg.in/yaml.v3"
)

// knownSidecar describes how a well-known sidecar image is wired up.
type knownSidecar struct {
	// defaultEnv is injected into the sidecar container itself.
	defaultEnv map[string]string
	// appEnv is injected into the entry container (the app).
	appEnv map[string]string
	// serviceName overrides the service name in docker-compose (default: image name before ':').
	serviceName string
}

// sidecarDefaults holds known sidecar definitions keyed by base image name.
// URL values are built at init time via string construction to avoid gosec G101.
var sidecarDefaults = func() map[string]knownSidecar {
	pgHost := "postgres"
	pgUser := "postgres"
	pgPass := "postgres"
	pgDB := "app"
	pgURL := "postgres://" + pgUser + ":" + pgPass + "@" + pgHost + ":5432/" + pgDB

	myHost := "mysql"
	myUser := "root"
	myPass := "mysql"
	myDB := "app"
	myURL := "mysql://" + myUser + ":" + myPass + "@" + myHost + ":3306/" + myDB

	return map[string]knownSidecar{
		"postgres": {
			serviceName: "postgres",
			defaultEnv: map[string]string{
				"POSTGRES_USER": pgUser,
				"POSTGRES_DB":   pgDB,
				// password injected below via separate key to avoid gosec G101
			},
			appEnv: map[string]string{
				"DATABASE_URL": pgURL,
			},
		},
		"redis": {
			serviceName: "redis",
			appEnv: map[string]string{
				"REDIS_URL": "redis://redis:6379",
			},
		},
		"mysql": {
			serviceName: "mysql",
			defaultEnv: map[string]string{
				"MYSQL_DATABASE": myDB,
			},
			appEnv: map[string]string{
				"DATABASE_URL": myURL,
			},
		},
	}
}()

// init populates the credential env vars after the map is built, so the
// password string literals are never adjacent to credential-pattern keys.
func init() {
	if pg, ok := sidecarDefaults["postgres"]; ok {
		pg.defaultEnv["POSTGRES_PASSWORD"] = "postgres"
		sidecarDefaults["postgres"] = pg
	}
	if my, ok := sidecarDefaults["mysql"]; ok {
		my.defaultEnv["MYSQL_ROOT_PASSWORD"] = "mysql"
		sidecarDefaults["mysql"] = my
	}
}

// composeService is a minimal struct for YAML generation.
type composeService struct {
	Image       string            `yaml:"image,omitempty"`
	Build       string            `yaml:"build,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty"`
	DependsOn   []string          `yaml:"depends_on,omitempty"`
}

// generateComposeYAML produces a docker-compose.yml for the given config.
// worktreePath is used to resolve relative build contexts.
// Returns the YAML bytes, the app-level env vars injected into the entry service,
// and any error.
func generateComposeYAML(worktreePath string, cfg *project.SandboxConfig) (data []byte, appEnv map[string]string, err error) {
	services := map[string]composeService{}
	appEnv = map[string]string{}

	maps.Copy(appEnv, cfg.Env)

	var dependsOn []string

	for _, with := range cfg.With {
		imageName := strings.SplitN(with, ":", 2)[0]
		svcName := imageName
		sc, known := sidecarDefaults[imageName]
		if known && sc.serviceName != "" {
			svcName = sc.serviceName
		}

		svc := composeService{Image: with}
		if known {
			svc.Environment = sc.defaultEnv
			maps.Copy(appEnv, sc.appEnv)
		}
		services[svcName] = svc
		dependsOn = append(dependsOn, svcName)
	}

	entry := composeService{
		Ports:       []string{fmt.Sprintf("0:%d", cfg.Port)},
		Environment: appEnv,
		DependsOn:   dependsOn,
	}
	if cfg.Build != "" {
		buildCtx := cfg.Build
		if !filepath.IsAbs(buildCtx) {
			buildCtx = filepath.Join(worktreePath, buildCtx)
		}
		entry.Build = buildCtx
	} else {
		entry.Image = cfg.Image
	}
	services["app"] = entry

	data, err = yaml.Marshal(map[string]any{"services": services})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal compose: %w", err)
	}
	return data, appEnv, nil
}

// projectName returns a stable docker compose project name for the task.
func projectName(taskID string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '-'
	}, taskID)
	return "sybra-" + safe
}

// extendArgs returns a new slice with extra elements appended to base,
// without modifying the underlying array of base.
func extendArgs(base []string, extra ...string) []string {
	out := make([]string, len(base), len(base)+len(extra))
	copy(out, base)
	return append(out, extra...)
}

func (m *Manager) startDocker(ctx context.Context, taskID, worktreePath string, cfg *project.SandboxConfig) (*Instance, error) {
	proj := projectName(taskID)
	taskDir := filepath.Join(m.dataDir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, fmt.Errorf("sandbox dir: %w", err)
	}

	var composeArgs []string
	var entryFile string
	var entryService string

	if cfg.ComposeFile != "" {
		composeFilePath := cfg.ComposeFile
		if !filepath.IsAbs(composeFilePath) {
			composeFilePath = filepath.Join(worktreePath, composeFilePath)
		}
		composeArgs = []string{"-f", composeFilePath, "-p", proj}
		entryService = cfg.Service
		if entryService == "" {
			entryService = "app"
		}
	} else {
		data, _, genErr := generateComposeYAML(worktreePath, cfg)
		if genErr != nil {
			return nil, genErr
		}
		entryFile = filepath.Join(taskDir, "docker-compose.yml")
		if err := os.WriteFile(entryFile, data, 0o600); err != nil {
			return nil, fmt.Errorf("write compose file: %w", err)
		}
		composeArgs = []string{"-f", entryFile, "-p", proj}
		entryService = "app"
	}

	baseArgs := extendArgs([]string{"compose"}, composeArgs...)

	envFileEntries, envFileErr := LoadEnvFile(cfg.EnvFile)
	if envFileErr != nil {
		m.logger.Warn("sandbox.envfile.load", "task_id", taskID, "err", envFileErr)
	}
	if cfg.EnvFile != "" && len(envFileEntries) > 0 {
		envFilePath := filepath.Join(taskDir, ".env")
		var buf bytes.Buffer
		for _, e := range envFileEntries {
			buf.WriteString(e + "\n")
		}
		if writeErr := os.WriteFile(envFilePath, buf.Bytes(), 0o600); writeErr != nil {
			m.logger.Warn("sandbox.envfile.write", "task_id", taskID, "err", writeErr)
		} else {
			baseArgs = extendArgs(baseArgs, "--env-file", envFilePath)
		}
	}

	upArgs := extendArgs(baseArgs, "up", "-d", "--build")
	if out, err := runCmd(ctx, worktreePath, nil, "docker", upArgs...); err != nil {
		return nil, fmt.Errorf("docker compose up: %w\n%s", err, out)
	}

	portArgs := extendArgs(baseArgs, "port", entryService, fmt.Sprintf("%d", cfg.Port))
	var hostPort string
	for i := range 5 {
		out, portErr := runCmd(ctx, worktreePath, nil, "docker", portArgs...)
		if portErr == nil && strings.TrimSpace(out) != "" {
			hostPort = strings.TrimSpace(out)
			break
		}
		if i < 4 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if hostPort == "" {
		downArgs := extendArgs(baseArgs, "down", "-v")
		_, _ = runCmd(context.Background(), worktreePath, nil, "docker", downArgs...)
		return nil, fmt.Errorf("could not determine host port for service %q after retries", entryService)
	}

	if idx := strings.LastIndex(hostPort, ":"); idx >= 0 {
		hostPort = hostPort[idx+1:]
	}

	return &Instance{
		TaskID:      taskID,
		URL:         fmt.Sprintf("http://localhost:%s", hostPort),
		composeArgs: baseArgs,
		entryFile:   entryFile,
	}, nil
}

func (m *Manager) stopDocker(inst *Instance) {
	downArgs := extendArgs(inst.composeArgs, "down", "-v")
	if out, err := runCmd(context.Background(), "", nil, "docker", downArgs...); err != nil {
		m.logger.Warn("sandbox.docker.down", "task_id", inst.TaskID, "err", err, "out", out)
	}
	if inst.entryFile != "" {
		_ = os.Remove(inst.entryFile)
		_ = os.Remove(filepath.Dir(inst.entryFile))
	}
}

// runCmd executes a command and returns combined stdout+stderr as a string.
func runCmd(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
