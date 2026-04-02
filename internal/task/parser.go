package task

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const frontmatterDelim = "---"

func Parse(path string) (Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Task{}, fmt.Errorf("read task file: %w", err)
	}

	t, err := ParseBytes(data)
	if err != nil {
		return Task{}, fmt.Errorf("parse %s: %w", path, err)
	}
	t.FilePath = path
	return t, nil
}

func ParseBytes(data []byte) (Task, error) {
	parts := bytes.SplitN(data, []byte(frontmatterDelim), 3)
	if len(parts) < 3 {
		return Task{}, fmt.Errorf("invalid frontmatter: expected --- delimiters")
	}

	var t Task
	if err := yaml.Unmarshal(parts[1], &t); err != nil {
		return Task{}, fmt.Errorf("unmarshal frontmatter: %w", err)
	}

	t.Body = string(bytes.TrimSpace(parts[2]))
	return t, nil
}

func Marshal(t Task) ([]byte, error) {
	t.UpdatedAt = time.Now().UTC()

	fm, err := yaml.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("marshal frontmatter: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString(frontmatterDelim + "\n")
	buf.Write(fm)
	buf.WriteString(frontmatterDelim + "\n")
	if t.Body != "" {
		buf.WriteString(t.Body)
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}
