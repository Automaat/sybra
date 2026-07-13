package buildcache

import (
	"path/filepath"

	"github.com/Automaat/sybra/internal/config"
)

func SharedRoot() string {
	return filepath.Join(config.HomeDir(), "shared-cache")
}

func TaskGoBuildDir(taskID string) string {
	return filepath.Join(SharedRoot(), "go-build", taskID)
}

func SharedGoModDir() string {
	return filepath.Join(SharedRoot(), "go-mod")
}

func SharedNPMDir() string {
	return filepath.Join(SharedRoot(), "npm")
}
