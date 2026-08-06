package task

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Automaat/sybra/internal/textutil"
)

// A status reason is built by truncating raw agent output, then persisted as
// YAML frontmatter. A cut landing inside a multibyte rune used to leave
// invalid UTF-8, which yaml.v3 re-encodes as an unreadable !!binary block —
// the task file and the board then show base64 instead of the reason.
func TestStatusReasonFromTruncatedAgentOutputMarshalsAsPlainYAML(t *testing.T) {
	t.Parallel()
	output := "planning failed " + strings.Repeat("…", 400) // ellipses put the 500-byte cut mid-rune

	reason := "planning plan retry budget exhausted after 3 attempt(s): " +
		textutil.TruncateBytes(strings.TrimSpace(output), 500, "\n... (truncated)")

	if !utf8.ValidString(reason) {
		t.Fatalf("truncated status reason is not valid UTF-8: %q", reason)
	}

	task := Task{
		ID:           "task-utf8",
		Title:        "utf8 reason",
		Status:       StatusHumanRequired,
		AgentMode:    AgentModeHeadless,
		StatusReason: reason,
	}
	data, err := Marshal(task)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "!!binary") {
		t.Fatalf("status reason marshalled as a binary block:\n%s", data)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if parsed.StatusReason != reason {
		t.Errorf("StatusReason did not round-trip:\ngot  %q\nwant %q", parsed.StatusReason, reason)
	}
}
