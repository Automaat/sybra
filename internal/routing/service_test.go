package routing

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
)

func testBaseConfig() abtest.Config {
	return abtest.Config{
		Experiments: []abtest.Experiment{
			{
				ID: "exp",
				Variants: []abtest.Variant{
					{ID: "v1", Provider: "claude", Model: "sonnet", Weight: 1},
					{ID: "v2", Provider: "codex", Model: "gpt", Weight: 1},
				},
			},
		},
	}
}

func testReport(v1Landed, v2Landed float64) evaluation.Report {
	return evaluation.Report{
		ByExperimentKind: []evaluation.ExperimentKindBreakdown{
			{
				Kind: "model",
				Groups: []evaluation.ExperimentGroup{
					{
						ExperimentID: "exp",
						Rows: []evaluation.ComparisonBreakdown{
							{
								ExperimentID: "exp", VariantID: "v1",
								Runs: 100, ResolvedRuns: 100,
								LandedEstimate: evaluation.RateEstimate{WilsonLower: v1Landed, HasData: true},
							},
							{
								ExperimentID: "exp", VariantID: "v2",
								Runs: 100, ResolvedRuns: 100,
								LandedEstimate: evaluation.RateEstimate{WilsonLower: v2Landed, HasData: true},
							},
						},
					},
				},
			},
		},
	}
}

// runOnceSync runs svc.Run with an already-cancelled context: Run always
// executes its first tick synchronously before checking ctx.Done(), so this
// deterministically exercises exactly one tick and returns.
func runOnceSync(svc *Service) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.Run(ctx)
}

func newTestService(t *testing.T, enabled bool, rep evaluation.Report, applied *[]abtest.Config, audited *[]audit.Event) (*Service, *Store) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:           enabled,
			IntervalHours:     6,
			WeightBudget:      20,
			FloorWeight:       1,
			MaxStep:           100, // large: converge to target in one tick
			MinSamplesToShift: 0,
			Coefficients:      config.DefaultRoutingCoefficients(),
		},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return rep, true },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			if applied != nil {
				*applied = append(*applied, cfg)
			}
			return nil
		},
		AuditLog: func(e audit.Event) error {
			if audited != nil {
				*audited = append(*audited, e)
			}
			return nil
		},
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	return svc, store
}

func TestService_Tick_Enabled_SavesAndApplies(t *testing.T) {
	var applied []abtest.Config
	var audited []audit.Event
	svc, store := newTestService(t, true, testReport(0.9, 0.1), &applied, &audited)

	runOnceSync(svc)

	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if overlay.Version != 1 {
		t.Fatalf("overlay.Version = %d, want 1", overlay.Version)
	}
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(applied))
	}
	if applied[0].WeightsVersion == nil || *applied[0].WeightsVersion != 1 {
		t.Fatalf("applied WeightsVersion = %+v, want 1", applied[0].WeightsVersion)
		panic("unreachable")
	}
	if len(audited) != 1 || audited[0].Type != audit.EventRoutingReweighted {
		t.Fatalf("audited = %+v, want one routing.reweighted event", audited)
	}
	// v1 landed far better than v2 — it must end up with strictly more weight.
	w1, _ := overlay.WeightAt("exp", "v1")
	w2, _ := overlay.WeightAt("exp", "v2")
	if w1 <= w2 {
		t.Fatalf("v1.weight = %d, want > v2.weight = %d", w1, w2)
	}
}

func TestService_Tick_ShadowMode_ComputesAndAuditsButNeverApplies(t *testing.T) {
	var applied []abtest.Config
	var audited []audit.Event
	svc, store := newTestService(t, false, testReport(0.9, 0.1), &applied, &audited)

	runOnceSync(svc)

	if len(applied) != 0 {
		t.Fatalf("apply calls = %d, want 0 in shadow mode", len(applied))
	}
	if len(audited) != 1 {
		t.Fatalf("audited = %d, want 1 (shadow mode still audits)", len(audited))
	}
	if audited[0].Data["applied"] != false {
		t.Fatalf("audited[0].Data[applied] = %v, want false", audited[0].Data["applied"])
	}
	if _, ok, _ := store.Load(); !ok {
		t.Fatalf("overlay not persisted in shadow mode, want persisted (compute + audit only)")
	}
}

// TestService_ShadowTicks_StayWithinMaxStepOfBase guards the rollout-safety
// contract: while routing is disabled nothing is applied, so live weights stay
// at base. Repeated shadow ticks must therefore keep the persisted overlay
// within one MaxStep of base — otherwise enabling routing and letting
// ApplyPersistedOverlay push the accumulated overlay would jump live traffic
// many steps in a single dispatch, bypassing MaxStep.
func TestService_ShadowTicks_StayWithinMaxStepOfBase(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	const maxStep = 2
	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:           false, // shadow mode: never applies
			IntervalHours:     6,
			WeightBudget:      20, // budget lets v1 target climb well past base+MaxStep
			FloorWeight:       1,
			MaxStep:           maxStep,
			MinSamplesToShift: 0,
			Coefficients:      config.DefaultRoutingCoefficients(),
		},
		Base:   testBaseConfig, // v1/v2 both start at weight 1
		Report: func() (evaluation.Report, bool) { return testReport(0.9, 0.1), true },
		Store:  store,
		Apply:  func(abtest.Config) error { t.Fatalf("Apply called in shadow mode"); return nil },
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})

	svc.loadOverlay()
	for range 5 {
		svc.tick()
	}

	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	w1, _ := overlay.WeightAt("exp", "v1")
	// base v1 weight is 1; after any number of shadow ticks it must not exceed
	// base + MaxStep, since live traffic never left base.
	if w1 > 1+maxStep {
		t.Fatalf("shadow overlay v1 weight = %d after 5 ticks, want <= %d (base+MaxStep)", w1, 1+maxStep)
	}
}

func TestService_Tick_NoReportYet_BootstrapsEnabledOverlay(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	var applied []abtest.Config
	var audited []audit.Event
	svc := NewService(Deps{
		Cfg:    config.RoutingConfig{Enabled: true, IntervalHours: 6},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return evaluation.Report{}, false },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			applied = append(applied, cfg)
			return nil
		},
		AuditLog: func(e audit.Event) error { audited = append(audited, e); return nil },
		Logger:   slog.New(slog.DiscardHandler),
	})
	runOnceSync(svc)
	if len(applied) != 1 || len(audited) != 1 {
		t.Fatalf("applied=%d audited=%d, want 1/1 bootstrap generation", len(applied), len(audited))
	}
	if applied[0].WeightsVersion == nil || *applied[0].WeightsVersion != 1 {
		t.Fatalf("applied WeightsVersion = %+v, want 1", applied[0].WeightsVersion)
		panic("unreachable")
	}
	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if overlay.Version != 1 {
		t.Fatalf("overlay.Version = %d, want 1", overlay.Version)
	}
	if w, has := overlay.WeightAt("exp", "v1"); !has || w != 1 {
		t.Fatalf("exp/v1 weight = (%d, %v), want (1, true)", w, has)
	}
	if w, has := overlay.WeightAt("exp", "v2"); !has || w != 1 {
		t.Fatalf("exp/v2 weight = (%d, %v), want (1, true)", w, has)
	}
}

// TestService_PrimeThenTick_NoReport_NoBootstrapChurn guards the fresh-enabled
// startup sequence: Prime() bootstraps generation 1 (stamping
// InsufficientData=true), then the very next tick (Run's synchronous first
// tick) takes the missing-report rollback path. The rollback baseline is
// weight-identical to the bootstrap overlay, so it must NOT mint a second
// generation or emit a spurious routing.rolled_back audit — otherwise early
// assignments split across generations 1 and 2 with no real weight change.
func TestService_PrimeThenTick_NoReport_NoBootstrapChurn(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	var applied []abtest.Config
	var audited []audit.Event
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc := NewService(Deps{
		Cfg:    config.RoutingConfig{Enabled: true, IntervalHours: 6},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return evaluation.Report{}, false },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			applied = append(applied, cfg)
			return nil
		},
		AuditLog: func(e audit.Event) error { audited = append(audited, e); return nil },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
	})

	// Prime bootstraps generation 1.
	svc.Prime()
	// Run's synchronous first tick takes the no-report rollback path.
	runOnceSync(svc)

	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if overlay.Version != 1 {
		t.Fatalf("overlay.Version = %d, want 1 (no churn after bootstrap)", overlay.Version)
	}
	// Prime applies bootstrap once; the follow-up rollback is a no-op, so no
	// second apply and no rolled_back audit.
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1 (bootstrap only)", len(applied))
	}
	for _, e := range audited {
		if e.Type == audit.EventRoutingRolledBack {
			t.Fatalf("emitted spurious routing.rolled_back audit: %+v", audited)
		}
	}
}

func TestService_Tick_NoReportYet_ShadowModeNoOp(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	var applied []abtest.Config
	var audited []audit.Event
	svc := NewService(Deps{
		Cfg:    config.RoutingConfig{Enabled: false, IntervalHours: 6},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return evaluation.Report{}, false },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			applied = append(applied, cfg)
			return nil
		},
		AuditLog: func(e audit.Event) error { audited = append(audited, e); return nil },
		Logger:   slog.New(slog.DiscardHandler),
	})
	runOnceSync(svc)
	if len(applied) != 0 || len(audited) != 0 {
		t.Fatalf("applied=%d audited=%d, want 0/0 with no report in shadow mode", len(applied), len(audited))
	}
	if _, ok, _ := store.Load(); ok {
		t.Fatalf("overlay persisted with no report in shadow mode, want none")
	}
}

// TestBuildOverlay_CarriesForwardUntouchedExperiments guards the persisted
// snapshot against a partial-plan tick silently dropping the last-learned
// weights of an experiment it did not re-plan this generation.
func TestBuildOverlay_CarriesForwardUntouchedExperiments(t *testing.T) {
	prev := Overlay{
		Version: 1,
		Experiments: []OverlayExperiment{
			{ExperimentID: "exp-a", Variants: []OverlayVariant{{VariantID: "v1", Weight: 7}}},
			{ExperimentID: "exp-b", Variants: []OverlayVariant{{VariantID: "v1", Weight: 9}}},
		},
	}
	// This tick only re-plans exp-a.
	plan := WeightPlan{Changed: true, Experiments: map[string]map[string]int{
		"exp-a": {"v1": 5},
	}}
	// Both experiments are still present in the base config.
	base := abtest.Config{Experiments: []abtest.Experiment{
		{ID: "exp-a", Variants: []abtest.Variant{{ID: "v1", Weight: 1}}},
		{ID: "exp-b", Variants: []abtest.Variant{{ID: "v1", Weight: 1}}},
	}}

	overlay := buildOverlay(2, time.Now(), plan, nil, prev, base)

	if wa, ok := overlay.WeightAt("exp-a", "v1"); !ok || wa != 5 {
		t.Fatalf("exp-a/v1 weight = (%d, %v), want (5, true)", wa, ok)
	}
	// exp-b was untouched this tick — its learned weight must survive, not
	// fall back to base.
	if wb, ok := overlay.WeightAt("exp-b", "v1"); !ok || wb != 9 {
		t.Fatalf("exp-b/v1 weight = (%d, %v), want (9, true) (carry-forward)", wb, ok)
	}
	if len(overlay.Experiments) != 2 {
		t.Fatalf("overlay has %d experiments, want 2", len(overlay.Experiments))
	}
}

// TestBuildOverlay_DropsRemovedExperimentsAndVariants guards against a stale
// overlay entry surviving after the operator removes an experiment (or a
// single variant) from the base config — it must not linger to revive
// obsolete weights if the same IDs are re-introduced later.
func TestBuildOverlay_DropsRemovedExperimentsAndVariants(t *testing.T) {
	prev := Overlay{
		Version: 1,
		Experiments: []OverlayExperiment{
			{ExperimentID: "exp-a", Variants: []OverlayVariant{{VariantID: "v1", Weight: 7}}},
			{ExperimentID: "exp-gone", Variants: []OverlayVariant{{VariantID: "v1", Weight: 9}}},
			{ExperimentID: "exp-c", Variants: []OverlayVariant{
				{VariantID: "v1", Weight: 3},
				{VariantID: "v-gone", Weight: 4},
			}},
		},
	}
	// This tick re-plans nothing; every prior experiment is a carry-forward
	// candidate.
	plan := WeightPlan{Changed: true, Experiments: map[string]map[string]int{}}
	// Base no longer declares exp-gone, and exp-c dropped v-gone.
	base := abtest.Config{Experiments: []abtest.Experiment{
		{ID: "exp-a", Variants: []abtest.Variant{{ID: "v1", Weight: 1}}},
		{ID: "exp-c", Variants: []abtest.Variant{{ID: "v1", Weight: 1}}},
	}}

	overlay := buildOverlay(2, time.Now(), plan, nil, prev, base)

	if _, ok := overlay.WeightAt("exp-gone", "v1"); ok {
		t.Fatalf("exp-gone survived carry-forward, want dropped (removed from base)")
	}
	if _, ok := overlay.WeightAt("exp-c", "v-gone"); ok {
		t.Fatalf("exp-c/v-gone survived carry-forward, want dropped (removed from base)")
	}
	if wa, ok := overlay.WeightAt("exp-a", "v1"); !ok || wa != 7 {
		t.Fatalf("exp-a/v1 weight = (%d, %v), want (7, true) (still in base)", wa, ok)
	}
	if wc, ok := overlay.WeightAt("exp-c", "v1"); !ok || wc != 3 {
		t.Fatalf("exp-c/v1 weight = (%d, %v), want (3, true) (still in base)", wc, ok)
	}
	if len(overlay.Experiments) != 2 {
		t.Fatalf("overlay has %d experiments, want 2 (exp-a, exp-c)", len(overlay.Experiments))
	}
}

// expRows builds a one-experiment breakdown group for the given experiment ID.
func expRows(expID string, v1Landed, v2Landed float64) evaluation.ExperimentGroup {
	return evaluation.ExperimentGroup{
		ExperimentID: expID,
		Rows: []evaluation.ComparisonBreakdown{
			{
				ExperimentID: expID, VariantID: "v1",
				Runs: 100, ResolvedRuns: 100,
				LandedEstimate: evaluation.RateEstimate{WilsonLower: v1Landed, HasData: true},
			},
			{
				ExperimentID: expID, VariantID: "v2",
				Runs: 100, ResolvedRuns: 100,
				LandedEstimate: evaluation.RateEstimate{WilsonLower: v2Landed, HasData: true},
			},
		},
	}
}

// TestService_Tick_NoOp_PrunesRemovedExperiment covers the service-level no-op
// tick path (plan.Changed == false): when the operator removes an experiment
// after its weights were persisted, the next tick — even one that shifts no
// weight — must purge the now-stale overlay entry so it cannot revive obsolete
// weights if the same experiment IDs are re-added later. buildOverlay's own
// pruning never runs on this path.
func TestService_Tick_NoOp_PrunesRemovedExperiment(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}

	includeB := true
	base := func() abtest.Config {
		cfg := abtest.Config{Experiments: []abtest.Experiment{
			{ID: "exp", Variants: []abtest.Variant{
				{ID: "v1", Provider: "claude", Model: "sonnet", Weight: 1},
				{ID: "v2", Provider: "codex", Model: "gpt", Weight: 1},
			}},
		}}
		if includeB {
			cfg.Experiments = append(cfg.Experiments, abtest.Experiment{
				ID: "exp-b", Variants: []abtest.Variant{
					{ID: "v1", Provider: "claude", Model: "sonnet", Weight: 1},
					{ID: "v2", Provider: "codex", Model: "gpt", Weight: 1},
				},
			})
		}
		return cfg
	}
	report := func() (evaluation.Report, bool) {
		groups := []evaluation.ExperimentGroup{expRows("exp", 0.9, 0.1)}
		if includeB {
			groups = append(groups, expRows("exp-b", 0.9, 0.1))
		}
		return evaluation.Report{
			ByExperimentKind: []evaluation.ExperimentKindBreakdown{
				{Kind: "model", Groups: groups},
			},
		}, true
	}

	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:           true,
			IntervalHours:     6,
			WeightBudget:      20,
			FloorWeight:       1,
			MaxStep:           100, // converge in one tick
			MinSamplesToShift: 0,
			Coefficients:      config.DefaultRoutingCoefficients(),
		},
		Base:   base,
		Report: report,
		Store:  store,
		Apply:  func(abtest.Config) error { return nil },
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})

	runOnceSync(svc) // converges both experiments -> version 1
	if o, ok, _ := store.Load(); !ok {
		t.Fatalf("overlay not persisted after first tick")
	} else if _, has := o.WeightAt("exp-b", "v1"); !has {
		t.Fatalf("exp-b not in overlay after first tick, want present")
	}

	// Operator removes exp-b from base and it drops out of the report. The
	// second tick shifts no weight on exp (already converged) -> plan.Changed
	// is false, so this exercises the no-op prune path.
	includeB = false
	runOnceSync(svc)

	o, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if _, has := o.WeightAt("exp-b", "v1"); has {
		t.Fatalf("exp-b survived the no-op tick, want pruned (removed from base)")
	}
	if _, has := o.WeightAt("exp", "v1"); !has {
		t.Fatalf("exp/v1 dropped by prune, want retained (still in base)")
	}
	if o.Version != 2 {
		t.Fatalf("overlay.Version = %d after prune, want 2 (bumped on prune)", o.Version)
	}
}

func TestService_Tick_IgnoresHistoricalRowsForRemovedCohorts(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	if err := store.Save(Overlay{
		Version: 1,
		Experiments: []OverlayExperiment{
			{ExperimentID: "exp", Variants: []OverlayVariant{
				{VariantID: "v1", Weight: 1},
				{VariantID: "v-gone", Weight: 9},
			}},
			{ExperimentID: "exp-gone", Variants: []OverlayVariant{{VariantID: "v1", Weight: 9}}},
		},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	base := func() abtest.Config {
		return abtest.Config{Experiments: []abtest.Experiment{{
			ID: "exp",
			Variants: []abtest.Variant{
				{ID: "v1", Provider: "claude", Model: "sonnet", Weight: 1},
			},
		}}}
	}
	report := func() (evaluation.Report, bool) {
		return evaluation.Report{
			ByExperimentKind: []evaluation.ExperimentKindBreakdown{
				{Kind: "model", Groups: []evaluation.ExperimentGroup{{
					ExperimentID: "exp",
					Rows: []evaluation.ComparisonBreakdown{
						{
							ExperimentID: "exp", VariantID: "v1",
							Runs: 100, ResolvedRuns: 100,
							LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.9, HasData: true},
						},
						{
							ExperimentID: "exp", VariantID: "v-gone",
							Runs: 100, ResolvedRuns: 100,
							LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.95, HasData: true},
						},
					},
				}}},
				{Kind: "unknown", Groups: []evaluation.ExperimentGroup{{
					ExperimentID: "exp-gone",
					Rows: []evaluation.ComparisonBreakdown{
						{
							ExperimentID: "exp-gone", VariantID: "v1",
							Runs: 100, ResolvedRuns: 100,
							LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.99, HasData: true},
						},
					},
				}}},
			},
		}, true
	}

	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:           true,
			IntervalHours:     6,
			WeightBudget:      20,
			FloorWeight:       1,
			MaxStep:           100,
			MinSamplesToShift: 0,
			Coefficients:      config.DefaultRoutingCoefficients(),
		},
		Base:   base,
		Report: report,
		Store:  store,
		Apply:  func(abtest.Config) error { return nil },
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})

	runOnceSync(svc)

	o, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if _, has := o.WeightAt("exp-gone", "v1"); has {
		t.Fatalf("exp-gone/v1 was rebuilt from unknown historical rows, want absent")
	}
	if _, has := o.WeightAt("exp", "v-gone"); has {
		t.Fatalf("exp/v-gone was rebuilt from historical rows, want absent")
	}
	if w, has := o.WeightAt("exp", "v1"); !has || w != 20 {
		t.Fatalf("exp/v1 weight = (%d, %v), want (20, true)", w, has)
	}
}

func TestService_Tick_ZeroWeightVariantStaysDisabled(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	if err := store.Save(Overlay{
		Version: 1,
		Experiments: []OverlayExperiment{{
			ExperimentID: "exp",
			Variants: []OverlayVariant{
				{VariantID: "v1", Weight: 1},
				{VariantID: "v2", Weight: 9},
			},
		}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	base := func() abtest.Config {
		return abtest.Config{Experiments: []abtest.Experiment{{
			ID: "exp",
			Variants: []abtest.Variant{
				{ID: "v1", Provider: "claude", Model: "sonnet", Weight: 1},
				{ID: "v2", Provider: "codex", Model: "gpt", Weight: 0},
			},
		}}}
	}
	report := func() (evaluation.Report, bool) {
		return evaluation.Report{
			ByExperimentKind: []evaluation.ExperimentKindBreakdown{{
				Kind: "model",
				Groups: []evaluation.ExperimentGroup{{
					ExperimentID: "exp",
					Rows: []evaluation.ComparisonBreakdown{
						{
							ExperimentID: "exp", VariantID: "v1",
							Runs: 100, ResolvedRuns: 100,
							LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.1, HasData: true},
						},
						{
							ExperimentID: "exp", VariantID: "v2",
							Runs: 100, ResolvedRuns: 100,
							LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.99, HasData: true},
						},
					},
				}},
			}},
		}, true
	}

	var applied []abtest.Config
	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:           true,
			IntervalHours:     6,
			WeightBudget:      20,
			FloorWeight:       1,
			MaxStep:           100,
			MinSamplesToShift: 0,
			Coefficients:      config.DefaultRoutingCoefficients(),
		},
		Base:   base,
		Report: report,
		Store:  store,
		Apply:  func(cfg abtest.Config) error { applied = append(applied, cfg); return nil },
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})

	runOnceSync(svc)

	o, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if _, has := o.WeightAt("exp", "v2"); has {
		t.Fatalf("zero-weight v2 survived overlay, want pruned")
	}
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(applied))
	}
	if w := weightOf(applied[0], "exp", "v2"); w != 0 {
		t.Fatalf("applied v2 weight = %d, want 0 (operator-disabled)", w)
	}
}

func TestService_VersionBumpsOnlyOnChange(t *testing.T) {
	var applied []abtest.Config
	var audited []audit.Event
	svc, store := newTestService(t, true, testReport(0.9, 0.1), &applied, &audited)

	runOnceSync(svc) // first tick: converges to target, version 1
	runOnceSync(svc) // second tick: same report, already at target -> no-op

	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if overlay.Version != 1 {
		t.Fatalf("overlay.Version = %d after unchanged second tick, want 1", overlay.Version)
	}
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1 (second tick is a no-op)", len(applied))
	}
	if len(audited) != 1 {
		t.Fatalf("audit calls = %d, want 1 (second tick is a no-op)", len(audited))
	}
}

func TestService_ApplyPersistedOverlay_PushesPriorGeneration(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	seeded := Overlay{
		Version: 5,
		Experiments: []OverlayExperiment{
			{ExperimentID: "exp", Variants: []OverlayVariant{{VariantID: "v1", Weight: 9}, {VariantID: "v2", Weight: 1}}},
		},
	}
	if err := store.Save(seeded); err != nil {
		t.Fatalf("Save: %v", err)
		panic("unreachable")
	}

	var applied []abtest.Config
	svc := NewService(Deps{
		Cfg:    config.RoutingConfig{Enabled: true},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return evaluation.Report{}, false },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			applied = append(applied, cfg)
			return nil
		},
		Logger: slog.New(slog.DiscardHandler),
	})

	svc.ApplyPersistedOverlay()

	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(applied))
	}
	if applied[0].WeightsVersion == nil || *applied[0].WeightsVersion != 5 {
		t.Fatalf("applied WeightsVersion = %+v, want 5", applied[0].WeightsVersion)
		panic("unreachable")
	}
	var gotV1, gotV2 int
	for _, exp := range applied[0].Experiments {
		for _, v := range exp.Variants {
			if v.ID == "v1" {
				gotV1 = v.Weight
			}
			if v.ID == "v2" {
				gotV2 = v.Weight
			}
		}
	}
	if gotV1 != 9 || gotV2 != 1 {
		t.Fatalf("applied weights v1=%d v2=%d, want 9/1", gotV1, gotV2)
	}
}

func TestService_ApplyPersistedOverlay_ZeroWeightVariantStaysDisabled(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	if err := store.Save(Overlay{
		Version: 5,
		Experiments: []OverlayExperiment{{
			ExperimentID: "exp",
			Variants: []OverlayVariant{
				{VariantID: "v1", Weight: 7},
				{VariantID: "v2", Weight: 9},
			},
		}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var applied []abtest.Config
	svc := NewService(Deps{
		Cfg: config.RoutingConfig{Enabled: true},
		Base: func() abtest.Config {
			return abtest.Config{Experiments: []abtest.Experiment{{
				ID: "exp",
				Variants: []abtest.Variant{
					{ID: "v1", Provider: "claude", Model: "sonnet", Weight: 1},
					{ID: "v2", Provider: "codex", Model: "gpt", Weight: 0},
				},
			}}}
		},
		Store:  store,
		Apply:  func(cfg abtest.Config) error { applied = append(applied, cfg); return nil },
		Logger: slog.New(slog.DiscardHandler),
	})

	svc.ApplyPersistedOverlay()

	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(applied))
	}
	if w := weightOf(applied[0], "exp", "v1"); w != 7 {
		t.Fatalf("applied v1 weight = %d, want 7", w)
	}
	if w := weightOf(applied[0], "exp", "v2"); w != 0 {
		t.Fatalf("applied v2 weight = %d, want 0 (operator-disabled)", w)
	}
}

func TestService_ApplyPersistedOverlay_DisabledIsNoOp(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	if err := store.Save(Overlay{Version: 1, Experiments: []OverlayExperiment{{ExperimentID: "exp", Variants: []OverlayVariant{{VariantID: "v1", Weight: 9}}}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var applied []abtest.Config
	svc := NewService(Deps{
		Cfg:    config.RoutingConfig{Enabled: false},
		Base:   testBaseConfig,
		Store:  store,
		Apply:  func(cfg abtest.Config) error { applied = append(applied, cfg); return nil },
		Logger: slog.New(slog.DiscardHandler),
	})
	svc.ApplyPersistedOverlay()
	if len(applied) != 0 {
		t.Fatalf("apply calls = %d, want 0 when routing disabled", len(applied))
	}
}

// TestService_Tick_StaleEvaluation_RollsBackToBaseline covers the "keep one
// stable production baseline" fail-safe: a report older than
// EvaluationMaxAgeHours must not be allowed to keep the overlay on its
// previously-promoted (non-base) weights — it must roll back to base.
func TestService_Tick_StaleEvaluation_RollsBackToBaseline(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	// Seed a prior generation that already shifted weight away from base
	// (v1=1, v2=1) toward v1.
	if err := store.Save(Overlay{
		Version: 3,
		Experiments: []OverlayExperiment{{
			ExperimentID: "exp",
			Variants: []OverlayVariant{
				{VariantID: "v1", Weight: 15},
				{VariantID: "v2", Weight: 1},
			},
		}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var applied []abtest.Config
	var audited []audit.Event
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	staleRep := testReport(0.9, 0.1)
	staleRep.SchemaVersion = evaluation.ScorecardSchemaVersion
	staleRep.GeneratedAt = now.Add(-30 * 24 * time.Hour) // far older than maxAge
	maxAgeHours := 24.0

	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:               true,
			IntervalHours:         6,
			WeightBudget:          20,
			FloorWeight:           1,
			MaxStep:               100,
			MinSamplesToShift:     0,
			EvaluationMaxAgeHours: &maxAgeHours,
			Coefficients:          config.DefaultRoutingCoefficients(),
		},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return staleRep, true },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			applied = append(applied, cfg)
			return nil
		},
		AuditLog: func(e audit.Event) error { audited = append(audited, e); return nil },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
	})

	runOnceSync(svc)

	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if overlay.Version != 4 {
		t.Fatalf("overlay.Version = %d, want 4 (bumped on rollback)", overlay.Version)
	}
	w1, _ := overlay.WeightAt("exp", "v1")
	w2, _ := overlay.WeightAt("exp", "v2")
	if w1 != 1 || w2 != 1 {
		t.Fatalf("overlay weights v1=%d v2=%d, want 1/1 (base declared weights)", w1, w2)
	}
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1 (rollback applied live)", len(applied))
	}
	if w := weightOf(applied[0], "exp", "v1"); w != 1 {
		t.Fatalf("applied v1 weight = %d, want 1", w)
	}
	if len(audited) != 1 || audited[0].Type != audit.EventRoutingRolledBack {
		t.Fatalf("audited = %+v, want one routing.rolled_back event", audited)
	}
}

// TestService_Tick_NoReport_LearnedOverlay_RollsBackToBaseline guards the
// missing-report path: when a learned overlay was loaded from disk (and
// re-pushed by ApplyPersistedOverlay) but Report() returns ok=false —
// evaluation disabled, or a restart before the first report — the tick must
// fall back to base's declared weights instead of continuing to serve stale
// learned weights.
func TestService_Tick_NoReport_LearnedOverlay_RollsBackToBaseline(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	// Seed a prior generation that shifted weight away from base (v1=1, v2=1).
	if err := store.Save(Overlay{
		Version: 3,
		Experiments: []OverlayExperiment{{
			ExperimentID: "exp",
			Variants: []OverlayVariant{
				{VariantID: "v1", Weight: 15},
				{VariantID: "v2", Weight: 1},
			},
		}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var applied []abtest.Config
	var audited []audit.Event
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc := NewService(Deps{
		Cfg:    config.RoutingConfig{Enabled: true, IntervalHours: 6},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return evaluation.Report{}, false },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			applied = append(applied, cfg)
			return nil
		},
		AuditLog: func(e audit.Event) error { audited = append(audited, e); return nil },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
	})

	runOnceSync(svc)

	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if overlay.Version != 4 {
		t.Fatalf("overlay.Version = %d, want 4 (bumped on rollback)", overlay.Version)
	}
	w1, _ := overlay.WeightAt("exp", "v1")
	w2, _ := overlay.WeightAt("exp", "v2")
	if w1 != 1 || w2 != 1 {
		t.Fatalf("overlay weights v1=%d v2=%d, want 1/1 (base declared weights)", w1, w2)
	}
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1 (rollback applied live)", len(applied))
	}
	if len(audited) != 1 || audited[0].Type != audit.EventRoutingRolledBack {
		t.Fatalf("audited = %+v, want one routing.rolled_back event", audited)
	}

	// Sustained no-report: already at baseline — must not churn a new version.
	runOnceSync(svc)
	if len(applied) != 1 || len(audited) != 1 {
		t.Fatalf("after second tick: applied=%d audited=%d, want 1/1 (no churn at baseline)", len(applied), len(audited))
	}
}

// TestService_Tick_StaleEvaluation_AlreadyAtBaseline_NoOp covers the churn
// guard: once the overlay already matches base's declared weights, a
// sustained stale-evaluation outage must not save a new overlay generation
// or re-apply/re-audit every subsequent tick.
func TestService_Tick_StaleEvaluation_AlreadyAtBaseline_NoOp(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	staleRep := testReport(0.9, 0.1)
	staleRep.SchemaVersion = evaluation.ScorecardSchemaVersion
	staleRep.GeneratedAt = now.Add(-30 * 24 * time.Hour)

	var applied []abtest.Config
	var audited []audit.Event
	maxAgeHours := 24.0
	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:               true,
			IntervalHours:         6,
			EvaluationMaxAgeHours: &maxAgeHours,
		},
		Base:   testBaseConfig,
		Report: func() (evaluation.Report, bool) { return staleRep, true },
		Store:  store,
		Apply: func(cfg abtest.Config) error {
			applied = append(applied, cfg)
			return nil
		},
		AuditLog: func(e audit.Event) error { audited = append(audited, e); return nil },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
	})

	// First tick: no overlay existed yet, so rolling back to (equal to)
	// base is itself a real, one-time persisted action.
	runOnceSync(svc)
	if len(applied) != 1 || len(audited) != 1 {
		t.Fatalf("after first tick: applied=%d audited=%d, want 1/1 (initial rollback to base)", len(applied), len(audited))
	}

	// Subsequent ticks: already at baseline — must not churn.
	runOnceSync(svc)
	runOnceSync(svc)

	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1 (no churn once already at baseline)", len(applied))
	}
	if len(audited) != 1 {
		t.Fatalf("audit calls = %d, want 1 (no churn once already at baseline)", len(audited))
	}
}

// TestService_Tick_MismatchedSchemaVersion_RollsBackToBaseline covers the
// "no aggregate cohort mixes incompatible metric schema versions" acceptance
// criterion: a report stamped with a schema version this build does not
// understand must not drive a weight decision, even when fresh.
func TestService_Tick_MismatchedSchemaVersion_RollsBackToBaseline(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	if err := store.Save(Overlay{
		Version: 1,
		Experiments: []OverlayExperiment{{
			ExperimentID: "exp",
			Variants: []OverlayVariant{
				{VariantID: "v1", Weight: 15},
				{VariantID: "v2", Weight: 1},
			},
		}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mismatchedRep := testReport(0.9, 0.1)
	mismatchedRep.SchemaVersion = evaluation.ScorecardSchemaVersion - 1
	mismatchedRep.GeneratedAt = now // fresh, but wrong schema

	var audited []audit.Event
	svc := NewService(Deps{
		Cfg: config.RoutingConfig{
			Enabled:       true,
			IntervalHours: 6,
			WeightBudget:  20,
			FloorWeight:   1,
			MaxStep:       100,
		},
		Base:     testBaseConfig,
		Report:   func() (evaluation.Report, bool) { return mismatchedRep, true },
		Store:    store,
		Apply:    func(abtest.Config) error { return nil },
		AuditLog: func(e audit.Event) error { audited = append(audited, e); return nil },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
	})

	runOnceSync(svc)

	overlay, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("store.Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	w1, _ := overlay.WeightAt("exp", "v1")
	w2, _ := overlay.WeightAt("exp", "v2")
	if w1 != 1 || w2 != 1 {
		t.Fatalf("overlay weights v1=%d v2=%d, want 1/1 (rolled back to base on schema mismatch)", w1, w2)
	}
	if len(audited) != 1 || audited[0].Type != audit.EventRoutingRolledBack {
		t.Fatalf("audited = %+v, want one routing.rolled_back event", audited)
	}
}

func weightOf(cfg abtest.Config, expID, variantID string) int {
	for i := range cfg.Experiments {
		if cfg.Experiments[i].ID != expID {
			continue
		}
		for j := range cfg.Experiments[i].Variants {
			if cfg.Experiments[i].Variants[j].ID == variantID {
				return cfg.Experiments[i].Variants[j].Weight
			}
		}
	}
	return 0
}
