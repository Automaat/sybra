package sandbox

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadEnvFile parses a .env file (KEY=VALUE lines) and returns the entries as
// "KEY=VALUE" strings suitable for appending to os.Environ(). Lines starting
// with # and blank lines are ignored. Lines without '=' are skipped. Expands
// a leading ~ to the user home directory. Returns nil, nil if path is empty.
func LoadEnvFile(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("expand ~: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env_file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var entries []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			continue
		}
		entries = append(entries, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env_file %s: %w", path, err)
	}
	return entries, nil
}
