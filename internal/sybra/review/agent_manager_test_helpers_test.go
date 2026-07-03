package review

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/project"
)

func newTestAgentManager(tb testing.TB, ctx context.Context, emit agent.EmitFunc, logger *slog.Logger, logDir string, cfgs ...agent.ManagerConfig) *agent.Manager {
	tb.Helper()
	cfg := agent.ManagerConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	m, err := agent.NewManager(ctx, emit, logger, logDir, cfg)
	if err != nil {
		tb.Fatalf("NewManager: %v", err)
	}
	return m
}

// mustWriteProjectYAML writes a minimal project record straight to the
// project.Store directory. Bypasses Store.Create (which performs a real git
// clone) so the test can stage work/pet entries without network access.
func mustWriteProjectYAML(t *testing.T, dir, id string, ptype project.ProjectType) {
	t.Helper()
	// project.Store.filePath maps "owner/repo" → "owner--repo.yaml".
	safe := id
	for i := 0; i < len(safe); i++ {
		if safe[i] == '/' {
			safe = safe[:i] + "--" + safe[i+1:]
		}
	}
	path := filepath.Join(dir, safe+".yaml")
	content := "id: " + id + "\ntype: " + string(ptype) + "\nowner: stub\nrepo: stub\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write project YAML: %v", err)
	}
}
