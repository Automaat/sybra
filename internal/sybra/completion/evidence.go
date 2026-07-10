package completion

import (
	"bytes"
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
// walks — a runaway Playwright MCP session must not turn into an unbounded
// artifact-store write.
const maxEvidenceFiles = 50

// maxEvidenceFileSize bounds an individual evidence file.
const maxEvidenceFileSize = 10 * 1024 * 1024 // 10MB

var evidenceNameSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

var textEvidenceExtensions = map[string]struct{}{
	".csv":  {},
	".htm":  {},
	".html": {},
	".json": {},
	".log":  {},
	".md":   {},
	".txt":  {},
	".xml":  {},
	".yaml": {},
	".yml":  {},
}

func (h *Handler) importEvidenceForAgent(ag *agent.Agent) {
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

func (h *Handler) importTestRunnerEvidence(ag *agent.Agent, wtPath, projectID string) {
	if h.artifacts == nil || wtPath == "" {
		return
	}
	dir := filepath.Join(wtPath, worktree.EvidenceDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
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

func (h *Handler) surfaceEvidenceImport(ag *agent.Agent, imported int) {
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

func (h *Handler) importEvidenceEntry(ag *agent.Agent, dir string, e os.DirEntry, index int, scrubCtx *WorkScrubContext) (bool, error) {
	info, err := e.Info()
	if err != nil {
		return false, err
	}
	if e.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil
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
		if !isTextEvidenceFile(e.Name(), content) {
			return false, nil
		}
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

func isTextEvidenceFile(name string, content []byte) bool {
	if _, ok := textEvidenceExtensions[strings.ToLower(filepath.Ext(name))]; ok {
		return true
	}
	return !bytes.Contains(content, []byte{0})
}

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
