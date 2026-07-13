package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/worktree"
)

const (
	mcpOwnerFlagEnv  = "SYBRA_MCP_OWNER"
	mcpAgentIDEnv    = "SYBRA_MCP_AGENT_ID"
	mcpTaskIDEnv     = "SYBRA_MCP_TASK_ID"
	mcpAgentModeEnv  = "SYBRA_MCP_AGENT_MODE"
	mcpOwnerFlagTrue = "1"
)

type mcpOwner struct {
	AgentID string
	TaskID  string
	Mode    string
}

// buildPlaywrightMCPConfig returns the Claude --mcp-config JSON payload for a
// headless Playwright MCP server writing screenshots/console logs to
// outputDir. Pure/no I/O — callers must ensure outputDir already exists.
func buildPlaywrightMCPConfig(outputDir string, extraArgs []string) (string, error) {
	if outputDir == "" {
		return "", fmt.Errorf("agent: playwright mcp: empty output dir")
	}
	args := []string{"-y", "@playwright/mcp@latest", "--headless", "--output-dir", outputDir}
	args = append(args, extraArgs...)
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{
				"command": "npx",
				"args":    args,
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("agent: playwright mcp: marshal config: %w", err)
	}
	return string(data), nil
}

func mcpOwnerForAgent(a *Agent) mcpOwner {
	if a == nil {
		return mcpOwner{}
	}
	return mcpOwner{
		AgentID: strings.TrimSpace(a.ID),
		TaskID:  strings.TrimSpace(a.TaskID),
		Mode:    strings.TrimSpace(a.Mode),
	}
}

func wrapMCPConfigWithOwnership(mcpJSON string, owner mcpOwner) (string, error) {
	if strings.TrimSpace(mcpJSON) == "" {
		return "", nil
	}
	assignments := mcpOwnerAssignments(owner)
	if len(assignments) == 0 {
		return mcpJSON, nil
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpJSON), &doc); err != nil {
		return "", fmt.Errorf("agent: wrap mcp ownership: %w", err)
	}
	for name, raw := range doc.MCPServers {
		server, err := wrapMCPServerOwnership(raw, assignments)
		if err != nil {
			return "", fmt.Errorf("agent: wrap mcp server %q: %w", name, err)
		}
		doc.MCPServers[name] = server
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("agent: marshal wrapped mcp config: %w", err)
	}
	return string(data), nil
}

func wrapMCPServerOwnership(raw json.RawMessage, assignments []string) (json.RawMessage, error) {
	var server map[string]any
	if err := json.Unmarshal(raw, &server); err != nil {
		return nil, err
	}
	command, _ := server["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("missing command")
	}
	args, err := stringSliceFromJSON(server["args"])
	if err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	wrapped := make([]string, 0, len(assignments)+1+len(args))
	wrapped = append(wrapped, assignments...)
	wrapped = append(wrapped, command)
	wrapped = append(wrapped, args...)
	server["command"] = "env"
	server["args"] = wrapped
	data, err := json.Marshal(server)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func mcpOwnerAssignments(owner mcpOwner) []string {
	if owner.AgentID == "" || owner.Mode == "" {
		return nil
	}
	assignments := []string{
		mcpOwnerFlagEnv + "=" + mcpOwnerFlagTrue,
		mcpAgentIDEnv + "=" + owner.AgentID,
		mcpAgentModeEnv + "=" + owner.Mode,
	}
	if owner.TaskID != "" {
		assignments = append(assignments, mcpTaskIDEnv+"="+owner.TaskID)
	}
	return assignments
}

func mcpOwnerFromEnvAssignments(assignments []string) mcpOwner {
	owner := mcpOwner{}
	marked := false
	for _, assignment := range assignments {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok {
			continue
		}
		switch key {
		case mcpOwnerFlagEnv:
			if value != mcpOwnerFlagTrue {
				return mcpOwner{}
			}
			marked = true
		case mcpAgentIDEnv:
			owner.AgentID = value
		case mcpTaskIDEnv:
			owner.TaskID = value
		case mcpAgentModeEnv:
			owner.Mode = value
		}
	}
	if !marked || owner.AgentID == "" || owner.Mode == "" {
		return mcpOwner{}
	}
	return owner
}

func stringSliceFromJSON(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("want []string-compatible value, got %T", v)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("want string arg, got %T", item)
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *Manager) preparePlaywrightMCP(cfg *RunConfig) {
	if cfg.Mode != "headless" || !cfg.PlaywrightMCPEligible {
		return
	}
	m.mu.RLock()
	enabled := m.playwrightMCPEnabled
	extraArgs := m.playwrightMCPExtraArgs
	m.mu.RUnlock()
	if !enabled {
		return
	}
	if cfg.provider == nil {
		return
	}

	outputDir, err := preflightPlaywrightMCP(cfg.Dir, cfg.PlaywrightMCPOutputDir)
	if err != nil {
		m.logger.Warn("agent.playwright_mcp.preflight_failed", "task_id", cfg.TaskID, "err", err)
		return
	}
	browsersDir := filepath.Join(outputDir, worktree.EvidenceBrowsersDirName)
	npmCacheDir := filepath.Join(outputDir, worktree.EvidenceNPMCacheDirName)
	mcpJSON, err := buildPlaywrightMCPConfig(outputDir, extraArgs)
	if err != nil {
		m.logger.Warn("agent.playwright_mcp.config_failed", "task_id", cfg.TaskID, "err", err)
		return
	}
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "PLAYWRIGHT_BROWSERS_PATH", "npm_config_cache")
	cfg.ExtraEnv = append(cfg.ExtraEnv,
		"PLAYWRIGHT_BROWSERS_PATH="+browsersDir,
		"npm_config_cache="+npmCacheDir,
	)
	cfg.MCPConfigJSON = mcpJSON
	m.logger.Info("agent.playwright_mcp.enabled", "task_id", cfg.TaskID, "output_dir", outputDir)
}

// preflightPlaywrightMCP git-excludes the evidence dir and creates the
// output/browsers directories the Playwright MCP server and its downloaded
// browsers write to, both under the worktree — an allowed write root for
// every OS-level process-sandbox posture (see injectProcessSandbox) — and
// checks npx is resolvable so the launcher doesn't fail on Claude's
// strict-mcp-config startup check. Returns the resolved output dir.
func preflightPlaywrightMCP(wtPath, outputDir string) (string, error) {
	if wtPath == "" {
		return "", fmt.Errorf("no worktree dir")
	}
	if outputDir == "" {
		outputDir = filepath.Join(wtPath, worktree.EvidenceDirName)
	}
	relOutputDir, err := validateEvidenceOutputDir(wtPath, outputDir)
	if err != nil {
		return "", err
	}
	// context.Background(): preparePlaywrightMCP runs inside
	// Manager.prepareRunConfig, which has no ctx parameter (see the sandbox
	// injection helpers alongside it).
	if err := worktree.ExcludeWorktreePath(context.Background(), wtPath, relOutputDir); err != nil {
		return "", fmt.Errorf("exclude evidence dir: %w", err)
	}
	if err := clearEvidenceOutputDir(outputDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("create evidence output dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(outputDir, worktree.EvidenceBrowsersDirName), 0o755); err != nil {
		return "", fmt.Errorf("create evidence browsers dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(outputDir, worktree.EvidenceNPMCacheDirName), 0o755); err != nil {
		return "", fmt.Errorf("create evidence npm cache dir: %w", err)
	}
	if _, err := exec.LookPath("npx"); err != nil {
		return "", fmt.Errorf("npx not found: %w", err)
	}
	return outputDir, nil
}

func validateEvidenceOutputDir(wtPath, outputDir string) (string, error) {
	rel, err := filepath.Rel(wtPath, outputDir)
	if err != nil {
		return "", fmt.Errorf("resolve evidence output dir: %w", err)
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("evidence output dir %q must be inside worktree %q", outputDir, wtPath)
	}
	return filepath.Clean(rel), nil
}

func clearEvidenceOutputDir(outputDir string) error {
	if err := os.RemoveAll(outputDir); err != nil {
		return fmt.Errorf("clear evidence output dir: %w", err)
	}
	return nil
}
