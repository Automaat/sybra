package sybra

import (
	"context"
	"log/slog"
	"slices"
	"strings"
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
}

func newPromptLabCoordinator(
	tasks *task.Manager,
	projects *project.Store,
	statsStore *stats.Store,
	logger *slog.Logger,
	cfg *config.Config,
	allowsProjectType func(project.ProjectType) bool,
	scrubContext func(string) *WorkScrubContext,
) *promptLabCoordinator {
	return &promptLabCoordinator{
		tasks:             tasks,
		projects:          projects,
		stats:             statsStore,
		logger:            logger,
		cfg:               cfg,
		allowsProjectType: allowsProjectType,
		scrubContext:      scrubContext,
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
	filed, err := c.fileScrubbedProposals(result)
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
func (c *promptLabCoordinator) fileScrubbedProposals(result promptlab.RunResult) ([]task.Task, error) {
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
		if _, ok := findExistingPromptLabProposal(existing, p.ID); ok {
			continue
		}
		body := c.scrubBody(promptlab.RenderProposalBody(p), p.Evidence.ProjectIDs)
		tags := []string{"prompt-lab-proposal", "role:" + p.Subject.Role}
		status := task.StatusTodo
		if p.RequiresHumanApproval {
			tags = append(tags, "requires-human")
			status = task.StatusHumanRequired
		}
		created, err := c.tasks.CreateFull(p.Title, body, task.AgentModeInteractive, task.Update{
			Status: &status,
			Tags:   &tags,
		})
		if err != nil {
			return filed, err
		}
		filed = append(filed, created)
		existing = append(existing, created)
	}
	return filed, nil
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

func findExistingPromptLabProposal(tasks []task.Task, proposalID string) (task.Task, bool) {
	marker := "Proposal ID:** `" + proposalID + "`"
	for i := range tasks {
		if task.IsTerminalStatus(tasks[i].Status) {
			continue
		}
		if !slices.Contains(tasks[i].Tags, "prompt-lab-proposal") {
			continue
		}
		if strings.Contains(tasks[i].Body, marker) {
			return tasks[i], true
		}
	}
	return task.Task{}, false
}

// initPromptLab constructs the promptlab coordinator. Cheap to always build
// (mirrors renovateCoordinator): the ticker itself no-ops when disabled, so
// the coordinator can be built unconditionally without an extra guard here.
func (a *App) initPromptLab() {
	a.promptLab = newPromptLabCoordinator(a.tasks, a.projects, a.stats, a.logger, a.cfg, a.allowsProjectType, a.workScrubContextForTask)
}
