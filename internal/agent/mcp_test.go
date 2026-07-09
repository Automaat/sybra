package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/worktree"
)

func TestBuildPlaywrightMCPConfig(t *testing.T) {
	t.Parallel()

	t.Run("shape", func(t *testing.T) {
		got, err := buildPlaywrightMCPConfig("/tmp/evidence", nil)
		if err != nil {
			t.Fatalf("buildPlaywrightMCPConfig: %v", err)
		}
		var parsed struct {
			McpServers struct {
				Playwright struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"playwright"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("unmarshal config: %v; raw=%s", err, got)
		}
		p := parsed.McpServers.Playwright
		if p.Command != "npx" {
			t.Errorf("command = %q, want npx", p.Command)
		}
		want := []string{"-y", "@playwright/mcp@latest", "--headless", "--output-dir", "/tmp/evidence"}
		if len(p.Args) != len(want) {
			t.Fatalf("args = %v, want %v", p.Args, want)
		}
		for i, w := range want {
			if p.Args[i] != w {
				t.Errorf("args[%d] = %q, want %q", i, p.Args[i], w)
			}
		}
	})

	t.Run("extra_args_appended", func(t *testing.T) {
		got, err := buildPlaywrightMCPConfig("/tmp/evidence", []string{"--browser", "firefox"})
		if err != nil {
			t.Fatalf("buildPlaywrightMCPConfig: %v", err)
		}
		if !jsonContainsArgs(t, got, "--browser", "firefox") {
			t.Errorf("expected extra args in config; got %s", got)
		}
	})

	t.Run("empty_output_dir_errors", func(t *testing.T) {
		if _, err := buildPlaywrightMCPConfig("", nil); err == nil {
			t.Fatal("expected error for empty output dir")
		}
	})
}

func jsonContainsArgs(t *testing.T, raw string, want ...string) bool {
	t.Helper()
	var parsed struct {
		McpServers struct {
			Playwright struct {
				Args []string `json:"args"`
			} `json:"playwright"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	args := parsed.McpServers.Playwright.Args
	for _, w := range want {
		if !slices.Contains(args, w) {
			return false
		}
	}
	return true
}

func TestPreflightPlaywrightMCP(t *testing.T) {
	t.Run("succeeds_and_excludes_evidence_dir", func(t *testing.T) {
		wt := t.TempDir()
		mustRunGit(t, wt, "init", "-b", "main")

		outputDir, err := preflightPlaywrightMCP(wt, "")
		if err != nil {
			t.Fatalf("preflightPlaywrightMCP: %v", err)
		}
		wantDir := filepath.Join(wt, worktree.EvidenceDirName)
		if outputDir != wantDir {
			t.Errorf("outputDir = %q, want %q", outputDir, wantDir)
		}
		if info, statErr := os.Stat(outputDir); statErr != nil || !info.IsDir() {
			t.Fatalf("output dir not created: %v", statErr)
		}
		if info, statErr := os.Stat(filepath.Join(outputDir, worktree.EvidenceBrowsersDirName)); statErr != nil || !info.IsDir() {
			t.Fatalf("browsers dir not created: %v", statErr)
		}
		if info, statErr := os.Stat(filepath.Join(outputDir, worktree.EvidenceNPMCacheDirName)); statErr != nil || !info.IsDir() {
			t.Fatalf("npm cache dir not created: %v", statErr)
		}

		out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("expected evidence dir excluded from git status; got:\n%s", out)
		}
	})

	t.Run("empty_worktree_dir_errors", func(t *testing.T) {
		if _, err := preflightPlaywrightMCP("", ""); err == nil {
			t.Fatal("expected error for empty worktree dir")
		}
	})

	t.Run("respects_configured_output_dir", func(t *testing.T) {
		wt := t.TempDir()
		mustRunGit(t, wt, "init", "-b", "main")
		custom := filepath.Join(wt, "custom-evidence")

		outputDir, err := preflightPlaywrightMCP(wt, custom)
		if err != nil {
			t.Fatalf("preflightPlaywrightMCP: %v", err)
		}
		if outputDir != custom {
			t.Errorf("outputDir = %q, want %q", outputDir, custom)
		}
		out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("expected configured evidence dir excluded from git status; got:\n%s", out)
		}
	})
}

func TestPreparePlaywrightMCP(t *testing.T) {
	t.Run("disabled_config_leaves_mcp_config_json_empty", func(t *testing.T) {
		m, _ := newTestManager(t)
		wt := t.TempDir()
		mustRunGit(t, wt, "init", "-b", "main")
		cfg := RunConfig{Mode: "headless", Dir: wt, PlaywrightMCPEligible: true, provider: claudeProvider{}}
		m.preparePlaywrightMCP(&cfg)
		if cfg.MCPConfigJSON != "" {
			t.Errorf("expected empty MCPConfigJSON when config disabled; got %q", cfg.MCPConfigJSON)
		}
		if slices.ContainsFunc(cfg.ExtraEnv, func(kv string) bool {
			return strings.HasPrefix(kv, "PLAYWRIGHT_BROWSERS_PATH=") || strings.HasPrefix(kv, "npm_config_cache=")
		}) {
			t.Fatalf("disabled config must not inject playwright env, got %v", cfg.ExtraEnv)
		}
	})

	t.Run("not_eligible_leaves_mcp_config_json_empty", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.playwrightMCPEnabled = true
		wt := t.TempDir()
		mustRunGit(t, wt, "init", "-b", "main")
		cfg := RunConfig{Mode: "headless", Dir: wt, provider: claudeProvider{}}
		m.preparePlaywrightMCP(&cfg)
		if cfg.MCPConfigJSON != "" {
			t.Errorf("expected empty MCPConfigJSON when not eligible; got %q", cfg.MCPConfigJSON)
		}
	})

	t.Run("non_claude_resolved_provider_leaves_mcp_config_json_empty", func(t *testing.T) {
		// Eligibility/config enabled but the FINAL resolved provider is codex
		// (e.g. this run failed over) — must not attach the claude-only flag.
		m, _ := newTestManager(t)
		m.playwrightMCPEnabled = true
		wt := t.TempDir()
		mustRunGit(t, wt, "init", "-b", "main")
		cfg := RunConfig{Mode: "headless", Dir: wt, PlaywrightMCPEligible: true, provider: codexProvider{}}
		m.preparePlaywrightMCP(&cfg)
		if cfg.MCPConfigJSON != "" {
			t.Errorf("expected empty MCPConfigJSON for non-claude resolved provider; got %q", cfg.MCPConfigJSON)
		}
		if slices.ContainsFunc(cfg.ExtraEnv, func(kv string) bool {
			return strings.HasPrefix(kv, "PLAYWRIGHT_BROWSERS_PATH=") || strings.HasPrefix(kv, "npm_config_cache=")
		}) {
			t.Fatalf("non-claude provider must not inject playwright env, got %v", cfg.ExtraEnv)
		}
	})

	t.Run("interactive_mode_leaves_mcp_config_json_empty", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.playwrightMCPEnabled = true
		wt := t.TempDir()
		mustRunGit(t, wt, "init", "-b", "main")
		cfg := RunConfig{Mode: "interactive", Dir: wt, PlaywrightMCPEligible: true, provider: claudeProvider{}}
		m.preparePlaywrightMCP(&cfg)
		if cfg.MCPConfigJSON != "" {
			t.Errorf("expected empty MCPConfigJSON for non-headless mode; got %q", cfg.MCPConfigJSON)
		}
	})

	t.Run("enabled_eligible_claude_headless_attaches_mcp_config", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.playwrightMCPEnabled = true
		wt := t.TempDir()
		mustRunGit(t, wt, "init", "-b", "main")
		cfg := RunConfig{Mode: "headless", Dir: wt, PlaywrightMCPEligible: true, provider: claudeProvider{}}
		m.preparePlaywrightMCP(&cfg)
		if cfg.MCPConfigJSON == "" {
			t.Fatal("expected MCPConfigJSON to be set")
		}
		if !jsonContainsArgs(t, cfg.MCPConfigJSON, "--output-dir", filepath.Join(wt, worktree.EvidenceDirName)) {
			t.Errorf("expected output dir in mcp config; got %s", cfg.MCPConfigJSON)
		}
		wantEnv := []string{
			"PLAYWRIGHT_BROWSERS_PATH=" + filepath.Join(wt, worktree.EvidenceDirName, worktree.EvidenceBrowsersDirName),
			"npm_config_cache=" + filepath.Join(wt, worktree.EvidenceDirName, worktree.EvidenceNPMCacheDirName),
		}
		for _, want := range wantEnv {
			if !slices.Contains(cfg.ExtraEnv, want) {
				t.Fatalf("ExtraEnv = %v, missing %q", cfg.ExtraEnv, want)
			}
		}
	})

	t.Run("preflight_failure_leaves_mcp_config_json_empty", func(t *testing.T) {
		// A non-git dir fails the evidence-dir exclude step, simulating a
		// broken launcher preflight — must not block the run, just skip MCP.
		m, _ := newTestManager(t)
		m.playwrightMCPEnabled = true
		notAGitRepo := t.TempDir()
		cfg := RunConfig{Mode: "headless", Dir: notAGitRepo, PlaywrightMCPEligible: true, provider: claudeProvider{}}
		m.preparePlaywrightMCP(&cfg)
		if cfg.MCPConfigJSON != "" {
			t.Errorf("expected empty MCPConfigJSON on preflight failure; got %q", cfg.MCPConfigJSON)
		}
	})
}

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
