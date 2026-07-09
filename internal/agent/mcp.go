package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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

// preparePlaywrightMCP attaches a headless Playwright MCP server to cfg when
// every gate passes: the run is headless, the workflow dispatcher marked it
// eligible (RoleTestRunner only — see agentAdapter.StartAgent),
// config.PlaywrightMCPEnabled is true, and the FINAL resolved provider (after
// health-gate failover) is claude. Keying off cfg.provider rather than the
// raw requested provider is load-bearing: a test-runner run requested as
// claude but failed over to codex must not spawn a claude-only MCP flag on a
// codex invocation.
//
// A launcher preflight failure (evidence dir exclude, mkdir, missing npx) is
// logged and swallowed — cfg.MCPConfigJSON stays empty and the run proceeds
// exactly as it would with the feature off, so a broken preflight never blocks
// dispatch of the underlying test-runner agent.
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
	if cfg.provider == nil || cfg.provider.Name() != "claude" {
		return
	}

	outputDir, err := preflightPlaywrightMCP(cfg.Dir, cfg.PlaywrightMCPOutputDir)
	if err != nil {
		m.logger.Warn("agent.playwright_mcp.preflight_failed", "task_id", cfg.TaskID, "err", err)
		return
	}
	mcpJSON, err := buildPlaywrightMCPConfig(outputDir, extraArgs)
	if err != nil {
		m.logger.Warn("agent.playwright_mcp.config_failed", "task_id", cfg.TaskID, "err", err)
		return
	}
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
	// context.Background(): preparePlaywrightMCP runs inside
	// Manager.prepareRunConfig, which has no ctx parameter (see the sandbox
	// injection helpers alongside it).
	if err := worktree.ExcludeEvidenceDir(context.Background(), wtPath); err != nil {
		return "", fmt.Errorf("exclude evidence dir: %w", err)
	}
	if outputDir == "" {
		outputDir = filepath.Join(wtPath, worktree.EvidenceDirName)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("create evidence output dir: %w", err)
	}
	browsersDir := filepath.Join(outputDir, "browsers")
	if err := os.MkdirAll(browsersDir, 0o755); err != nil {
		return "", fmt.Errorf("create evidence browsers dir: %w", err)
	}
	if _, err := exec.LookPath("npx"); err != nil {
		return "", fmt.Errorf("npx not found: %w", err)
	}
	return outputDir, nil
}
