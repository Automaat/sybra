package sybra

import (
	"context"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

// promptLabCoordinator runs the deterministic (non-LLM) Prompt Lab ticker:
// collect -> propose -> evaluate -> file reviewed local tasks. Gated on
// cfg.PromptLab.Enabled and, per weak subject, on AllowsProjectType so a
// machine that doesn't own a subject's originating project never files a
// proposal derived from it.
type promptLabCoordinator struct {
	tasks             *task.Manager
	projects          *project.Store
	stats             *stats.Store
	logger            *slog.Logger
	cfg               *config.Config
	allowsProjectType func(project.ProjectType) bool
	scrubContext      func(projectID string) *WorkScrubContext
	approve           func(taskID string) error
}

const promptLabProjectID = "Automaat/sybra"

const promptLabNoProjectReason = "Prompt Lab proposal not auto-approved: project " + promptLabProjectID +
	" is not registered, so the authoring workflow has no repo to open a worktree in. Register it, then approve."

const promptLabNoProjectErr = "prompt-lab approval unavailable: project " + promptLabProjectID +
	" is not registered, so the authoring workflow has no repo to open a worktree in"

func newPromptLabCoordinator(
	tasks *task.Manager,
	projects *project.Store,
	statsStore *stats.Store,
	logger *slog.Logger,
	cfg *config.Config,
	allowsProjectType func(project.ProjectType) bool,
	scrubContext func(string) *WorkScrubContext,
	approve func(string) error,
) *promptLabCoordinator {
	return &promptLabCoordinator{
		tasks:             tasks,
		projects:          projects,
		stats:             statsStore,
		logger:            logger,
		cfg:               cfg,
		allowsProjectType: allowsProjectType,
		scrubContext:      scrubContext,
		approve:           approve,
	}
}

// run ticks on the configured interval, computing and filing proposals each
// time. No-op when disabled. Returns when ctx is cancelled.
func (c *promptLabCoordinator) run(ctx context.Context) {
	if !c.cfg.PromptLab.Enabled {
		c.logger.Info("promptlab.disabled")
		return
	}
	interval := time.Duration(c.cfg.PromptLab.IntervalHours * float64(time.Hour))
	if interval < time.Hour {
		interval = 24 * time.Hour
	}
	c.logger.Info("promptlab.start", "interval", interval.String())

	c.tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

func (c *promptLabCoordinator) tick(ctx context.Context) {
	var records []stats.RunRecord
	if c.stats != nil {
		records = c.stats.All()
	}
	lookback := time.Duration(c.cfg.PromptLab.LookbackHours * float64(time.Hour))
	result, err := promptlab.Run(ctx, promptlab.Options{
		Records:       records,
		OutputDir:     config.PromptLabDir(),
		Lookback:      lookback,
		MinSamples:    c.cfg.PromptLab.MinSamples,
		MinEffectSize: c.cfg.PromptLab.MinEffectSize,
		MaxProposals:  c.cfg.PromptLab.MaxProposalsPerRun,
		Logger:        c.logger,
	})
	if err != nil {
		c.logger.Warn("promptlab.tick.failed", "err", err)
		return
	}
	filed, err := c.fileScrubbedProposals(ctx, result)
	if err != nil {
		c.logger.Warn("promptlab.file.failed", "err", err)
		return
	}
	c.logger.Info("promptlab.tick",
		"weak_subjects", len(result.WeakSubjects),
		"proposals", len(result.Proposals),
		"filed", len(filed))
}

// fileScrubbedProposals files each proposal as a reviewed local task, gated
// per subject on AllowsProjectType and scrubbed via scrubContext when the
// subject's evidence traces back to a work-typed project. Scrub is EXPLICIT
// here, not inherited: internal/harnessevolution's filer does not scrub, so
// this coordinator builds and applies the blocklist itself rather than
// assuming that precedent covers it too.
func (c *promptLabCoordinator) fileScrubbedProposals(ctx context.Context, result promptlab.RunResult) ([]task.Task, error) {
	existing, err := c.tasks.List()
	if err != nil {
		return nil, err
	}
	var filed []task.Task
	for i := range result.Proposals {
		p := result.Proposals[i]
		if !c.allowsAnyProject(p.Evidence.ProjectIDs) {
			continue
		}
		cooldown := time.Duration(c.cfg.PromptLab.RefileCooldownDays * float64(24*time.Hour))
		if promptlab.HasProposal(existing, p.ID, cooldown, time.Now().UTC()) {
			continue
		}
		body := c.scrubBody(promptlab.RenderProposalBody(p), p.Evidence.ProjectIDs)
		tags := []string{promptlab.ProposalTag, "role:" + p.Subject.Role}
		status := task.StatusTodo
		if p.RequiresHumanApproval {
			tags = append(tags, "requires-human")
			status = task.StatusHumanRequired
		}
		update := task.Update{
			Status: &status,
			Tags:   &tags,
		}
		if projectID := promptLabTargetProjectID(c.projects); projectID != "" {
			update.ProjectID = &projectID
		}
		created, err := c.tasks.CreateFull(p.Title, body, task.AgentModeHeadless, update)
		if err != nil {
			return filed, err
		}
		c.maybeAutoApprove(ctx, created, p)
		filed = append(filed, created)
		existing = append(existing, created)
	}
	return filed, nil
}

func (c *promptLabCoordinator) maybeAutoApprove(ctx context.Context, t task.Task, p promptlab.Proposal) {
	if c.approve == nil || !c.cfg.PromptLabAutoApprove() {
		return
	}
	if ctx.Err() != nil {
		c.logger.Warn("promptlab.auto_approve.skipped_shutdown", "task_id", t.ID, "proposal_id", p.ID)
		return
	}
	if !isPendingProposalStatus(t.Status) || p.Offline.Verdict == promptlab.VerdictFailed {
		return
	}
	if t.ProjectID == "" {
		c.logger.Warn("promptlab.auto_approve.skipped_no_project", "task_id", t.ID, "proposal_id", p.ID)
		c.setStatusReason(t.ID, promptLabNoProjectReason)
		return
	}
	if err := c.approve(t.ID); err != nil {
		c.logger.Warn("promptlab.auto_approve.failed", "task_id", t.ID, "proposal_id", p.ID, "err", err)
		return
	}
	c.logger.Info("promptlab.auto_approve", "task_id", t.ID, "proposal_id", p.ID)
}

func (c *promptLabCoordinator) setStatusReason(taskID, reason string) {
	if _, err := c.tasks.Update(taskID, task.Update{StatusReason: &reason}); err != nil {
		c.logger.Warn("promptlab.status_reason.failed", "task_id", taskID, "err", err)
	}
}

func promptLabTargetProjectID(projects *project.Store) string {
	if projects == nil {
		return ""
	}
	if _, err := projects.Get(promptLabProjectID); err == nil {
		return promptLabProjectID
	}
	return ""
}

// allowsAnyProject reports whether this machine should file a proposal
// backed by the given evidence project IDs. A subject with no known project
// (fleet-wide evidence, not traced to one project) — or one whose project
// IDs no longer resolve (e.g. a deleted project) — is treated as pet-typed —
// the least restrictive routing — so it is never silently dropped for lack
// of attribution.
func (c *promptLabCoordinator) allowsAnyProject(projectIDs []string) bool {
	resolved := false
	for _, id := range projectIDs {
		p, err := c.projects.Get(id)
		if err != nil {
			continue
		}
		resolved = true
		if c.allowsProjectType(p.Type) {
			return true
		}
	}
	if resolved {
		return false
	}
	return c.allowsProjectType(project.ProjectTypePet)
}

func (c *promptLabCoordinator) scrubBody(body string, projectIDs []string) string {
	for _, id := range projectIDs {
		if ctx := c.scrubContext(id); ctx != nil {
			body, _ = scrub.Scrub(body, ctx.Blocklist)
		}
	}
	return body
}

// initPromptLab constructs the promptlab coordinator. Cheap to always build
// (mirrors renovateCoordinator): the ticker itself no-ops when disabled, so
// the coordinator can be built unconditionally without an extra guard here.
func (a *App) initPromptLab() {
	a.promptLab = newPromptLabCoordinator(
		a.tasks, a.projects, a.stats, a.logger, a.cfg,
		a.allowsProjectType, a.workScrubContextForTask,
		func(taskID string) error { return a.promptLabSvc.autoApprove(taskID) },
	)
}
