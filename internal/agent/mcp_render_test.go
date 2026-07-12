package agent

import (
	"strings"
	"testing"
)

func TestParseMCPServers_EmptyAndInvalid(t *testing.T) {
	got, err := parseMCPServers("")
	if err != nil || len(got) != 0 {
		t.Fatalf("empty input: got %v err %v, want empty/nil", got, err)
	}
	if _, err := parseMCPServers("{not json"); err == nil {
		t.Fatal("malformed JSON should error")
	}
}

func TestRenderCopilotMCPConfig(t *testing.T) {
	if out, err := renderCopilotMCPConfig(""); err != nil || out != "" {
		t.Fatalf("empty input: got %q err %v", out, err)
	}
	in := `{"mcpServers":{"playwright":{"command":"npx","args":["-y","@playwright/mcp@latest"]}}}`
	out, err := renderCopilotMCPConfig(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{`"mcpServers"`, `"type":"local"`, `"tools":["*"]`, `"command":"npx"`, `"@playwright/mcp@latest"`} {
		if !strings.Contains(out, want) {
			t.Errorf("copilot config missing %q; got %s", want, out)
		}
	}
}

func TestRenderCodexMCPArgs(t *testing.T) {
	if out, err := renderCodexMCPArgs(""); err != nil || out != nil {
		t.Fatalf("empty input: got %v err %v", out, err)
	}
	in := `{"mcpServers":{"playwright":{"command":"npx","args":["-y","x"]}}}`
	out, err := renderCodexMCPArgs(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := []string{"-c", `mcp_servers.playwright.command="npx"`, "-c", `mcp_servers.playwright.args=["-y","x"]`}
	if len(out) != len(want) {
		t.Fatalf("args = %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, out[i], want[i])
		}
	}
}

func TestRenderCodexMCPArgs_DeterministicOrder(t *testing.T) {
	in := `{"mcpServers":{"bravo":{"command":"b"},"alpha":{"command":"a"}}}`
	out, err := renderCodexMCPArgs(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(out) < 4 || out[1] != `mcp_servers.alpha.command="a"` || out[3] != `mcp_servers.bravo.command="b"` {
		t.Fatalf("servers not rendered in sorted order: %v", out)
	}
}
