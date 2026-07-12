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
