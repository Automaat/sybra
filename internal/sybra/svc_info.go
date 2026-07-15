package sybra

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"sync"

	"github.com/Automaat/sybra/internal/version"
)

// VersionInfo holds version strings for the server and client.
type VersionInfo struct {
	Server string `json:"server"`
}

// CodexModel is a single entry from `codex debug models`.
type CodexModel struct {
	Slug                     string   `json:"slug"`
	DisplayName              string   `json:"display_name"`
	SupportedReasoningLevels []string `json:"supported_reasoning_levels,omitempty"`
}

// CopilotModel is a single Copilot model option (slug + display name).
type CopilotModel struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// InfoService exposes build metadata to the frontend.
type InfoService struct {
	once              sync.Once
	codexModels       []CodexModel
	runtimeOnce       sync.Once
	availableRuntimes []RuntimeInfo
	detectRuntimes    func() []RuntimeInfo
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

// GetAvailableRuntimes returns the cached PATH snapshot for known AI runtimes.
// The snapshot is warmed eagerly during app startup and lazily guarded here so
// zero-value InfoService instances stay usable in tests.
func (s *InfoService) GetAvailableRuntimes() []RuntimeInfo {
	s.primeRuntimeSnapshot()
	return slices.Clone(s.availableRuntimes)
}

func fetchCodexModels() []CodexModel {
	out, err := exec.CommandContext(context.Background(), "codex", "debug", "models").Output()
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

// GetCopilotModels returns the curated Copilot model catalog. Copilot CLI has
// no machine-readable `models` subcommand (unlike `codex debug models`), so the
// list is a fixed selection of the latest model from each vendor available in
// the binary's registry. The first entry is the default (latest GPT). "auto"
// lets Copilot pick a model itself — a safe fallback when a specific slug is
// unavailable on the user's plan.
func (s *InfoService) GetCopilotModels() []CopilotModel {
	return []CopilotModel{
		{Slug: "", DisplayName: "Default (GPT-5.5)"},
		{Slug: "gpt-5.5", DisplayName: "GPT-5.5"},
		{Slug: "gpt-5.4", DisplayName: "GPT-5.4"},
		{Slug: "gpt-5.4-mini", DisplayName: "GPT-5.4 Mini"},
		{Slug: "gpt-5.3-codex", DisplayName: "GPT-5.3 Codex"},
		{Slug: "claude-opus-4.6", DisplayName: "Claude Opus 4.6"},
		{Slug: "claude-sonnet-4.6", DisplayName: "Claude Sonnet 4.6"},
		{Slug: "claude-haiku-4.5", DisplayName: "Claude Haiku 4.5"},
		{Slug: "gemini-3.1-pro-preview", DisplayName: "Gemini 3.1 Pro"},
		{Slug: "auto", DisplayName: "Auto"},
	}
}
