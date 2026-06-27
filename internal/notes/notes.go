// Package notes defines the agent working-memory scratchpad (NOTES.md): a
// git-excluded file Sybra seeds into implementation worktrees and inlines into
// agent prompts. It gives providers without session resume (Codex) and any
// restarted agent a durable record of plan, decisions, and dead ends that
// survives runs and context compaction — the Ralph-loop external-memory
// pattern.
//
// The file lifecycle (creation + git-exclusion) lives in internal/worktree;
// this leaf holds only the shared constants and the pure read/seed helpers so
// both internal/worktree and internal/agent can depend on it without a cycle
// (worktree deliberately avoids importing agent, and agent must not import
// worktree's project/task chain just to seed a prompt).
package notes

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// FileName is the scratchpad's name at the worktree root.
const FileName = "NOTES.md"

// seedMaxBytes caps how much of NOTES.md is inlined into a prompt. Agents are
// told to keep it concise, but a runaway file must not blow up the context
// window. When over the cap, a head+tail window is kept (see clampNotes).
const seedMaxBytes = 16 * 1024

// seedHeadBytes is the slice of an over-cap NOTES.md kept from the top. The
// seed template puts Plan/Decisions first, so keeping a head preserves the
// durable structure while the remaining budget keeps the recent tail.
const seedHeadBytes = seedMaxBytes / 3

const elisionMarker = "\n\n…(middle of NOTES.md elided to fit context; keep it concise)…\n\n"

// SeedTemplate is written when a worktree first gets a scratchpad. It is short
// on purpose: it teaches the structure without crowding the agent's prompt when
// inlined verbatim on the first run.
const SeedTemplate = `# Working Notes

Persistent scratchpad for this task. Survives across agent runs, restarts, and
context compaction — for providers without session resume (e.g. Codex) this is
the only memory of prior turns. Git-excluded; never committed. Keep it concise
and current.

## Plan

## Decisions

## Tried & failed
`

// Read returns the scratchpad contents for the worktree at dir and whether it
// was read. Any read failure — a missing file (non-worktree agent or unseeded
// worktree), a permission error, or transient IO — yields ("", false) so
// callers no-op rather than seed a partial/empty scratchpad. "false" therefore
// means "unavailable", not strictly "file missing".
func Read(dir string) (string, bool) {
	if strings.TrimSpace(dir) == "" {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// SeedPrompt appends the working-memory section to an agent prompt: a standing
// instruction to read and maintain NOTES.md, plus its current contents inlined
// so a resume-less provider (Codex) or a restarted agent recovers prior
// decisions without a tool call. Returns prompt unchanged when dir has no
// NOTES.md. The section is appended (not prepended) so it lands near the end of
// the prompt, where instruction-following is strongest.
func SeedPrompt(prompt, dir string) string {
	content, ok := Read(dir)
	if !ok {
		return prompt
	}
	body, truncated := clampNotes(strings.TrimSpace(content))

	var sb strings.Builder
	sb.WriteString(prompt)
	sb.WriteString("\n\n---\n\n## Working memory: `")
	sb.WriteString(FileName)
	sb.WriteString("`\n\n")
	sb.WriteString("A persistent scratchpad lives at `")
	sb.WriteString(FileName)
	sb.WriteString("` in your worktree root (git-excluded, never committed). It is your\n")
	sb.WriteString("durable record of plan, decisions, and dead ends — it survives runs,\n")
	sb.WriteString("restarts, and context compaction. **Read it before acting and keep it\n")
	sb.WriteString("current as you work.** Current contents")
	if truncated {
		sb.WriteString(" (head+tail; middle elided to fit context)")
	}
	sb.WriteString(":\n\n")
	if body == "" {
		sb.WriteString("_(empty — start filling it in)_\n")
	} else {
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	return sb.String()
}

// clampNotes bounds an over-cap NOTES.md to a head+tail window joined by an
// elision marker, returning (clamped, wasTruncated). The head preserves the
// template's Plan/Decisions sections (top of file); the tail preserves the most
// recent activity. Both cuts land on UTF-8 rune boundaries so the inlined text
// never begins or ends mid-rune (which would decode to U+FFFD).
func clampNotes(body string) (string, bool) {
	if len(body) <= seedMaxBytes {
		return body, false
	}
	head := trimToRuneBoundaryEnd(body[:seedHeadBytes])
	tail := trimToRuneBoundaryStart(body[len(body)-(seedMaxBytes-seedHeadBytes):])
	return head + elisionMarker + tail, true
}

// trimToRuneBoundaryStart drops a leading partial rune so s begins on a rune
// boundary. A valid UTF-8 string with no leading continuation byte is returned
// unchanged.
func trimToRuneBoundaryStart(s string) string {
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// trimToRuneBoundaryEnd drops a trailing partial rune so s ends on a rune
// boundary. Only an invalid encoding (RuneError with size 1) is trimmed; a
// legitimately-present U+FFFD (size 3) is kept.
func trimToRuneBoundaryEnd(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
