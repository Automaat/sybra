package sybra

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/worktree"
)

// maxEvidenceFiles bounds how many files a single test-runner evidence import
// walks — a runaway Playwright MCP session (e.g. one screenshot per retry
// loop iteration) must not turn into an unbounded artifact-store write.
const maxEvidenceFiles = 50

// maxEvidenceFileSize bounds an individual evidence file. Screenshots and
// console logs are small; this guards against an accidental large capture
// (e.g. a video recording) bloating the local artifact store.
const maxEvidenceFileSize = 10 * 1024 * 1024 // 10MB

// evidenceNameSanitizeRe strips everything outside the artifact store's
// allowed name charset (see artifact.validName) from an evidence file's stem
// and extension.
var evidenceNameSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (h *AgentCompletionHandler) importEvidenceForAgent(ag *agent.Agent) {
	if h.tasks == nil || h.worktrees == nil {
		return
	}
	t, err := h.tasks.Get(ag.TaskID)
	if err != nil {
		return
	}
	if wtPath := h.worktrees.PathFor(t); wtPath != "" {
		if _, statErr := os.Stat(wtPath); statErr == nil {
			h.importTestRunnerEvidence(ag, wtPath, t.ProjectID)
		}
	}
}

// importTestRunnerEvidence walks a completed test-runner's evidence directory
// (worktree.EvidenceDirName, populated by a headless Playwright MCP server —
// see internal/agent/mcp.go) and imports bounded, sanitized regular files into
// the task's local artifact store. Called synchronously from OnComplete
// before the async worktree cleanup so the directory still exists.
//
// projectID resolves a WorkScrubContext via h.workScrub: for work-typed
// tasks, evidence content is redacted through scrub.Scrub before it is
// persisted, so a captured screenshot/console log can never carry a raw
// work-repo identifier into the local artifact store. See CLAUDE.md —
// Work-Data Confidentiality.
//
// Best-effort throughout: a missing/empty/unreadable evidence dir, a nil
// artifact store, or a per-file failure are all no-ops or logged warnings —
// evidence capture must never block workflow advancement past a completed
// test-runner run.
func (h *AgentCompletionHandler) importTestRunnerEvidence(ag *agent.Agent, wtPath, projectID string) {
	if h.artifacts == nil || wtPath == "" {
		return
	}
	dir := filepath.Join(wtPath, worktree.EvidenceDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // missing/unreadable dir: nothing to import
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var scrubCtx *WorkScrubContext
	if h.workScrub != nil {
		scrubCtx = h.workScrub(projectID)
	}

	imported := 0
	for _, e := range entries {
		if imported >= maxEvidenceFiles {
			h.logger.Warn("agent.evidence.import.truncated",
				"task_id", ag.TaskID, "agent_id", ag.ID, "max", maxEvidenceFiles, "found", len(entries))
			break
		}
		ok, err := h.importEvidenceEntry(ag, dir, e, imported, scrubCtx)
		if err != nil {
			h.logger.Warn("agent.evidence.import.failed",
				"task_id", ag.TaskID, "agent_id", ag.ID, "file", e.Name(), "err", err)
			continue
		}
		if ok {
			imported++
		}
	}
	if imported > 0 {
		h.surfaceEvidenceImport(ag, imported)
	}
}

// surfaceEvidenceImport appends a progress-log entry so imported evidence is
// discoverable from the task's Progress tab (`sybra-cli --json progress list
// <id>`), satisfying the "surface it on the task" requirement without a
// dedicated GUI artifact viewer. Best-effort: a nil store or append failure
// is logged, never fatal to the completion path.
func (h *AgentCompletionHandler) surfaceEvidenceImport(ag *agent.Agent, imported int) {
	if h.artifacts == nil {
		return
	}
	msg := fmt.Sprintf("imported %d test-runner evidence file(s) into the artifact store", imported)
	if err := h.artifacts.AppendProgress(ag.TaskID, artifact.ProgressEntry{
		Kind:    artifact.ProgressKindProgress,
		Role:    string(agent.RoleTestRunner),
		Message: msg,
	}); err != nil {
		h.logger.Warn("agent.evidence.import.progress-log-failed",
			"task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}
}

// importEvidenceEntry imports a single evidence-dir entry. ok reports whether
// a file was actually imported (false for a silently-skipped directory or
// symlink); err is non-nil only for a genuine failure worth logging. scrubCtx
// is non-nil only for work-typed tasks — when set, content is redacted
// through scrub.Scrub before it is written to the artifact store.
func (h *AgentCompletionHandler) importEvidenceEntry(ag *agent.Agent, dir string, e os.DirEntry, index int, scrubCtx *WorkScrubContext) (ok bool, err error) {
	info, err := e.Info()
	if err != nil {
		return false, err
	}
	if e.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil // skip: only plain regular files are imported
	}
	if !ag.StartedAt.IsZero() && info.ModTime().Before(ag.StartedAt) {
		return false, nil
	}
	if info.Size() > maxEvidenceFileSize {
		return false, fmt.Errorf("file %d bytes exceeds %d byte cap", info.Size(), maxEvidenceFileSize)
	}
	srcPath := filepath.Join(dir, e.Name())
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return false, err
	}
	if scrubCtx != nil {
		scrubbed, _ := scrub.Scrub(string(content), scrubCtx.Blocklist)
		content = []byte(scrubbed)
	}
	_, err = h.artifacts.Put(ag.TaskID, artifact.Artifact{
		Kind:         artifact.KindGeneric,
		Name:         sanitizeEvidenceName(ag.ID, index, e.Name()),
		ProducerRole: string(agent.RoleTestRunner),
		SourcePath:   srcPath,
		Content:      content,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// sanitizeEvidenceName derives a store-safe, collision-resistant artifact name
// from an evidence file's original basename. Prefixing with the agent ID and
// a per-run index means a rerun (a fresh agent ID) or a duplicate basename
// within the same run never overwrites an earlier import.
func sanitizeEvidenceName(agentID string, index int, base string) string {
	ext := evidenceNameSanitizeRe.ReplaceAllString(filepath.Ext(base), "")
	stem := evidenceNameSanitizeRe.ReplaceAllString(strings.TrimSuffix(base, filepath.Ext(base)), "-")
	if stem == "" {
		stem = "evidence"
	}
	name := fmt.Sprintf("test-runner-evidence-%s-%02d-%s%s", agentID, index, stem, ext)
	const maxNameLen = 200
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	return name
}
