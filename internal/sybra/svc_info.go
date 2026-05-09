package sybra

import (
	"encoding/json"
	"os/exec"
	"sync"

	"github.com/Automaat/sybra/internal/version"
)

// VersionInfo holds version strings for the server and client.
type VersionInfo struct {
	Server string `json:"server"`
}

// CodexModel is a single entry from `codex debug models`.
type CodexModel struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// InfoService exposes build metadata to the frontend.
type InfoService struct {
	once        sync.Once
	codexModels []CodexModel
}

// GetVersion returns version information for the running server binary.
func (s *InfoService) GetVersion() VersionInfo {
	return VersionInfo{Server: version.Version}
}

// GetCodexModels runs `codex debug models` once per session and returns the
// parsed model catalog. Returns an empty slice when codex is unavailable or
// the output cannot be parsed — callers should fall back to a built-in list.
func (s *InfoService) GetCodexModels() []CodexModel {
	s.once.Do(func() {
		s.codexModels = fetchCodexModels()
	})
	return s.codexModels
}

func fetchCodexModels() []CodexModel {
	out, err := exec.Command("codex", "debug", "models").Output()
	if err != nil {
		return nil
	}
	var payload struct {
		Models []CodexModel `json:"models"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	return payload.Models
}
