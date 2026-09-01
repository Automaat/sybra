// Package reconciliation observes application state and drives the pure
// internal/reconcile decision table. It is nested under internal/sybra because
// observation still touches App-adjacent task, project, worktree, and GitHub
// dependencies.
package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/reconcile"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

const maxReobserveAttempts = 5

var (
	refreshedRemoteTrackingSHA = project.RefreshedRemoteTrackingSHA
	reconcileGitConfigOutput   = gitexec.Output
)

type Config struct {
	Tasks     *task.Manager
	Projects  *project.Store
	Worktrees *worktree.Manager
	Logger    *slog.Logger
	Evidence  *evidence.Store
	Audit     func(reconcile.Request, reconcile.Plan)

	FetchPRState   func(string, int) (github.PRState, error)
	FetchPRHeadSHA func(string, int) (string, error)
	FetchPRMeta    func(context.Context, string, int) (github.PullRequest, error)
}

type Reconciler struct {
	tasks     *task.Manager
	projects  *project.Store
	worktrees *worktree.Manager
	logger    *slog.Logger
	evidence  *evidence.Store
	audit     func(reconcile.Request, reconcile.Plan)
	prState   func(string, int) (github.PRState, error)
	prHead    func(string, int) (string, error)
	prMeta    func(context.Context, string, int) (github.PullRequest, error)
}

func New(cfg Config) *Reconciler {
	state := cfg.FetchPRState
	if state == nil {
		state = github.FetchPRState
	}
	head := cfg.FetchPRHeadSHA
	if head == nil {
		head = github.FetchPRHeadSHA
	}
	meta := cfg.FetchPRMeta
	if meta == nil {
		meta = github.FetchPRMetaContext
	}
	return &Reconciler{tasks: cfg.Tasks, projects: cfg.Projects, worktrees: cfg.Worktrees, logger: cfg.Logger, evidence: cfg.Evidence, audit: cfg.Audit, prState: state, prHead: head, prMeta: meta}
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Plan, error) {
	for range maxReobserveAttempts {
		snapshot, err := r.observe(ctx, req, "")
		if err != nil {
			return reconcile.Plan{}, err
		}
		plan := reconcile.Decide(snapshot.Snapshot)
		if plan.Action != reconcile.ActionCheckpoint && plan.Action != reconcile.ActionPush && plan.Action != reconcile.ActionAdoptRemote {
			r.log(req, plan)
			return plan, nil
		}

		fresh, err := r.observe(ctx, req, "")
		if err != nil {
			return reconcile.Plan{}, err
		}
		freshPlan := reconcile.Decide(fresh.Snapshot)
		if freshPlan.Action != plan.Action || !samePreconditions(plan.Preconditions, freshPlan.Preconditions) {
			continue
		}
		switch plan.Action {
		case reconcile.ActionCheckpoint:
			_, err = project.CheckpointCommit(ctx, fresh.GitPath(), "wip: reconcile completed agent work\n\nSybra preserved observed author changes before post-run routing.")
			if err != nil {
				return reconcile.Plan{}, fmt.Errorf("reconcile checkpoint: %w", err)
			}
		case reconcile.ActionPush:
			if err = project.PushSync(ctx, fresh.GitPath(), fresh.Git.Branch); err != nil {
				return reconcile.Plan{}, fmt.Errorf("reconcile push: %w", err)
			}
		case reconcile.ActionAdoptRemote:
			if err = project.ReconcileWithRemote(ctx, fresh.GitPath(), fresh.Git.Branch); err != nil {
				return reconcile.Plan{}, fmt.Errorf("reconcile adopt remote: %w", err)
			}
		default:
			return reconcile.Plan{}, fmt.Errorf("reconcile: unsupported effect action %q", plan.Action)
		}
	}
	return reconcile.Plan{}, fmt.Errorf("reconcile: state changed during %d attempts", maxReobserveAttempts)
}

// CanCleanup is the destructive worktree gate. It re-observes instead of
// trusting a completion-time plan, so task generation, lease ownership, HEAD,
// remote reachability, dirty state, and operation markers are current at the
// exact cleanup attempt.
func (r *Reconciler) CanCleanup(ctx context.Context, t task.Task, path string) bool {
	snapshot, err := r.observe(ctx, reconcile.Request{TaskID: t.ID, Intent: reconcile.IntentCleanup}, path)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("post-run.cleanup.reconcile", "task_id", t.ID, "err", err)
		}
		return false
	}
	return reconcile.Decide(snapshot.Snapshot).AllowsCleanup()
}

// observedSnapshot embeds only while applying a strict checkpoint. Keeping the
// path out of reconcile.Snapshot prevents an ambient filesystem location from
// becoming part of the pure decision contract or an idempotency key.
type observedSnapshot struct {
	reconcile.Snapshot
	gitPath string
}

func (s observedSnapshot) GitPath() string { return s.gitPath }

func (r *Reconciler) observe(ctx context.Context, req reconcile.Request, pathOverride string) (observedSnapshot, error) {
	if r == nil || r.tasks == nil {
		return observedSnapshot{}, fmt.Errorf("reconcile: task store is unavailable")
	}
	t, err := r.tasks.Get(req.TaskID)
	if err != nil {
		return observedSnapshot{}, fmt.Errorf("reconcile task: %w", err)
	}
	s := reconcile.Snapshot{Intent: req.Intent, TaskID: t.ID, TaskGeneration: t.Generation, WorkflowGeneration: t.Generation}
	s.Sidecars = observedSidecars(t)
	if r.evidence != nil {
		completionEvidence, evidenceErr := r.evidence.Load(t.ID)
		if evidenceErr != nil {
			return observedSnapshot{}, fmt.Errorf("reconcile verification evidence: %w", evidenceErr)
		}
		s.Evidence = observedEvidence(completionEvidence)
	}
	if t.Workflow != nil {
		s.WorkflowID = t.Workflow.WorkflowID
		s.WorkflowStep = t.Workflow.CurrentStep
		if req.RunID != "" {
			s.Lease = reconcile.LeaseState{ID: req.RunID, Required: true}
		}
		if step, ok := t.Workflow.AgentRoute(req.RunID); ok {
			s.Lease.Current = step == t.Workflow.CurrentStep
		}
	}
	for i := range t.AgentRuns {
		run := &t.AgentRuns[i]
		if run.AgentID != req.RunID {
			continue
		}
		s.Run = reconcile.RunState{ID: run.AgentID, Role: run.Role, Terminal: run.Outcome != "", Success: run.Outcome == task.RunOutcomeSuccess}
		break
	}
	if req.Intent == reconcile.IntentCleanup && s.Run.ID == "" {
		s.Run.Terminal = true
		s.Run.Success = true
	}

	path := pathOverride
	if path == "" {
		path = t.WorktreeDir
	}
	if path == "" && r.worktrees != nil {
		path = r.worktrees.PathFor(t)
	}
	var prHead authoritativePRHead
	if t.PRNumber > 0 {
		pr, prErr := r.prState(t.ProjectID, t.PRNumber)
		if prErr != nil {
			return observedSnapshot{}, fmt.Errorf("reconcile PR state: %w", prErr)
		}
		head, headErr := r.prHead(t.ProjectID, t.PRNumber)
		if headErr != nil {
			return observedSnapshot{}, fmt.Errorf("reconcile PR head: %w", headErr)
		}
		prHead.sha = head
		if r.prMeta != nil {
			if meta, metaErr := r.prMeta(ctx, t.ProjectID, t.PRNumber); metaErr == nil {
				prHead.repo = meta.HeadRepo
				prHead.branch = meta.HeadRefName
			} else if r.logger != nil {
				r.logger.Warn("reconcile.pr-meta", "task_id", t.ID, "pr", t.PRNumber, "err", metaErr)
			}
		}
		s.PR = reconcile.PRState{Number: t.PRNumber, State: pr.State, HeadSHA: head, Mergeable: pr.Mergeable, Checks: pr.CIStatus()}
	}
	if path != "" {
		if _, statErr := os.Stat(path); statErr == nil {
			git, gitErr := r.observeGit(ctx, &t, path, prHead)
			if gitErr != nil {
				return observedSnapshot{}, gitErr
			}
			s.Git = git
		}
	}
	return observedSnapshot{Snapshot: s, gitPath: path}, nil
}

func observedSidecars(t task.Task) []reconcile.SidecarState {
	contents := []struct {
		name    string
		content string
	}{
		{"plan", t.Plan},
		{"plan-contract", t.PlanContract},
		{"plan-critique", t.PlanCritique},
		{"plan-research", t.PlanResearch},
		{"plan-decisions", t.PlanDecisions},
		{"plan-brief", t.PlanBrief},
		{"code-review", t.CodeReview},
		{"current-test-failures", t.CurrentTestFailures},
		{"acceptance-ledger", t.AcceptanceLedger},
		{"spec-decision", t.SpecDecision},
	}
	items := make([]reconcile.SidecarState, 0, len(contents)+len(t.PlanDrafts))
	for _, sidecar := range contents {
		if sidecar.content != "" {
			items = append(items, reconcile.SidecarState{Name: sidecar.name, Digest: contentDigest(sidecar.content)})
		}
	}
	for name, content := range t.PlanDrafts {
		if content != "" {
			items = append(items, reconcile.SidecarState{Name: "plan-draft:" + name, Digest: contentDigest(content)})
		}
	}
	return items
}

func observedEvidence(completionEvidence evidence.CompletionEvidence) reconcile.EvidenceState {
	state := reconcile.EvidenceState{Verified: len(completionEvidence.Criteria) > 0}
	for i := range completionEvidence.Criteria {
		criterion := &completionEvidence.Criteria[i]
		item := reconcile.EvidenceItem{Criterion: criterion.Criterion, SourceSHA: criterion.FinalRev, Passed: criterion.Passed()}
		state.Items = append(state.Items, item)
		if !item.Passed || item.SourceSHA == "" {
			state.Verified = false
		}
		if state.SourceSHA == "" {
			state.SourceSHA = item.SourceSHA
		} else if state.SourceSHA != item.SourceSHA {
			state.SourceSHA = ""
			state.Verified = false
		}
	}
	return state
}

func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

type authoritativePRHead struct {
	repo   string
	branch string
	sha    string
}

func (r *Reconciler) observeGit(ctx context.Context, t *task.Task, path string, prHead authoritativePRHead) (reconcile.GitState, error) {
	g := reconcile.GitState{Available: true, Branch: t.Branch, PushForbidden: t.IsPRReview(), Healthy: project.WorktreeHealthy(ctx, path)}
	if !g.Healthy {
		return g, nil
	}
	head, err := project.CurrentCommit(ctx, path)
	if err != nil {
		return g, fmt.Errorf("reconcile HEAD: %w", err)
	}
	g.HeadSHA = head
	g.HeadExists = gitexec.RunQuiet(ctx, gitexec.Options{Dir: path}, "cat-file", "-e", head+"^{commit}") == nil
	dirty, err := project.IsWorktreeDirty(ctx, path)
	if err != nil {
		return g, fmt.Errorf("reconcile dirty state: %w", err)
	}
	g.Dirty = dirty
	g.Staged = gitexec.RunQuiet(ctx, gitexec.Options{Dir: path}, "diff", "--cached", "--quiet") != nil
	g.Operation = project.WorktreeOperation(ctx, path)

	g.PRHeadSHA = prHead.sha
	if sha, exists, err := authoritativeRemoteSHA(ctx, path, t, prHead); err != nil {
		return g, fmt.Errorf("reconcile remote branch: %w", err)
	} else if exists {
		g.RemoteSHA = sha
		g.Behind, g.Ahead, err = aheadBehind(ctx, path, sha, head)
		if err != nil {
			return g, fmt.Errorf("reconcile remote reachability: %w", err)
		}
	}
	if r.projects != nil && t.ProjectID != "" {
		proj, err := r.projects.Get(t.ProjectID)
		if err != nil {
			return g, fmt.Errorf("reconcile project: %w", err)
		}
		baseBranch, err := project.DefaultBranch(ctx, proj.ClonePath)
		if err != nil {
			return g, fmt.Errorf("reconcile base branch: %w", err)
		}
		if base, ok := project.ResolveBareRef(ctx, proj.ClonePath, "refs/remotes/origin/"+baseBranch); ok {
			g.BaseSHA = base
			_, baseAhead, reachabilityErr := aheadBehind(ctx, path, base, head)
			if reachabilityErr != nil {
				return g, fmt.Errorf("reconcile base reachability: %w", reachabilityErr)
			}
			g.TaskWorkReachable = head != base && (baseAhead > 0 || g.RemoteSHA == head || g.PRHeadSHA == head)
			g.TreeEquivalentToBase = gitexec.RunQuiet(ctx, gitexec.Options{Dir: path}, "diff", "--quiet", base, head) == nil
		}
	}
	return g, nil
}

func authoritativeRemoteSHA(ctx context.Context, path string, t *task.Task, prHead authoritativePRHead) (string, bool, error) {
	if remote, branch, ok := authoritativePRTrackingRef(ctx, path, t, prHead); ok {
		return refreshedRemoteTrackingSHA(ctx, path, remote, branch)
	}
	if prHead.sha != "" {
		return prHead.sha, true, nil
	}
	if t.Branch == "" {
		return "", false, nil
	}
	return refreshedRemoteTrackingSHA(ctx, path, project.PushRemote(ctx, path), t.Branch)
}

func authoritativePRTrackingRef(ctx context.Context, path string, t *task.Task, prHead authoritativePRHead) (remote, branch string, ok bool) {
	branch = prHead.branch
	if branch == "" {
		branch = t.Branch
	}
	if branch == "" {
		return "", "", false
	}
	if prHead.repo != "" {
		switch {
		case strings.EqualFold(prHead.repo, t.ProjectID):
			return "origin", branch, true
		case remoteRepoMatches(ctx, path, "fork", prHead.repo):
			return "fork", branch, true
		}
	}
	return "", "", false
}

func remoteRepoMatches(ctx context.Context, path, remote, repo string) bool {
	raw, err := reconcileGitConfigOutput(ctx, gitexec.Options{Dir: path}, "config", "--get", "remote."+remote+".url")
	if err != nil {
		return false
	}
	owner, name, err := project.ParseGitHubURL(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return strings.EqualFold(owner+"/"+name, repo)
}

func aheadBehind(ctx context.Context, path, left, right string) (behind, ahead int, err error) {
	out, err := gitexec.Output(ctx, gitexec.Options{Dir: path}, "rev-list", "--left-right", "--count", left+"..."+right)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(out)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	behind, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse behind count %q: %w", parts[0], err)
	}
	ahead, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse ahead count %q: %w", parts[1], err)
	}
	return behind, ahead, nil
}

func samePreconditions(a, b reconcile.Preconditions) bool { return a == b }

func (r *Reconciler) log(req reconcile.Request, plan reconcile.Plan) {
	if r.logger != nil {
		r.logger.Info("post-run.reconciled", "task_id", req.TaskID, "run_id", req.RunID, "intent", req.Intent, "action", plan.Action, "reason", plan.Reason)
	}
	if r.audit != nil {
		r.audit(req, plan)
	}
}
