package review

import (
	"cmp"
	"slices"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

type prPollEntry struct {
	lastHeadSHA   string
	lastUpdatedAt string
	stableStreak  int
	skipTicks     int
}

type knownPRPollSelection struct {
	tasks       []task.Task
	selectedPRs int
	deferredPRs int
	cappedPRs   int
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

func (r *Handler) selectKnownPRPoll(tasks []task.Task) knownPRPollSelection {
	if r.prPollState == nil {
		r.prPollState = make(map[string]prPollEntry)
	}

	active := make([]task.Task, 0, len(tasks))
	passthrough := make([]task.Task, 0, len(tasks))
	candidates := make([]task.Task, 0, len(tasks))
	activePRs := 0
	deferred := 0

	for i := range tasks {
		tk := tasks[i]
		if r.agents != nil && r.agents.HasRunningAgentForTask(tk.ID) {
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
		entry := r.prPollState[key]
		if entry.skipTicks > 0 {
			entry.skipTicks--
			r.prPollState[key] = entry
			deferred++
			continue
		}
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

	return knownPRPollSelection{
		tasks:       selected,
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

func (r *Handler) noteKnownPRResult(repo string, number int, pr github.PullRequest) {
	if repo == "" || number == 0 {
		return
	}
	if r.prPollState == nil {
		r.prPollState = make(map[string]prPollEntry)
	}

	key := prRefCacheKey(repo, number)
	entry, ok := r.prPollState[key]
	if !ok || entry.lastHeadSHA != pr.HeadSHA || entry.lastUpdatedAt != pr.UpdatedAt {
		r.prPollState[key] = prPollEntry{
			lastHeadSHA:   pr.HeadSHA,
			lastUpdatedAt: pr.UpdatedAt,
		}
		return
	}

	entry.stableStreak++
	entry.skipTicks = expBackoff(entry.stableStreak, r.reviewsStableBackoffMaxTicks())
	r.prPollState[key] = entry
}

func (r *Handler) pruneKnownPRState(seen map[string]struct{}) {
	if r.prPollState == nil {
		return
	}
	for key := range r.prPollState {
		if _, ok := seen[key]; !ok {
			delete(r.prPollState, key)
		}
	}
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
