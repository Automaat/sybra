package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Automaat/sybra/internal/artifact"
)

const (
	toolResultInlineBytes      = 2 * 1024
	toolResultSummaryMaxBytes  = 4 * 1024
	toolResultContextLines     = 2
	toolResultHeadLines        = 12
	toolResultTailLines        = 12
	toolResultMaxFocusWindows  = 4
	toolResultMaxFocusSections = 2
)

var (
	testFailuresHeadingRe = regexp.MustCompile(`(?i)^##\s+test failures\b`)
	tracebackStartRe      = regexp.MustCompile(`^Traceback \(most recent call last\):`)
	goTestFailRe          = regexp.MustCompile(`^(--- FAIL:|FAIL\t|FAIL$|panic:)`)
	compilerDiagRe        = regexp.MustCompile(`^[^[:space:]:][^:]*:\d+(?::\d+)?:`)
	errorWordRe           = regexp.MustCompile(`(?i)\b(error|errors|failed|failure|fatal|panic|exception|traceback|undefined|cannot|invalid|unexpected)\b`)
)

type lineWindow struct {
	start int
	end   int
}

type toolResultArtifactRef struct {
	persisted bool
	name      string
	path      string
	digest    string
	sizeBytes int
	lineCount int
}

// bindToolResultEvent rewrites oversized headless tool results into a bounded,
// artifact-backed summary before Sybra stores or replays them.
func bindToolResultEvent(taskID, producerRole string, store *artifact.Store, ev StreamEvent) StreamEvent {
	if len(ev.toolResults) == 0 {
		return ev
	}

	results := make([]ToolResultBlock, len(ev.toolResults))
	parts := make([]string, 0, len(ev.toolResults))
	for i := range ev.toolResults {
		results[i] = ev.toolResults[i]
		bounded := bindToolResultContent(taskID, producerRole, store, ev.toolResults[i])
		results[i].Content = bounded
		if bounded != "" {
			parts = append(parts, bounded)
		}
	}
	ev.toolResults = results
	ev.Content = strings.Join(parts, "\n\n")
	return ev
}

func bindToolResultContent(taskID, producerRole string, store *artifact.Store, tr ToolResultBlock) string {
	if len(tr.Content) <= toolResultInlineBytes {
		return tr.Content
	}

	ref := persistToolResultArtifact(taskID, producerRole, store, tr)
	summary := summarizeToolResult(tr.Content, tr.IsError)

	var b strings.Builder
	b.WriteString("[tool output truncated]\n")
	if ref.persisted && ref.name != "" {
		fmt.Fprintf(&b, "artifact_name: %s\n", ref.name)
	}
	if ref.persisted && ref.path != "" {
		fmt.Fprintf(&b, "artifact_path: %s\n", ref.path)
	}
	if ref.digest != "" {
		fmt.Fprintf(&b, "sha256: %s\n", ref.digest)
	}
	if ref.sizeBytes > 0 {
		fmt.Fprintf(&b, "bytes: %d\n", ref.sizeBytes)
	}
	if ref.lineCount > 0 {
		fmt.Fprintf(&b, "lines: %d\n", ref.lineCount)
	}
	if ref.persisted {
		b.WriteString("focused_range_hint: request a focused line range from the saved artifact if you need more context\n")
	}
	b.WriteString("\n")
	b.WriteString(summary)
	return truncateUTF8(b.String(), toolResultSummaryMaxBytes)
}

func persistToolResultArtifact(taskID, producerRole string, store *artifact.Store, tr ToolResultBlock) toolResultArtifactRef {
	ref := toolResultArtifactRef{
		sizeBytes: len(tr.Content),
		lineCount: countLines(tr.Content),
	}
	sum := sha256.Sum256([]byte(tr.Content))
	ref.digest = hex.EncodeToString(sum[:])
	if store == nil || taskID == "" {
		return ref
	}
	ref.name = fmt.Sprintf("tool-output-%s-%s.txt", sanitizeArtifactToken(tr.ToolUseID), ref.digest[:12])
	if _, err := store.Put(taskID, artifact.Artifact{
		Kind:         artifact.KindGeneric,
		Name:         ref.name,
		ProducerRole: producerRole,
		SourcePath:   "tool-result:" + tr.ToolUseID,
		Content:      []byte(tr.Content),
	}); err != nil {
		ref.name = ""
		return ref
	}
	ref.persisted = true
	path, err := store.Path(taskID, ref.name)
	if err == nil {
		ref.path = path
	}
	return ref
}

func summarizeToolResult(content string, isError bool) string {
	lines := splitToolResultLines(content)
	if len(lines) == 0 {
		return ""
	}

	var sections []string
	for _, w := range interestingToolResultWindows(lines, isError) {
		label := fmt.Sprintf("focused_excerpt lines %d-%d:", w.start+1, w.end+1)
		sections = append(sections, label+"\n"+strings.Join(lines[w.start:w.end+1], "\n"))
		if len(sections) >= toolResultMaxFocusSections {
			break
		}
	}
	if len(sections) == 0 {
		headEnd := min(len(lines), toolResultHeadLines)
		sections = append(sections, fmt.Sprintf("head lines 1-%d:\n%s", headEnd, strings.Join(lines[:headEnd], "\n")))
		if tailStart := max(0, len(lines)-toolResultTailLines); tailStart > 0 {
			sections = append(sections, fmt.Sprintf("tail lines %d-%d:\n%s", tailStart+1, len(lines), strings.Join(lines[tailStart:], "\n")))
		}
		return truncateUTF8(strings.Join(sections, "\n\n"), toolResultSummaryMaxBytes)
	}

	headEnd := min(len(lines), toolResultHeadLines)
	headWindow := lineWindow{start: 0, end: headEnd - 1}
	if headEnd > 0 && !windowCovered(headWindow, sections, lines) {
		sections = append(sections, fmt.Sprintf("head lines 1-%d:\n%s", headEnd, strings.Join(lines[:headEnd], "\n")))
	}
	if tailStart := max(0, len(lines)-toolResultTailLines); tailStart > 0 {
		tailWindow := lineWindow{start: tailStart, end: len(lines) - 1}
		if !windowCovered(tailWindow, sections, lines) {
			sections = append(sections, fmt.Sprintf("tail lines %d-%d:\n%s", tailStart+1, len(lines), strings.Join(lines[tailStart:], "\n")))
		}
	}
	return truncateUTF8(strings.Join(sections, "\n\n"), toolResultSummaryMaxBytes)
}

func interestingToolResultWindows(lines []string, isError bool) []lineWindow {
	if start := findTestFailuresSection(lines); start >= 0 {
		end := len(lines) - 1
		for i := start + 1; i < len(lines); i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
				end = i - 1
				break
			}
		}
		return []lineWindow{{start: start, end: max(start, end)}}
	}

	if start := findTraceback(lines); start >= 0 {
		end := len(lines) - 1
		for i := start + 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "" {
				end = i - 1
				break
			}
		}
		return []lineWindow{{start: start, end: max(start, end)}}
	}

	var windows []lineWindow
	for i := range lines {
		if scoreToolResultLine(strings.TrimSpace(lines[i]), isError) == 0 {
			continue
		}
		windows = append(windows, lineWindow{
			start: max(0, i-toolResultContextLines),
			end:   min(len(lines)-1, i+toolResultContextLines),
		})
		if len(windows) >= toolResultMaxFocusWindows {
			break
		}
	}
	return mergeWindows(windows)
}

func findTestFailuresSection(lines []string) int {
	for i := range lines {
		if testFailuresHeadingRe.MatchString(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
}

func findTraceback(lines []string) int {
	for i := range lines {
		if tracebackStartRe.MatchString(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
}

func scoreToolResultLine(line string, isError bool) int {
	switch {
	case line == "":
		return 0
	case strings.Contains(line, "TEST_VERDICT:"):
		return 100
	case testFailuresHeadingRe.MatchString(line):
		return 95
	case goTestFailRe.MatchString(line):
		return 90
	case tracebackStartRe.MatchString(line):
		return 85
	case compilerDiagRe.MatchString(line) && errorWordRe.MatchString(line):
		return 80
	case strings.Contains(strings.ToLower(line), "error:"):
		return 70
	case isError && errorWordRe.MatchString(line):
		return 60
	default:
		return 0
	}
}

func mergeWindows(windows []lineWindow) []lineWindow {
	if len(windows) == 0 {
		return nil
	}
	out := make([]lineWindow, 0, len(windows))
	cur := windows[0]
	for i := 1; i < len(windows); i++ {
		if windows[i].start <= cur.end+1 {
			cur.end = max(cur.end, windows[i].end)
			continue
		}
		out = append(out, cur)
		cur = windows[i]
	}
	return append(out, cur)
}

func splitToolResultLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func countLines(content string) int {
	if content == "" {
		return 0
	}
	return len(splitToolResultLines(content))
}

func sanitizeArtifactToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "tool"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "tool"
	}
	return out
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 3 {
		return s[:maxBytes]
	}
	cut := maxBytes - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func windowCovered(w lineWindow, sections, lines []string) bool {
	if len(sections) == 0 || len(lines) == 0 {
		return false
	}
	needle := strings.Join(lines[w.start:w.end+1], "\n")
	for _, sec := range sections {
		if strings.Contains(sec, needle) {
			return true
		}
	}
	return false
}
