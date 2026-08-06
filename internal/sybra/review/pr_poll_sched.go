package review

import (
	"cmp"
	"context"
	"slices"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

type knownPRPollSelection struct {
	tasks       []task.Task
	selectedPRs int
	deferredPRs int
	cappedPRs   int
	// retainKeys are the PRs this tick deliberately skipped (deferred by
	// backoff or cut by the per-tick cap). They are absent from `tasks`, so
	// without listing them here Prune would delete the very skip counters that
	// deferred them and the backoff could never last more than one tick.
	retainKeys []string
}

func expBackoff(streak, maxTicks int) int {
	if streak <= 0 || maxTicks <= 0 {
		return 0
	}
	skip := 1
	for i := 1; i < streak && skip < maxTicks; i++ {
		skip *= 2
		if skip >= maxTicks {
			return maxTicks
		}
	}
	return skip
}

func (r *Handler) selectKnownPRPoll(ctx context.Context, tasks []task.Task) knownPRPollSelection {
	active := make([]task.Task, 0, len(tasks))
	passthrough := make([]task.Task, 0, len(tasks))
	candidates := make([]task.Task, 0, len(tasks))
	var retainKeys []string
	activePRs := 0
	deferred := 0

	for i := range tasks {
		tk := tasks[i]
		if r.hasBlockingAgentForTask(ctx, tk.ID) {
			active = append(active, tk)
			if knownPRPollEligible(&tk) {
				activePRs++
			}
			continue
		}
		if !knownPRPollEligible(&tk) {
			passthrough = append(passthrough, tk)
			continue
		}

		key := prRefCacheKey(tk.ProjectID, tk.PRNumber)
		_, _, skipTicks, _ := r.prSnapshots.Backoff(key)
		if r.prSnapshots.TaskStatusAdvancedSince(key, tk.StatusChangedAt) {
			r.prSnapshots.ResetBackoff(key)
			skipTicks = 0
		}
		if skipTicks > 0 {
			if r.knownPRStillStableDuringBackoff(&tk, key) {
				deferred++
				retainKeys = append(retainKeys, key)
				continue
			}
		}
		r.prSnapshots.NoteTaskStatus(key, tk.StatusChangedAt)
		candidates = append(candidates, tk)
	}

	slices.SortFunc(candidates, func(a, b task.Task) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	budget := len(candidates)
	maxPRsPerTick := r.reviewsMaxPRsPerTick()
	if maxPRsPerTick > 0 {
		budget = min(max(maxPRsPerTick-activePRs, 0), len(candidates))
	}

	selected := make([]task.Task, 0, len(active)+len(passthrough)+budget)
	selected = append(selected, active...)
	selected = append(selected, passthrough...)
	selected = append(selected, candidates[:budget]...)
	for i := range candidates[budget:] {
		capped := &candidates[budget+i]
		retainKeys = append(retainKeys, prRefCacheKey(capped.ProjectID, capped.PRNumber))
	}

	return knownPRPollSelection{
		tasks:       selected,
		retainKeys:  retainKeys,
		selectedPRs: activePRs + budget,
		deferredPRs: deferred,
		cappedPRs:   len(candidates) - budget,
	}
}

func knownPRPollEligible(t *task.Task) bool {
	if t.PRNumber == 0 || t.ProjectID == "" {
		return false
	}
	return prMonitorEligible(t) || prClosedEligible(t) || humanRequiredBlockerReconcileEligible(t)
}

func (r *Handler) knownPRStillStableDuringBackoff(t *task.Task, key string) bool {
	lastHeadSHA, lastUpdatedAt, _, _ := r.prSnapshots.Backoff(key)
	headStateFn := r.fetchHeadStateFn
	if headStateFn == nil {
		r.prSnapshots.DecrementSkipTicks(key)
		return true
	}
	sha, open, updatedAt, err := headStateFn(t.ProjectID, t.PRNumber)
	if err != nil {
		r.prSnapshots.DecrementSkipTicks(key)
		return true
	}
	if !open || sha == "" || sha != lastHeadSHA || updatedAt != lastUpdatedAt {
		r.prSnapshots.ResetBackoff(key)
		return false
	}
	r.prSnapshots.DecrementSkipTicks(key)
	return true
}

func (r *Handler) noteKnownPRResult(repo string, number int, pr github.PullRequest) {
	if repo == "" || number == 0 {
		return
	}
	key := prRefCacheKey(repo, number)
	r.prSnapshots.NoteResult(key, pr.HeadSHA, pr.UpdatedAt, r.reviewsStableBackoffMaxTicks())
}

func (r *Handler) pruneKnownPRState(seen map[string]struct{}) {
	r.prSnapshots.Prune(seen)
}

func (r *Handler) reviewsMaxPRsPerTick() int {
	if r.cfg == nil {
		return config.DefaultReviewsMaxPRsPerTick
	}
	return r.cfg.ReviewsMaxPRsPerTick()
}

func (r *Handler) reviewsStableBackoffMaxTicks() int {
	if r.cfg == nil {
		return config.DefaultReviewsStableBackoffMaxTicks
	}
	return r.cfg.ReviewsStableBackoffMaxTicks()
}
