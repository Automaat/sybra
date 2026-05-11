package sybra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/task"
)

// humanReviewPromptHeadTail bounds how many lines of the host log file are
// pulled into the review prompt. Keeps the prompt size predictable.
const (
	humanReviewLogTail        = 200
	humanReviewMaxAgentRuns   = 5
	humanReviewMaxAgentTurns  = 40
	humanReviewMaxAgentResult = 4000
	humanReviewWindow         = time.Hour
)

// humanReviewIssueFiler is the subset of monitor.GHIssueSink the handler
// depends on. Stub-friendly for tests; production wiring uses
// monitor.NewGHIssueSink.
type humanReviewIssueFiler interface {
	SubmitIssue(ctx context.Context, title, body string, extraLabels []string) (created bool, url string, err error)
}

// humanReviewHandler spawns a headless review agent each time a task
// transitions into status=human-required. The agent inspects task state,
// agent runs and Sybra logs/source, then emits a fenced verdict block
// (see verdictDecision) which the handler turns into a side-effect:
// genuine -> append a note; sybra_bug -> file a deduplicated GitHub issue
// and flip the task to status=blocked.
type humanReviewHandler struct {
	cfg     *config.Config
	tasks   *task.Manager
	agents  *agent.Manager
	audit   *audit.Logger
	logger  *slog.Logger
	sink    humanReviewIssueFiler
	homeDir string
	logFile string
	now     func() time.Time

	// canFilePublic reports whether human-review may run for a task with the
	// given project ID. Work-typed projects are blocked to prevent leaking
	// work-repo content into Automaat/sybra (see CLAUDE.md — Work-Data
	// Confidentiality). Wired from App.canFilePublicForProject; nil during
	// tests allows everything.
	canFilePublic func(projectID string) bool

	mu       sync.Mutex
	inflight map[string]string // taskID -> agent ID
	recent   []time.Time       // spawn timestamps (rolling window)
}

// verdictDecision is the agent's structured output. Captured from a fenced
// ```sybra-verdict\n{ ... }\n``` block in the agent's final assistant message.
type verdictDecision struct {
	Decision    string   `json:"decision"` // "human" | "sybra_bug"
	Summary     string   `json:"summary"`
	IssueTitle  string   `json:"issue_title,omitempty"`
	IssueBody   string   `json:"issue_body,omitempty"`
	IssueLabels []string `json:"issue_labels,omitempty"`
}

func newHumanReviewHandler(
	cfg *config.Config,
	tasks *task.Manager,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	sink humanReviewIssueFiler,
	homeDir, logFile string,
	canFilePublic func(projectID string) bool,
) *humanReviewHandler {
	return &humanReviewHandler{
		cfg:           cfg,
		tasks:         tasks,
		agents:        agents,
		audit:         al,
		logger:        logger,
		sink:          sink,
		homeDir:       homeDir,
		logFile:       logFile,
		now:           time.Now,
		canFilePublic: canFilePublic,
		inflight:      make(map[string]string),
	}
}

// initHumanReview is called once during App.Startup. It is a no-op when the
// feature is disabled or no Sybra source dir is configured.
func (a *App) initHumanReview() {
	if a.cfg == nil || !a.cfg.HumanReview.Enabled {
		return
	}
	dir := strings.TrimSpace(a.cfg.HumanReview.SybraRepoDir)
	if dir == "" {
		a.logger.Warn("human-review.disabled", "reason", "sybra_repo_dir is empty")
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		a.logger.Warn("human-review.disabled", "reason", "sybra_repo_dir not a directory", "dir", dir, "err", err)
		return
	}
	sink := monitor.NewGHIssueSink(a.cfg.HumanReviewIssueLabel(), a.cfg.HumanReviewRepo())
	logFile := filepath.Join(a.logDir, "sybra.log")
	a.humanReview = newHumanReviewHandler(a.cfg, a.tasks, a.agents, a.audit, a.logger, sink, config.HomeDir(), logFile, a.canFilePublicForProject)
	a.logger.Info("human-review.enabled", "dir", dir, "repo", a.cfg.HumanReviewRepo(), "model", a.cfg.HumanReviewModel())
}

// maybeSpawn is called from the status hook when a task lands in
// human-required. Returns immediately if the feature is disabled, the task
// already has an in-flight review, or the rolling rate limit is exceeded.
func (h *humanReviewHandler) maybeSpawn(taskID, prevStatus string) {
	if h == nil {
		return
	}
	// Don't loop when an unblock flow flips blocked → todo → human-required.
	if prevStatus == string(task.StatusBlocked) {
		h.skip(taskID, "prev_status_blocked")
		return
	}
	t, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.task.get", "task_id", taskID, "err", err)
		return
	}
	// Work-Data Confidentiality: human-review files issues on Automaat/sybra
	// (public) with task body + agent logs embedded. Work-typed projects must
	// never reach that pipeline, regardless of per-machine routing.
	if h.canFilePublic != nil && !h.canFilePublic(t.ProjectID) {
		h.skip(taskID, "work_project_no_public_filing")
		return
	}

	h.mu.Lock()
	if _, busy := h.inflight[taskID]; busy {
		h.mu.Unlock()
		h.skip(taskID, "in_flight")
		return
	}
	if !h.allowSpawnLocked() {
		h.mu.Unlock()
		h.skip(taskID, "rate_limited")
		return
	}
	// Reserve the slot before spawning so a racing second flip is rejected.
	h.inflight[taskID] = ""
	h.recent = append(h.recent, h.now())
	h.mu.Unlock()

	prompt := h.buildPrompt(t)
	ag, err := h.agents.Run(agent.RunConfig{
		TaskID:             taskID,
		Name:               agent.RoleHumanReview.AgentName(t.Title),
		Mode:               "headless",
		Provider:           "claude",
		Model:              h.cfg.HumanReviewModel(),
		Prompt:             prompt,
		Dir:                h.cfg.HumanReview.SybraRepoDir,
		RequirePermissions: false,
		OneShot:            true,
	})
	if err != nil {
		h.mu.Lock()
		delete(h.inflight, taskID)
		h.mu.Unlock()
		h.logger.Error("human-review.spawn", "task_id", taskID, "err", err)
		h.logAudit(audit.EventHumanReviewSkipped, taskID, "", map[string]any{"reason": "spawn_failed", "err": err.Error()})
		return
	}
	h.mu.Lock()
	h.inflight[taskID] = ag.ID
	h.mu.Unlock()

	if err := h.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Role:      string(agent.RoleHumanReview),
		Mode:      "headless",
		Provider:  ag.Provider,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
		Prompt:    prompt,
	}); err != nil {
		h.logger.Error("human-review.add-run", "task_id", taskID, "agent_id", ag.ID, "err", err)
	}
	h.logAudit(audit.EventHumanReviewSpawned, taskID, ag.ID, map[string]any{
		"prev_status": prevStatus, "model": h.cfg.HumanReviewModel(),
	})
}

// onComplete is the routing target inside App.onAgentComplete for runs whose
// role is RoleHumanReview. It parses the verdict and applies side-effects.
func (h *humanReviewHandler) onComplete(ag *agent.Agent) {
	if h == nil || ag == nil {
		return
	}
	taskID := ag.TaskID
	defer func() {
		h.mu.Lock()
		delete(h.inflight, taskID)
		h.mu.Unlock()
	}()

	final := finalAssistantText(ag)
	v, parseErr := parseVerdict(final)
	if parseErr != nil {
		h.logger.Warn("human-review.verdict.parse", "task_id", taskID, "agent_id", ag.ID, "err", parseErr)
		h.appendNote(taskID, "Auto-review (unparseable verdict)", final)
		h.logAudit(audit.EventHumanReviewVerdict, taskID, ag.ID, map[string]any{"decision": "unparseable"})
		return
	}
	h.logAudit(audit.EventHumanReviewVerdict, taskID, ag.ID, map[string]any{
		"decision": v.Decision, "summary": v.Summary,
	})

	switch v.Decision {
	case "human":
		h.appendNote(taskID, "Auto-review verdict: needs human", v.Summary)
	case "sybra_bug":
		h.fileIssue(taskID, ag.ID, v)
	default:
		h.logger.Warn("human-review.verdict.unknown", "task_id", taskID, "decision", v.Decision)
		h.appendNote(taskID, "Auto-review (unknown decision)", final)
	}
}

func (h *humanReviewHandler) fileIssue(taskID, agentID string, v verdictDecision) {
	if strings.TrimSpace(v.IssueTitle) == "" || strings.TrimSpace(v.IssueBody) == "" {
		h.logger.Warn("human-review.issue.empty", "task_id", taskID, "agent_id", agentID)
		h.appendNote(taskID, "Auto-review verdict: sybra_bug (no issue payload)", v.Summary)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	created, url, err := h.sink.SubmitIssue(ctx, v.IssueTitle, v.IssueBody, v.IssueLabels)
	if err != nil {
		h.logger.Error("human-review.issue.submit", "task_id", taskID, "agent_id", agentID, "err", err)
		// On rate limit or transient failure, leave the task in human-required
		// and append the verdict so the user has the diagnosis text.
		h.appendNote(taskID, "Auto-review verdict: sybra_bug (issue submission failed)", v.Summary+"\n\nError: "+err.Error())
		return
	}
	h.logAudit(audit.EventHumanReviewIssue, taskID, agentID, map[string]any{
		"created": created, "url": url, "title": v.IssueTitle,
	})

	body := fmt.Sprintf("**Linked Sybra bug:** %s\n\n%s", urlOrPlaceholder(url, v.IssueTitle), v.Summary)
	header := "Auto-review verdict: blocked by Sybra bug"
	if !created {
		header = "Auto-review verdict: blocked by existing Sybra bug"
	}
	t, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.task.get-after-submit", "task_id", taskID, "err", err)
		return
	}
	newBody := appendSection(t.Body, header, body)
	upd := task.Update{
		Body:           &newBody,
		Status:         task.Ptr(task.StatusBlocked),
		StatusReason:   task.Ptr(fmt.Sprintf("auto-review: %s (%s)", v.Summary, urlOrPlaceholder(url, v.IssueTitle))),
		BlockedByIssue: task.Ptr(url),
	}
	if _, err := h.tasks.Update(taskID, upd); err != nil {
		h.logger.Error("human-review.task.update", "task_id", taskID, "err", err)
	}
}

func (h *humanReviewHandler) appendNote(taskID, header, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	t, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.append.task-get", "task_id", taskID, "err", err)
		return
	}
	newBody := appendSection(t.Body, header, body)
	if _, err := h.tasks.Update(taskID, task.Update{Body: &newBody}); err != nil {
		h.logger.Error("human-review.append.task-update", "task_id", taskID, "err", err)
	}
}

func (h *humanReviewHandler) skip(taskID, reason string) {
	h.logger.Info("human-review.skip", "task_id", taskID, "reason", reason)
	h.logAudit(audit.EventHumanReviewSkipped, taskID, "", map[string]any{"reason": reason})
}

// allowSpawnLocked must be called with h.mu held. Trims expired entries from
// h.recent and returns false if the configured rate is exceeded.
func (h *humanReviewHandler) allowSpawnLocked() bool {
	limit := h.cfg.HumanReviewMaxPerHour()
	if limit <= 0 {
		return false
	}
	cutoff := h.now().Add(-humanReviewWindow)
	kept := h.recent[:0]
	for _, ts := range h.recent {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	h.recent = kept
	return len(h.recent) < limit
}

func (h *humanReviewHandler) logAudit(evt, taskID, agentID string, data map[string]any) {
	if h.audit == nil {
		return
	}
	if err := h.audit.Log(audit.Event{
		Timestamp: h.now(),
		Type:      evt,
		TaskID:    taskID,
		AgentID:   agentID,
		Data:      data,
	}); err != nil {
		h.logger.Warn("human-review.audit.log", "type", evt, "task_id", taskID, "err", err)
	}
}

// buildPrompt assembles the review agent's instructions and context. The
// agent runs in the Sybra source tree with skip-permissions, so it can
// freely grep the codebase + read host logs to diagnose the transition.
func (h *humanReviewHandler) buildPrompt(t task.Task) string {
	var b strings.Builder
	b.WriteString("# Sybra auto-review of human-required transition\n\n")
	b.WriteString("You are a diagnostic agent for Sybra (the desktop task orchestrator). A user task just transitioned to status=human-required. Decide whether this is genuinely a human-input situation or whether Sybra itself misbehaved (workflow misfire, agent mis-config, infrastructure flakiness, code bug). If it's a Sybra bug, prepare a GitHub issue payload — the host process will file it.\n\n")
	b.WriteString("## Task\n")
	fmt.Fprintf(&b, "- ID: %s\n- Title: %s\n- Status: %s\n", t.ID, t.Title, t.Status)
	if t.StatusReason != "" {
		fmt.Fprintf(&b, "- Status reason: %s\n", t.StatusReason)
	}
	if t.ProjectID != "" {
		fmt.Fprintf(&b, "- Project: %s\n", t.ProjectID)
	}
	if t.PRNumber > 0 {
		fmt.Fprintf(&b, "- PR: #%d\n", t.PRNumber)
	}
	b.WriteString("\n### Task body\n")
	b.WriteString(strings.TrimSpace(t.Body))
	b.WriteString("\n\n")

	b.WriteString("## Recent agent runs\n")
	runs := t.AgentRuns
	if len(runs) > humanReviewMaxAgentRuns {
		runs = runs[len(runs)-humanReviewMaxAgentRuns:]
	}
	if len(runs) == 0 {
		b.WriteString("(none)\n")
	} else {
		for i := range runs {
			r := runs[i]
			fmt.Fprintf(&b, "\n### Run %d — role=%s mode=%s state=%s started=%s\n",
				i+1, defaultStr(r.Role, "implementation"), r.Mode, r.State, r.StartedAt.Format(time.RFC3339))
			if r.Result != "" {
				res := r.Result
				if len(res) > humanReviewMaxAgentResult {
					res = res[:humanReviewMaxAgentResult] + "\n... (truncated)"
				}
				b.WriteString("Result:\n```\n")
				b.WriteString(strings.TrimSpace(res))
				b.WriteString("\n```\n")
			}
			if r.LogFile != "" {
				fmt.Fprintf(&b, "Log file (read with Read tool, last %d turns are most useful): %s\n", humanReviewMaxAgentTurns, r.LogFile)
			}
		}
	}
	b.WriteString("\n")

	if t.Workflow != nil {
		b.WriteString("## Workflow execution\n")
		fmt.Fprintf(&b, "- Workflow ID: %s\n- Started: %s\n- Current step: %s\n",
			t.Workflow.WorkflowID, t.Workflow.StartedAt.Format(time.RFC3339), t.Workflow.CurrentStep)
		b.WriteString("\n")
	}

	if tail := tailFile(h.logFile, humanReviewLogTail); tail != "" {
		b.WriteString("## Sybra host log (tail)\n```\n")
		b.WriteString(tail)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Investigation hints\n")
	b.WriteString("- Working directory is the Sybra source tree. Use Grep/Read/Glob to inspect Go code under internal/ when a stack trace or error message points there.\n")
	fmt.Fprintf(&b, "- Audit events live under %s (newest file is today's). Run `gh issue list --repo %s --label %s` first to avoid duplicate filings.\n",
		filepath.Join(h.homeDir, "audit"), h.cfg.HumanReviewRepo(), h.cfg.HumanReviewIssueLabel())
	b.WriteString("- A genuine human-required reason looks like: scope question, creative decision, missing credentials, ambiguous requirement, or an external system the agent legitimately cannot reach.\n")
	b.WriteString("- A Sybra bug looks like: workflow step never ran the agent, agent started in the wrong dir, status flipped despite a successful PR, sidecar required but never written, repeated provider gate blocks, panics in logs, mis-routed completions.\n\n")

	b.WriteString("## Output protocol (REQUIRED)\n")
	b.WriteString("End your response with EXACTLY one fenced block tagged `sybra-verdict` containing JSON:\n\n")
	b.WriteString("```sybra-verdict\n{\n  \"decision\": \"human\" | \"sybra_bug\",\n  \"summary\": \"one-sentence diagnosis\",\n  \"issue_title\": \"type(scope): short title\",   // sybra_bug only, must follow Sybra conventional commit format (e.g. fix(workflow): ...)\n  \"issue_body\": \"## What happened\\n...\\n\\n## Repro\\n...\\n\\n## Suspected cause\\n...\",  // sybra_bug only\n  \"issue_labels\": [\"workflow\", \"bug\"]              // sybra_bug only, optional\n}\n```\n\n")
	b.WriteString("If decision=human, omit issue_* fields. The host parses this block deterministically — any other format will be treated as a parse failure.\n")
	return b.String()
}

// parseVerdict extracts and unmarshals the fenced sybra-verdict JSON block.
var verdictBlockRe = regexp.MustCompile("(?s)```\\s*sybra-verdict\\s*\\n(.*?)\\n```")

func parseVerdict(text string) (verdictDecision, error) {
	if strings.TrimSpace(text) == "" {
		return verdictDecision{}, errors.New("empty assistant text")
	}
	m := verdictBlockRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return verdictDecision{}, errors.New("no sybra-verdict block")
	}
	var v verdictDecision
	if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v); err != nil {
		return verdictDecision{}, fmt.Errorf("verdict json: %w", err)
	}
	v.Decision = strings.TrimSpace(strings.ToLower(v.Decision))
	v.Summary = strings.TrimSpace(v.Summary)
	if v.Decision != "human" && v.Decision != "sybra_bug" {
		return verdictDecision{}, fmt.Errorf("invalid decision %q", v.Decision)
	}
	return v, nil
}

// finalAssistantText concatenates the assistant text from the agent's stream.
// The fenced verdict block always lives in the last assistant turn.
func finalAssistantText(ag *agent.Agent) string {
	out := ag.Output()
	for i := range slices.Backward(out) {
		if out[i].Type == "assistant" && strings.Contains(out[i].Content, "sybra-verdict") {
			return out[i].Content
		}
	}
	for i := range slices.Backward(out) {
		if out[i].Type == "result" {
			return out[i].Content
		}
	}
	return ""
}

// appendSection appends a `## header` section to body, separated by a blank
// line. Keeps the source body intact even if it already ends without newline.
func appendSection(body, header, content string) string {
	body = strings.TrimRight(body, "\n")
	if body != "" {
		body += "\n\n"
	}
	body += "## " + header + "\n\n" + strings.TrimSpace(content) + "\n"
	return body
}

func defaultStr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func urlOrPlaceholder(url, title string) string {
	if url != "" {
		return url
	}
	return "(issue: " + title + ")"
}

// tailFile reads the last n lines of path. Best-effort: returns "" on error
// or when the file is missing (server containers often don't ship the log).
func tailFile(path string, n int) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
