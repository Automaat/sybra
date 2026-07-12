package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type mcpServerSpec struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func parseMCPServers(mcpJSON string) (map[string]mcpServerSpec, error) {
	if strings.TrimSpace(mcpJSON) == "" {
		return map[string]mcpServerSpec{}, nil
	}
	var doc struct {
		MCPServers map[string]mcpServerSpec `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpJSON), &doc); err != nil {
		return nil, fmt.Errorf("agent: parse mcp config: %w", err)
	}
	return doc.MCPServers, nil
}

func sortedServerNames(servers map[string]mcpServerSpec) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func renderCopilotMCPConfig(mcpJSON string) (string, error) {
	servers, err := parseMCPServers(mcpJSON)
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		return "", nil
	}
	rendered := make(map[string]any, len(servers))
	for name, s := range servers {
		rendered[name] = map[string]any{
			"type":    "local",
			"command": s.Command,
			"args":    s.Args,
			"tools":   []string{"*"},
		}
	}
	data, err := json.Marshal(map[string]any{"mcpServers": rendered})
	if err != nil {
		return "", fmt.Errorf("agent: render copilot mcp config: %w", err)
	}
	return string(data), nil
}

func renderCodexMCPArgs(mcpJSON string) ([]string, error) {
	servers, err := parseMCPServers(mcpJSON)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, nil
	}
	var args []string
	for _, name := range sortedServerNames(servers) {
		s := servers[name]
		command, err := json.Marshal(s.Command)
		if err != nil {
			return nil, fmt.Errorf("agent: render codex mcp command: %w", err)
		}
		args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.command=%s", name, command))
		if len(s.Args) > 0 {
			serverArgs, err := json.Marshal(s.Args)
			if err != nil {
				return nil, fmt.Errorf("agent: render codex mcp args: %w", err)
			}
			args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.args=%s", name, serverArgs))
		}
	}
	return args, nil
}
