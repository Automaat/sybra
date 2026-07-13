package buildcache

import (
	"fmt"
	"os"
	"strings"
)

// PrepareGoEnv ensures the task-scoped Go build cache and shared module cache
// exist, then returns env with those values injected.
func PrepareGoEnv(taskID string, env []string) ([]string, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("prepare Go cache: empty task id")
	}
	goBuild := TaskGoBuildDir(taskID)
	goMod := SharedGoModDir()
	for _, dir := range []string{goBuild, goMod} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("prepare Go cache %q: %w", dir, err)
		}
	}
	env = stripEnvKeys(env, "GOCACHE", "GOMODCACHE")
	env = append(env, "GOCACHE="+goBuild, "GOMODCACHE="+goMod)
	return env, nil
}

func stripEnvKeys(env []string, keys ...string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
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
