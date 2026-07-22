package workflow

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/buildcache"
)

type envtestPlan struct {
	Needed          bool
	VersionSpec     string
	SetupEnvtestRef string
}

const maxEnvtestInspectFileSize = 1 * 1024 * 1024 // 1 MiB

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
	root, err := os.OpenRoot(wtPath)
	if err != nil {
		return envtestPlan{}, err
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !shouldInspectEnvtestFile(path, name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxEnvtestInspectFileSize {
			return nil
		}
		data, err := fs.ReadFile(root.FS(), path)
		if err != nil {
			return err
		}
		if explicit := inferEnvtestVersionFromText(data); explicit != "" {
			plan.VersionSpec = explicit
		}
		if fileImportsEnvtest(path, data) {
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

func fileImportsEnvtest(path string, data []byte) bool {
	if filepath.Ext(path) != ".go" {
		return false
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, data, parser.ImportsOnly)
	if err != nil {
		return false
	}
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if importPath == "sigs.k8s.io/controller-runtime/pkg/envtest" {
			return true
		}
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
	versionToken := strings.TrimSpace(raw)
	if fields := strings.Fields(versionToken); len(fields) > 0 {
		versionToken = fields[0]
	}
	versionToken = strings.Trim(versionToken, `"'()[]{};,`)
	if versionToken == "" {
		return ""
	}
	versionToken = strings.TrimPrefix(versionToken, "v")
	if !strings.HasPrefix(versionToken, "1.") {
		return ""
	}
	for _, ch := range versionToken {
		if (ch < '0' || ch > '9') && ch != '.' && ch != 'x' && ch != '!' {
			return ""
		}
	}
	return versionToken
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
		if value, ok := strings.CutPrefix(kv, prefix); ok {
			return value
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
