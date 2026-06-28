package llmexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunJSONFallsBackOnRateLimit(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "claude"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", res.Provider)
	}
	if res.Text != `{"ok":true}` {
		t.Fatalf("text = %q", res.Text)
	}
}

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
