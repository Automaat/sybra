package review

import (
	"context"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// Keep asynchronous CI in the existing PR monitor, not a second workflow
// polling loop. Read only trusted default-branch policy, never the PR's own
// .sybra.yaml. An unreadable policy/check snapshot fails closed.
func (r *Handler) factoryCIReady(ctx context.Context, t task.Task, pr github.PullRequest) bool {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	policy, err := r.factoryCIPolicy(ctx, t)
	if err != nil {
		r.logger.Warn("factory.ci.policy-unavailable", "task_id", t.ID, "err", err)
		return false
	}
	if policy == nil || !policy.Enabled {
		return true
	}
	fetch := r.fetchCIGate
	if fetch == nil {
		fetch = github.FetchPRCIGate
	}
	gate, err := fetch(ctx, t.ProjectID, pr.Number, policy.RequiredChecks)
	if err != nil || pr.HeadSHA == "" || gate.SHA != pr.HeadSHA || !gate.Approved() {
		r.logger.Info("factory.ci.waiting", "task_id", t.ID, "head", pr.HeadSHA,
			"missing", gate.Missing, "pending", gate.Pending, "failed", gate.Failed, "err", err)
		return false
	}
	r.logAudit("factory.ci.verified", t.ID, "", map[string]any{
		"head_sha": gate.SHA, "required_checks": gate.Succeeded, "pr": pr.Number,
	})
	return true
}

func (r *Handler) factoryCIPolicy(ctx context.Context, t task.Task) (*project.CIConfig, error) {
	if r.projects == nil {
		return nil, fmt.Errorf("project store unavailable")
	}
	proj, err := r.projects.Get(t.ProjectID)
	if err != nil {
		return nil, err
	}
	var repoChecks *project.ChecksConfig
	if proj.ClonePath != "" {
		cfg, err := project.LoadRepoConfigAtDefaultBranch(ctx, proj.ClonePath)
		if err != nil {
			return nil, err
		}
		repoChecks = cfg.Checks
	}
	checks := project.MergeChecks(repoChecks, proj.Checks)
	if checks == nil {
		return &project.CIConfig{}, nil
	}
	return checks.CI, nil
}
