package task

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var frontmatterRe = regexp.MustCompile(`(?m)^---\s*$`)

// utf8BOM is stripped from the front of task files before the frontmatter
// regex runs. Editors on Windows (Notepad, VS Code with "add BOM") and some
// web download flows prepend this. Leaving it in place makes the first `---`
// not match `^` in multiline mode, which surfaces as a confusing "invalid
// frontmatter" error and causes the whole task to disappear from the list.
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

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
	data = bytes.TrimPrefix(data, utf8BOM)
	locs := frontmatterRe.FindAllIndex(data, 2)
	if len(locs) < 2 {
		return Task{}, fmt.Errorf("invalid frontmatter: expected --- delimiters")
	}

	fm := data[locs[0][1]:locs[1][0]]

	var t Task
	if err := yaml.Unmarshal(fm, &t); err != nil {
		return Task{}, fmt.Errorf("unmarshal frontmatter: %w", err)
	}

	t.Body = string(bytes.TrimSpace(data[locs[1][1]:]))
	if t.TaskType == "" {
		t.TaskType = TaskTypeNormal
	}
	if t.AgentRuns == nil {
		t.AgentRuns = []AgentRun{}
	}
	if t.AgentMode != "" {
		if _, err := ValidateAgentMode(t.AgentMode); err != nil {
			return Task{}, err
		}
	}
	return t, nil
}

func Marshal(t Task) ([]byte, error) {
	t.UpdatedAt = time.Now().UTC()

	// Strip leading whitespace/newlines from agent run results and prompts
	// so yaml.v3 doesn't emit |N- block scalars that it fails to parse back
	// (known round-trip bug: leading blank lines or indented first line
	// force an explicit indentation indicator that miscounts columns inside
	// a nested sequence).
	for i := range t.AgentRuns {
		t.AgentRuns[i].Result = strings.TrimLeft(t.AgentRuns[i].Result, " \t\n\r")
		t.AgentRuns[i].Prompt = strings.TrimLeft(t.AgentRuns[i].Prompt, " \t\n\r")
	}

	fm, err := yaml.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("marshal frontmatter: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString("---" + "\n")
	buf.Write(fm)
	buf.WriteString("---" + "\n")
	if t.Body != "" {
		buf.WriteString(t.Body)
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}
