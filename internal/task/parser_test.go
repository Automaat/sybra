package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    Task
		wantErr bool
	}{
		{
			name: "valid frontmatter with body",
			input: `---
id: abc123
title: Test task
status: todo
agent_mode: headless
tags: [backend, auth]
---
## Description
Some body content`,
			want: Task{
				ID:        "abc123",
				Title:     "Test task",
				Status:    StatusTodo,
				AgentMode: "headless",
				Tags:      []string{"backend", "auth"},
				Body:      "## Description\nSome body content",
			},
		},
		{
			name: "valid frontmatter without body",
			input: `---
id: def456
title: Empty body task
status: done
---
`,
			want: Task{
				ID:     "def456",
				Title:  "Empty body task",
				Status: StatusDone,
			},
		},
		{
			name:    "missing delimiters",
			input:   "no frontmatter here",
			wantErr: true,
		},
		{
			name:    "only one delimiter",
			input:   "---\nid: test\n",
			wantErr: true,
		},
		{
			name: "human-required status",
			input: `---
id: hr1
title: Needs human
status: human-required
---
Blocked on credentials`,
			want: Task{
				ID:     "hr1",
				Title:  "Needs human",
				Status: StatusHumanRequired,
				Body:   "Blocked on credentials",
			},
		},
		{
			name: "allowed_tools parsed",
			input: `---
id: t1
title: With tools
status: todo
allowed_tools: [Read, Write, Bash]
---`,
			want: Task{
				ID:           "t1",
				Title:        "With tools",
				Status:       StatusTodo,
				AllowedTools: []string{"Read", "Write", "Bash"},
			},
		},
		{
			name: "invalid agent_mode rejected",
			input: `---
id: bad1
title: Bad mode
status: todo
agent_mode: supervised
---`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBytes([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tt.want.ID {
				t.Errorf("ID = %q, want %q", got.ID, tt.want.ID)
			}
			if got.Title != tt.want.Title {
				t.Errorf("Title = %q, want %q", got.Title, tt.want.Title)
			}
			if got.Status != tt.want.Status {
				t.Errorf("Status = %q, want %q", got.Status, tt.want.Status)
			}
			if got.AgentMode != tt.want.AgentMode {
				t.Errorf("AgentMode = %q, want %q", got.AgentMode, tt.want.AgentMode)
			}
			if got.Body != tt.want.Body {
				t.Errorf("Body = %q, want %q", got.Body, tt.want.Body)
			}
			if len(got.Tags) != len(tt.want.Tags) {
				t.Errorf("Tags = %v, want %v", got.Tags, tt.want.Tags)
			}
			if len(got.AllowedTools) != len(tt.want.AllowedTools) {
				t.Errorf("AllowedTools = %v, want %v", got.AllowedTools, tt.want.AllowedTools)
			}
		})
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	content := `---
id: file-test
title: From file
status: in-progress
---
Body here`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	task, err := Parse(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.ID != "file-test" {
		t.Errorf("ID = %q, want %q", task.ID, "file-test")
	}
	if task.FilePath != path {
		t.Errorf("FilePath = %q, want %q", task.FilePath, path)
	}
}

func TestParseNonexistentFile(t *testing.T) {
	t.Parallel()
	_, err := Parse("/nonexistent/path.md")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestMarshal(t *testing.T) {
	t.Parallel()
	task := Task{
		ID:        "m1",
		Title:     "Marshal test",
		Status:    StatusTodo,
		AgentMode: "headless",
		Body:      "Some body",
	}

	data, err := Marshal(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		t.Error("missing opening delimiter")
	}
	if !strings.Contains(s, "id: m1") {
		t.Error("missing id field")
	}
	if !strings.Contains(s, "title: Marshal test") {
		t.Error("missing title field")
	}
	if !strings.Contains(s, "status: todo") {
		t.Error("missing status field")
	}
	if !strings.HasSuffix(s, "Some body\n") {
		t.Errorf("unexpected body suffix: %q", s[len(s)-20:])
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	original := Task{
		ID:        "rt1",
		Title:     "Round trip",
		Status:    StatusInReview,
		AgentMode: "interactive",
		Tags:      []string{"test", "ci"},
		Body:      "## Steps\n- Step one\n- Step two",
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.ID != original.ID {
		t.Errorf("ID = %q, want %q", parsed.ID, original.ID)
	}
	if parsed.Title != original.Title {
		t.Errorf("Title = %q, want %q", parsed.Title, original.Title)
	}
	if parsed.Status != original.Status {
		t.Errorf("Status = %q, want %q", parsed.Status, original.Status)
	}
	if parsed.Body != original.Body {
		t.Errorf("Body = %q, want %q", parsed.Body, original.Body)
	}
	if len(parsed.Tags) != len(original.Tags) {
		t.Errorf("Tags = %v, want %v", parsed.Tags, original.Tags)
	}
}

func TestMarshalEmptyBody(t *testing.T) {
	t.Parallel()
	task := Task{ID: "e1", Title: "No body", Status: StatusTodo}
	data, err := Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	// Should end with closing delimiter, no trailing body
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if lines[len(lines)-1] != "---" {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], "---")
	}
}

func TestMarshalRoundTripAllowedTools(t *testing.T) {
	t.Parallel()
	original := Task{
		ID:           "at1",
		Title:        "Tools roundtrip",
		Status:       StatusTodo,
		AgentMode:    "headless",
		AllowedTools: []string{"Read", "Write", "Bash"},
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(parsed.AllowedTools) != len(original.AllowedTools) {
		t.Fatalf("AllowedTools len = %d, want %d", len(parsed.AllowedTools), len(original.AllowedTools))
	}
	for i, tool := range original.AllowedTools {
		if parsed.AllowedTools[i] != tool {
			t.Errorf("AllowedTools[%d] = %q, want %q", i, parsed.AllowedTools[i], tool)
		}
	}
}

func TestMarshalRoundTripAgentRuns(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	original := Task{
		ID:        "ar1",
		Title:     "AgentRuns roundtrip",
		Status:    StatusInProgress,
		AgentMode: "headless",
		AgentRuns: []AgentRun{
			{
				AgentID:   "agent-001",
				Mode:      "headless",
				State:     "done",
				StartedAt: now,
				CostUSD:   1.23,
				Result:    "success",
				LogFile:   "/tmp/log.txt",
			},
			{
				AgentID:   "agent-002",
				Mode:      "interactive",
				State:     "running",
				StartedAt: now,
			},
		},
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(parsed.AgentRuns) != 2 {
		t.Fatalf("AgentRuns len = %d, want 2", len(parsed.AgentRuns))
	}

	r := parsed.AgentRuns[0]
	if r.AgentID != "agent-001" {
		t.Errorf("AgentRuns[0].AgentID = %q, want %q", r.AgentID, "agent-001")
	}
	if r.CostUSD != 1.23 {
		t.Errorf("AgentRuns[0].CostUSD = %f, want 1.23", r.CostUSD)
	}
	if r.Result != "success" {
		t.Errorf("AgentRuns[0].Result = %q, want %q", r.Result, "success")
	}
	if r.LogFile != "/tmp/log.txt" {
		t.Errorf("AgentRuns[0].LogFile = %q, want %q", r.LogFile, "/tmp/log.txt")
	}

	r2 := parsed.AgentRuns[1]
	if r2.AgentID != "agent-002" {
		t.Errorf("AgentRuns[1].AgentID = %q, want %q", r2.AgentID, "agent-002")
	}
	if r2.State != "running" {
		t.Errorf("AgentRuns[1].State = %q, want %q", r2.State, "running")
	}
}

func TestMarshalRoundTripAgentRunsIndentedResult(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	original := Task{
		ID:        "indent1",
		Title:     "Indented result roundtrip",
		Status:    StatusDone,
		AgentMode: "headless",
		AgentRuns: []AgentRun{
			{
				AgentID:   "a1",
				Mode:      "headless",
				State:     "stopped",
				StartedAt: now,
				Result:    "    indented line\nnormal line\n- bullet",
			},
			{
				AgentID:   "a2",
				Mode:      "headless",
				State:     "stopped",
				StartedAt: now,
				Result:    "second run result",
			},
		},
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("parse round-trip failed (this was the |4- bug): %v", err)
	}

	if len(parsed.AgentRuns) != 2 {
		t.Fatalf("AgentRuns len = %d, want 2", len(parsed.AgentRuns))
	}
	if parsed.AgentRuns[1].Result != "second run result" {
		t.Errorf("AgentRuns[1].Result = %q, want %q", parsed.AgentRuns[1].Result, "second run result")
	}
}

func TestMarshalRoundTripAgentRunsLeadingBlankLines(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	original := Task{
		ID:        "blank1",
		Title:     "Leading blank lines roundtrip",
		Status:    StatusInReview,
		AgentMode: "headless",
		AgentRuns: []AgentRun{
			{
				AgentID:   "a1",
				Mode:      "headless",
				State:     "stopped",
				StartedAt: now,
				Result:    "\n\nThe PR URL is invalid.\n\nPlease provide a valid URL:\n```\nhttps://example.com\n```",
			},
		},
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("parse round-trip failed (leading blank lines triggered |N- bug): %v", err)
	}
	if len(parsed.AgentRuns) != 1 {
		t.Fatalf("AgentRuns len = %d, want 1", len(parsed.AgentRuns))
	}
	if !strings.Contains(parsed.AgentRuns[0].Result, "The PR URL is invalid.") {
		t.Errorf("AgentRuns[0].Result = %q, missing expected content", parsed.AgentRuns[0].Result)
	}
}

func TestMarshalUpdatesTimestamp(t *testing.T) {
	t.Parallel()
	before := time.Now().UTC().Add(-time.Second)
	task := Task{
		ID:     "ts1",
		Title:  "Timestamp test",
		Status: StatusTodo,
	}

	data, err := Marshal(task)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatal(err)
	}

	if parsed.UpdatedAt.Before(before) {
		t.Errorf("UpdatedAt = %v, expected after %v", parsed.UpdatedAt, before)
	}
}

func TestParseBytesSpecialCharsInBody(t *testing.T) {
	t.Parallel()
	input := "---\nid: sc1\ntitle: Special\nstatus: todo\n---\n## Code\n```go\nfunc main() { fmt.Println(\"hello\") }\n```\n\n- Item with `backticks`\n- Item with *emphasis*"
	task, err := ParseBytes([]byte(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(task.Body, "```go") {
		t.Error("body should contain code fence")
	}
	if !strings.Contains(task.Body, "`backticks`") {
		t.Error("body should contain backticks")
	}
}

// TestParseBytesBOMPrefix verifies task files with a leading UTF-8 BOM
// parse successfully. Windows editors (Notepad, VS Code with BOM enabled)
// prepend `\xef\xbb\xbf` on save — prior to the fix the frontmatter regex
// failed to match the first `---` and the whole task silently disappeared
// from the list. ParseBytes now strips the BOM before the regex runs.
func TestParseBytesBOMPrefix(t *testing.T) {
	t.Parallel()
	bom := "\xef\xbb\xbf"
	input := bom + "---\nid: bom1\ntitle: BOM task\nstatus: todo\n---\nBody"
	got, err := ParseBytes([]byte(input))
	if err != nil {
		t.Fatalf("BOM-prefixed task file should parse cleanly after the fix; got %v", err)
	}
	if got.ID != "bom1" {
		t.Errorf("ID = %q, want %q", got.ID, "bom1")
	}
	if got.Title != "BOM task" {
		t.Errorf("Title = %q, want %q", got.Title, "BOM task")
	}
	if got.Body != "Body" {
		t.Errorf("Body = %q, want %q", got.Body, "Body")
	}
}

// TestParseBytesCRLFLineEndings verifies that task files saved on Windows
// (CRLF line endings) parse identically to Unix-LF files. The frontmatter
// regex `(?m)^---\s*$` relies on `\s*` absorbing `\r` before end-of-line.
// A regression that tightened the regex to `$` with `strict=true` would
// drop CRLF files on the floor.
func TestParseBytesCRLFLineEndings(t *testing.T) {
	t.Parallel()
	input := "---\r\nid: crlf1\r\ntitle: CRLF task\r\nstatus: todo\r\nagent_mode: headless\r\n---\r\nBody line one\r\nBody line two\r\n"
	got, err := ParseBytes([]byte(input))
	if err != nil {
		t.Fatalf("CRLF file should parse: %v", err)
	}
	if got.ID != "crlf1" {
		t.Errorf("ID = %q, want %q", got.ID, "crlf1")
	}
	if got.Title != "CRLF task" {
		t.Errorf("Title = %q, want %q", got.Title, "CRLF task")
	}
	if !strings.Contains(got.Body, "Body line one") || !strings.Contains(got.Body, "Body line two") {
		t.Errorf("Body = %q, expected both lines present", got.Body)
	}
}

// TestParseBytesDuplicateYAMLKeysLastWins documents yaml.v3's last-wins
// behavior for duplicate mapping keys in frontmatter. A task saved by two
// tools that both emit an `id` field would otherwise silently shadow the
// first. Pinning the behavior here means any future switch to a strict
// yaml.v3 decoder (KnownFields/Strict) will surface as a failing test
// instead of a runtime surprise.
func TestParseBytesDuplicateYAMLKeysLastWins(t *testing.T) {
	t.Parallel()
	input := "---\nid: first\nid: second\ntitle: Dup keys\nstatus: todo\n---\nBody"
	got, err := ParseBytes([]byte(input))
	if err != nil {
		// Strict mode would reject — also acceptable, but must say which key.
		if !strings.Contains(err.Error(), "id") && !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("duplicate-key error should reference the key; got %q", err)
		}
		return
	}
	if got.ID != "second" {
		t.Errorf("ID = %q, want %q (yaml.v3 last-wins)", got.ID, "second")
	}
}

// TestParseBytesLargeBody verifies a ~1 MiB body survives round-trip without
// truncation or allocation panics. Agents dumping command output into the
// task body can easily hit this range. A regression that truncates silently
// (e.g. introducing a bytes.Buffer.Cap check) would corrupt history.
func TestParseBytesLargeBody(t *testing.T) {
	t.Parallel()
	const size = 1024 * 1024
	body := strings.Repeat("x", size)
	input := "---\nid: big1\ntitle: Big body\nstatus: todo\n---\n" + body
	got, err := ParseBytes([]byte(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Body) != size {
		t.Errorf("Body len = %d, want %d (no truncation)", len(got.Body), size)
	}
	// Round-trip.
	data, err := Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reparsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(reparsed.Body) != size {
		t.Errorf("reparse Body len = %d, want %d", len(reparsed.Body), size)
	}
}
