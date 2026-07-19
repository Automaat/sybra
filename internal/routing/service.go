package routing

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
)

// OverlayEvent is the Emit payload name for a changed routing overlay.
const OverlayEvent = "routing:overlay"

// WeightApplier pushes a merged (base + overlay) abtest.Config to every live
// A/B selection site (workflow engine, orchestrator, evaluation service, the
// shared app config struct read directly by human-review/staff-review
// dispatch) — see internal/sybra's applyRoutingWeights. Never written to
// config.yaml.
type WeightApplier func(abtest.Config) error

// Deps are the collaborators for the routing service.
type Deps struct {
	Cfg config.RoutingConfig
	// Base returns the current operator-configured A/B suite (experiment/
	// variant identities and declared default weights). Called fresh every
	// tick so a live config edit (new/removed variant) is picked up.
	Base func() abtest.Config
	// Report returns the most recently computed evaluation report and
	// whether one exists yet. Reads the evaluation service's own cache
	// (LastReport) rather than recomputing, so routing never doubles the
	// stats/audit scan cost of a tick.
	Report func() (evaluation.Report, bool)
	Store  *Store
	// Apply pushes a changed, merged config live. Never called when
	// Cfg.Enabled is false (shadow mode: compute + audit only).
	Apply    WeightApplier
	AuditLog func(audit.Event) error
	Emit     func(string, any)
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service periodically scores A/B variants from the evaluation report and
// turns the scores into a bounded weight overlay. Built and run even when
// disabled: a disabled tick still computes, persists, and audits a plan —
// it just never calls Apply — so rollout is observable before it is live.
type Service struct {
	cfg      config.RoutingConfig
	base     func() abtest.Config
	report   func() (evaluation.Report, bool)
	store    *Store
	apply    WeightApplier
	auditLog func(audit.Event) error
	emit     func(string, any)
	logger   *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	overlay Overlay
	loaded  bool
}

// NewService builds the service, filling zero-value dependencies with safe
// no-op defaults so a partially-wired Deps (e.g. in tests) never panics.
func NewService(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Emit == nil {
		d.Emit = func(string, any) {}
	}
	if d.AuditLog == nil {
		d.AuditLog = func(audit.Event) error { return nil }
	}
	if d.Base == nil {
		d.Base = func() abtest.Config { return abtest.Config{} }
	}
	return &Service{
		cfg:      d.Cfg,
		base:     d.Base,
		report:   d.Report,
		store:    d.Store,
		apply:    d.Apply,
		auditLog: d.AuditLog,
		emit:     d.Emit,
		logger:   d.Logger,
		now:      d.Now,
	}
}

// Run ticks on the configured interval, computing and persisting a weight
// plan each time. Runs regardless of Cfg.Enabled (see Service doc); returns
// when ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	s.loadOverlay()
	interval := time.Duration(s.cfg.IntervalHours * float64(time.Hour))
	if interval < time.Hour {
		interval = 6 * time.Hour
	}
	s.logger.Info("routing.start", "enabled", s.cfg.Enabled, "interval", interval.String())

	s.tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// ApplyPersistedOverlay pushes the last-saved overlay (if any) through Apply
// once, synchronously — meant to be called at startup before the ticker's
// first interval elapses, so a restart with routing already enabled does not
// briefly serve unweighted base config while waiting for the next tick.
// No-op when routing is disabled, Apply is unset, or no overlay was ever
// saved.
func (s *Service) ApplyPersistedOverlay() {
	s.loadOverlay()
	if !s.cfg.Enabled || s.apply == nil {
		return
	}
	s.mu.Lock()
	overlay := s.overlay
	loaded := s.loaded
	s.mu.Unlock()
	if !loaded || len(overlay.Experiments) == 0 {
		return
	}
	merged := mergeWeights(s.base(), overlay)
	if err := s.apply(merged); err != nil {
		s.logger.Warn("routing.startup_apply.failed", "err", err)
	}
}

// LastOverlay returns the most recently computed/loaded overlay and whether
// one exists.
func (s *Service) LastOverlay() (Overlay, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overlay, s.loaded && len(s.overlay.Experiments) > 0
}

func (s *Service) loadOverlay() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	s.loaded = true
	if s.store == nil {
		return
	}
	o, ok, err := s.store.Load()
	if err != nil {
		s.logger.Warn("routing.overlay.load_failed", "err", err)
		return
	}
	if ok {
		s.overlay = o
	}
}

func (s *Service) tick(_ context.Context) {
	if s.report == nil || s.store == nil {
		return
	}
	rep, ok := s.report()
	if !ok {
		s.logger.Debug("routing.tick.no_report")
		return
	}

	base := s.base()
	scores := ScoreVariants(flattenRows(rep), coefficientsFromConfig(s.cfg.Coefficients))

	s.mu.Lock()
	prevOverlay := s.overlay
	s.mu.Unlock()

	current := currentWeights(base, prevOverlay, s.cfg.Enabled)
	plan := PlanWeights(scores, current, PlanOptions{
		WeightBudget:      s.cfg.WeightBudget,
		FloorWeight:       s.cfg.FloorWeight,
		MaxStep:           s.cfg.MaxStep,
		MinSamplesToShift: s.cfg.MinSamplesToShift,
	})
	if !plan.Changed {
		// No weight change this tick, but the operator may have removed an
		// experiment/variant since the overlay was last persisted. Purge any
		// now-stale entries so they cannot linger on disk and revive obsolete
		// weights if the same IDs are re-added later — buildOverlay's own
		// pruning never runs on this path.
		if pruned, changed := pruneOverlay(prevOverlay, base, s.now()); changed {
			s.persistAndApply(pruned, base)
			return
		}
		s.logger.Debug("routing.tick.unchanged")
		return
	}

	version := prevOverlay.Version + 1
	overlay := buildOverlay(version, s.now(), plan, scores, prevOverlay, base)
	s.persistAndApply(overlay, base)
}

// persistAndApply saves the overlay generation, pushes it live when routing is
// enabled, and emits the overlay event + audit record. A save failure aborts
// before any apply/emit so the in-memory overlay never diverges from disk.
func (s *Service) persistAndApply(overlay Overlay, base abtest.Config) {
	if err := s.store.Save(overlay); err != nil {
		s.logger.Warn("routing.overlay.save_failed", "err", err)
		return
	}
	s.mu.Lock()
	s.overlay = overlay
	s.mu.Unlock()

	applied := false
	if s.cfg.Enabled && s.apply != nil {
		merged := mergeWeights(base, overlay)
		if err := s.apply(merged); err != nil {
			s.logger.Warn("routing.apply.failed", "err", err, "version", overlay.Version)
		} else {
			applied = true
		}
	}

	s.emit(OverlayEvent, overlay)
	s.emitAudit(overlay, applied)
	s.logger.Info("routing.tick", "version", overlay.Version, "experiments", len(overlay.Experiments), "applied", applied)
}

func (s *Service) emitAudit(overlay Overlay, applied bool) {
	data := map[string]any{
		"version": overlay.Version,
		"applied": applied,
	}
	experiments := make([]map[string]any, 0, len(overlay.Experiments))
	for _, exp := range overlay.Experiments {
		variants := make([]map[string]any, 0, len(exp.Variants))
		for _, v := range exp.Variants {
			variants = append(variants, map[string]any{
				"variant_id":        v.VariantID,
				"weight":            v.Weight,
				"score":             v.Score,
				"runs":              v.Runs,
				"resolved_runs":     v.ResolvedRuns,
				"insufficient_data": v.InsufficientData,
				"landed_lower":      v.Inputs.LandedWilsonLower,
				"cost_per_landed":   v.Inputs.CostPerLanded,
				"duration_p50s":     v.Inputs.DurationP50S,
			})
		}
		experiments = append(experiments, map[string]any{
			"experiment_id": exp.ExperimentID,
			"variants":      variants,
		})
	}
	data["experiments"] = experiments
	if err := s.auditLog(audit.Event{Type: audit.EventRoutingReweighted, Data: data}); err != nil {
		s.logger.Warn("routing.audit.failed", "err", err)
	}
}

// flattenRows collects every experiment/variant comparison row across every
// experiment kind — routing weights all A/B kinds (model/prompt/skill/
// compound) uniformly, matching how abtest.Config.Experiments itself makes
// no kind distinction at selection time.
func flattenRows(rep evaluation.Report) []evaluation.ComparisonBreakdown {
	var rows []evaluation.ComparisonBreakdown
	for _, kind := range rep.ByExperimentKind {
		for _, group := range kind.Groups {
			rows = append(rows, group.Rows...)
		}
	}
	return rows
}

// currentWeights builds the full configured (experimentID -> variantID ->
// weight) universe from base, then overrides each entry with the
// last-applied overlay weight when one was recorded — so PlanWeights' step
// clamp is relative to what is actually live, not base's static defaults,
// while every configured variant (even one with zero runs) still gets an
// entry.
//
// overlayLive gates the overlay override to what is genuinely serving live
// traffic. In shadow mode (routing disabled) Apply is never called, so live
// weights stay at base — folding the overlay in there would let each shadow
// tick's MaxStep clamp advance from the *previous overlay* rather than from
// base, accumulating drift across ticks. ApplyPersistedOverlay would then push
// that many-steps-away overlay live in a single jump on the first enabled
// dispatch, bypassing the MaxStep rollout cap. Clamping against base while
// shadowing keeps the persisted overlay within one MaxStep of base, so the
// first live apply respects the cap.
func currentWeights(base abtest.Config, overlay Overlay, overlayLive bool) map[string]map[string]int {
	out := map[string]map[string]int{}
	for i := range base.Experiments {
		exp := &base.Experiments[i]
		if exp.ID == "" {
			continue
		}
		variants := map[string]int{}
		for j := range exp.Variants {
			v := &exp.Variants[j]
			if v.ID == "" {
				continue
			}
			weight := v.Weight
			if weight <= 0 {
				weight = defaultFloorWeight
			}
			if overlayLive {
				if w, ok := overlay.WeightAt(exp.ID, v.ID); ok {
					weight = w
				}
			}
			variants[v.ID] = weight
		}
		if len(variants) > 0 {
			out[exp.ID] = variants
		}
	}
	return out
}

// buildOverlay assembles the next persisted overlay generation from a weight
// plan and the scores that drove it. Experiments present in plan.Experiments
// are rebuilt from the plan; every experiment the plan did NOT touch is
// carried forward from prev — but only when it (and each carried variant)
// still exists in the current base config, so an experiment/variant the
// operator has since removed drops out of the overlay instead of silently
// reviving stale weights if the same IDs are re-introduced later. This keeps
// re-plans from dropping the last-learned weights of untouched experiments
// while never resurrecting a deleted cohort's obsolete overlay. Output is
// sorted by experiment ID for deterministic persistence.
func buildOverlay(version int, now time.Time, plan WeightPlan, scores []Score, prev Overlay, base abtest.Config) Overlay {
	scoreByKey := map[string]Score{}
	for _, s := range scores {
		scoreByKey[s.ExperimentID+"|"+s.VariantID] = s
	}

	expIDs := make([]string, 0, len(plan.Experiments))
	for expID := range plan.Experiments {
		expIDs = append(expIDs, expID)
	}
	sort.Strings(expIDs)

	overlay := Overlay{Version: version, GeneratedAt: now}
	planned := make(map[string]bool, len(plan.Experiments))
	for _, expID := range expIDs {
		planned[expID] = true
		weights := plan.Experiments[expID]
		variantIDs := make([]string, 0, len(weights))
		for vid := range weights {
			variantIDs = append(variantIDs, vid)
		}
		sort.Strings(variantIDs)

		ov := OverlayExperiment{ExperimentID: expID}
		for _, vid := range variantIDs {
			s := scoreByKey[expID+"|"+vid]
			ov.Variants = append(ov.Variants, OverlayVariant{
				VariantID:          vid,
				Weight:             weights[vid],
				Score:              s.Value,
				Inputs:             s.Inputs,
				Runs:               s.Runs,
				ResolvedRuns:       s.ResolvedRuns,
				InsufficientData:   s.InsufficientData,
				SkillParityUnknown: s.SkillParityUnknown,
			})
		}
		overlay.Experiments = append(overlay.Experiments, ov)
	}

	// Carry forward untouched experiments from the prior generation, but
	// only those still present in the current base config — a removed
	// experiment/variant must not linger in the overlay and revive stale
	// weights if the same IDs are re-added later.
	baseVariants := baseVariantSet(base)
	for _, exp := range prev.Experiments {
		if planned[exp.ExperimentID] {
			continue
		}
		vs, ok := baseVariants[exp.ExperimentID]
		if !ok {
			continue
		}
		kept := OverlayExperiment{ExperimentID: exp.ExperimentID}
		for _, v := range exp.Variants {
			if vs[v.VariantID] {
				kept.Variants = append(kept.Variants, v)
			}
		}
		if len(kept.Variants) > 0 {
			overlay.Experiments = append(overlay.Experiments, kept)
		}
	}
	sort.Slice(overlay.Experiments, func(i, j int) bool {
		return overlay.Experiments[i].ExperimentID < overlay.Experiments[j].ExperimentID
	})
	return overlay
}

// baseVariantSet indexes base as experimentID -> set of live variant IDs,
// skipping empty IDs — the membership oracle used to decide which persisted
// overlay entries are still declared by the operator.
func baseVariantSet(base abtest.Config) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for i := range base.Experiments {
		exp := &base.Experiments[i]
		if exp.ID == "" {
			continue
		}
		vs := map[string]bool{}
		for j := range exp.Variants {
			if id := exp.Variants[j].ID; id != "" {
				vs[id] = true
			}
		}
		out[exp.ID] = vs
	}
	return out
}

// pruneOverlay strips experiments/variants no longer present in base from a
// persisted overlay, returning the pruned overlay and whether anything was
// removed. buildOverlay only prunes when a plan is produced; the no-op tick
// path (plan.Changed == false) never reaches it, so this covers the case
// where the operator removes a cohort without any concurrent weight change —
// its stale weights must not survive on disk to be revived if the same IDs
// are re-added later. Version bumps only when something is actually pruned.
func pruneOverlay(prev Overlay, base abtest.Config, now time.Time) (Overlay, bool) {
	baseVariants := baseVariantSet(base)
	pruned := Overlay{Version: prev.Version, GeneratedAt: prev.GeneratedAt}
	changed := false
	for _, exp := range prev.Experiments {
		vs, ok := baseVariants[exp.ExperimentID]
		if !ok {
			changed = true
			continue
		}
		kept := OverlayExperiment{ExperimentID: exp.ExperimentID}
		for _, v := range exp.Variants {
			if vs[v.VariantID] {
				kept.Variants = append(kept.Variants, v)
			} else {
				changed = true
			}
		}
		if len(kept.Variants) > 0 {
			pruned.Experiments = append(pruned.Experiments, kept)
		}
	}
	if changed {
		pruned.Version = prev.Version + 1
		pruned.GeneratedAt = now
	}
	return pruned, changed
}

// mergeWeights clones base and overwrites each configured variant's Weight
// with the overlay's applied weight, stamping WeightsVersion so every
// downstream Assignment records this generation. Experiments/variants not
// covered by the overlay (e.g. never scored yet) keep base's declared
// weight unchanged.
func mergeWeights(base abtest.Config, overlay Overlay) abtest.Config {
	merged := base
	merged.Experiments = make([]abtest.Experiment, len(base.Experiments))
	for i := range base.Experiments {
		exp := base.Experiments[i]
		exp.Variants = append([]abtest.Variant(nil), exp.Variants...)
		for j := range exp.Variants {
			if w, ok := overlay.WeightAt(exp.ID, exp.Variants[j].ID); ok {
				exp.Variants[j].Weight = w
			}
		}
		merged.Experiments[i] = exp
	}
	version := overlay.Version
	merged.WeightsVersion = &version
	return merged
}

func coefficientsFromConfig(c config.RoutingCoefficients) Coefficients {
	return Coefficients{
		LandedWeight:       c.LandedWeight,
		MergeWeight:        c.MergeWeight,
		CIFirstPassWeight:  c.CIFirstPassWeight,
		ReworkWeight:       c.ReworkWeight,
		FailureWeight:      c.FailureWeight,
		CostWeight:         c.CostWeight,
		DurationWeight:     c.DurationWeight,
		CostNormalizer:     c.CostNormalizer,
		DurationNormalizer: c.DurationNormalizer,
	}
}
