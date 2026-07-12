package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cleanEnvPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return ""
	}
	tmpRoot := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	if !strings.HasPrefix(abs+string(filepath.Separator), tmpRoot) {
		return ""
	}
	return abs
}

func popScenario() string {
	if sf := cleanEnvPath(os.Getenv("FAKE_COPILOT_SCENARIO_FILE")); sf != "" {
		data, err := os.ReadFile(sf)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 0 && lines[0] != "" {
				scenario := strings.TrimSpace(lines[0])
				_ = os.WriteFile(sf, []byte(strings.Join(lines[1:], "\n")), 0o644)
				return scenario
			}
		}
	}
	if s := os.Getenv("FAKE_COPILOT_SCENARIO"); s != "" {
		return s
	}
	return "success"
}

func sessionID() string {
	for i, a := range os.Args {
		if a == "--session-id" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return "fake-copilot-session-1"
}

func emit(event map[string]any) {
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
	time.Sleep(10 * time.Millisecond)
}

func main() {
	if logFile := cleanEnvPath(os.Getenv("FAKE_COPILOT_ARGS_LOG")); logFile != "" {
		_ = os.WriteFile(logFile, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	}

	scenario := popScenario()
	exitCode := 0
	message := "Working on it..."
	switch scenario {
	case "fail_exit", "fail":
		message = "command failed"
		exitCode = 1
	}

	emit(map[string]any{"type": "assistant.message", "data": map[string]any{"content": message}})
	emit(map[string]any{"type": "result", "sessionId": sessionID(), "exitCode": exitCode})
	os.Exit(exitCode)
}
