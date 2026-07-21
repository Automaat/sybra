package workflow

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/buildcache"
)

type envtestPlan struct {
	Needed          bool
	VersionSpec     string
	SetupEnvtestRef string
}

func verifyCommandEnv(ctx context.Context, taskID, wtPath string, rawCmds ...string) ([]string, error) {
	env, err := buildcache.PrepareGoEnv(taskID, os.Environ())
	if err != nil {
		return nil, err
	}
	if envValue(env, "KUBEBUILDER_ASSETS") != "" || !commandsNeedEnvtest(rawCmds) {
		return env, nil
	}
	plan, err := discoverEnvtestPlan(wtPath)
	if err != nil || !plan.Needed {
		return env, err
	}
	assets, err := provisionEnvtestAssets(ctx, wtPath, plan, env)
	if err != nil {
		return nil, err
	}
	env = stripEnvKeysLocal(env, "KUBEBUILDER_ASSETS")
	return append(env, "KUBEBUILDER_ASSETS="+assets), nil
}

func commandsNeedEnvtest(rawCmds []string) bool {
	for _, raw := range rawCmds {
		if strings.Contains(raw, "go test") || strings.Contains(raw, "go list") {
			return true
		}
	}
	return false
}

func discoverEnvtestPlan(wtPath string) (envtestPlan, error) {
	goModPath := filepath.Join(wtPath, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return envtestPlan{}, nil
		}
		return envtestPlan{}, err
	}
	plan := envtestPlan{
		VersionSpec:     inferEnvtestVersionFromGoMod(goMod),
		SetupEnvtestRef: inferSetupEnvtestRef(goMod),
	}
	if plan.SetupEnvtestRef == "" {
		plan.SetupEnvtestRef = "latest"
	}
	err = filepath.WalkDir(wtPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !shouldInspectEnvtestFile(path, d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if explicit := inferEnvtestVersionFromText(data); explicit != "" {
			plan.VersionSpec = explicit
		}
		if bytes.Contains(data, []byte("sigs.k8s.io/controller-runtime/pkg/envtest")) ||
			bytes.Contains(data, []byte("KUBEBUILDER_ASSETS")) ||
			bytes.Contains(data, []byte("setup-envtest")) {
			plan.Needed = true
		}
		return nil
	})
	if err != nil {
		return envtestPlan{}, err
	}
	return plan, nil
}

func shouldInspectEnvtestFile(path, name string) bool {
	switch name {
	case "Makefile", "makefile", "GNUmakefile", "go.mod":
		return true
	}
	switch filepath.Ext(path) {
	case ".go", ".mk", ".sh", ".bash", ".envrc", ".yaml", ".yml":
		return true
	}
	return false
}

func inferEnvtestVersionFromText(data []byte) string {
	for raw := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if key, val, ok := strings.Cut(line, "="); ok && strings.Contains(key, "ENVTEST_K8S_VERSION") {
			if v := cleanEnvtestVersionToken(val); v != "" {
				return v
			}
		}
		if idx := strings.Index(line, "setup-envtest"); idx >= 0 {
			fields := strings.Fields(line[idx:])
			for i, field := range fields {
				if field != "use" {
					continue
				}
				for _, token := range fields[i+1:] {
					if strings.HasPrefix(token, "-") {
						continue
					}
					if v := cleanEnvtestVersionToken(token); v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}

func cleanEnvtestVersionToken(raw string) string {
	token := strings.TrimSpace(raw)
	if fields := strings.Fields(token); len(fields) > 0 {
		token = fields[0]
	}
	token = strings.Trim(token, `"'()[]{};,`)
	if token == "" {
		return ""
	}
	token = strings.TrimPrefix(token, "v")
	if !strings.HasPrefix(token, "1.") {
		return ""
	}
	for _, ch := range token {
		if (ch < '0' || ch > '9') && ch != '.' && ch != 'x' && ch != '!' {
			return ""
		}
	}
	return token
}

func inferEnvtestVersionFromGoMod(goMod []byte) string {
	for raw := range strings.SplitSeq(string(goMod), "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "k8s.io/api" || fields[0] == "k8s.io/apimachinery" || fields[0] == "k8s.io/client-go" {
			if version := k8sVersionSpecFromModule(fields[1]); version != "" {
				return version
			}
		}
	}
	return ""
}

func inferSetupEnvtestRef(goMod []byte) string {
	for raw := range strings.SplitSeq(string(goMod), "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 2 || fields[0] != "sigs.k8s.io/controller-runtime" {
			continue
		}
		version := strings.TrimPrefix(strings.TrimPrefix(fields[1], "v"), "0.")
		minor, _, ok := strings.Cut(version, ".")
		if !ok || minor == "" {
			return ""
		}
		return "release-0." + minor
	}
	return ""
}

func k8sVersionSpecFromModule(raw string) string {
	version := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if !strings.HasPrefix(version, "0.") {
		return ""
	}
	version = strings.TrimPrefix(version, "0.")
	minor, _, ok := strings.Cut(version, ".")
	if !ok || minor == "" {
		return ""
	}
	return "1." + minor + ".x!"
}

func provisionEnvtestAssets(ctx context.Context, wtPath string, plan envtestPlan, env []string) (string, error) {
	args := []string{"use", "-p", "path"}
	if plan.VersionSpec != "" {
		args = append(args, plan.VersionSpec)
	}
	var cmd *exec.Cmd
	if bin, err := exec.LookPath("setup-envtest"); err == nil {
		cmd = exec.CommandContext(ctx, bin, args...)
	} else {
		goArgs := []string{"run", "sigs.k8s.io/controller-runtime/tools/setup-envtest@" + plan.SetupEnvtestRef}
		goArgs = append(goArgs, args...)
		cmd = exec.CommandContext(ctx, "go", goArgs...)
	}
	cmd.Dir = wtPath
	cmd.Env = append(stripEnvKeysLocal(env, "XDG_DATA_HOME"), "XDG_DATA_HOME="+buildcache.SharedRoot())
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return "", fmt.Errorf("provision envtest assets: %w", err)
		}
		return "", fmt.Errorf("provision envtest assets: %w: %s", err, trimDiffLine(detail))
	}
	assets := strings.TrimSpace(string(out))
	if assets == "" {
		return "", fmt.Errorf("provision envtest assets: empty path")
	}
	return assets, nil
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func stripEnvKeysLocal(env []string, keys ...string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, key := range keys {
			if strings.HasPrefix(kv, key+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
