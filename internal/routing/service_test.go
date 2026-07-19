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
	}
	if overlay.Version != 1 {
		t.Fatalf("overlay.Version = %d, want 1", overlay.Version)
	}
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(applied))
	}
	if applied[0].WeightsVersion == nil || *applied[0].WeightsVersion != 1 {
		t.Fatalf("applied WeightsVersion = %+v, want 1", applied[0].WeightsVersion)
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

func TestService_Tick_NoReportYet_NoOp(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
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
	if len(applied) != 0 || len(audited) != 0 {
		t.Fatalf("applied=%d audited=%d, want 0/0 with no report available", len(applied), len(audited))
	}
	if _, ok, _ := store.Load(); ok {
		t.Fatalf("overlay persisted with no report available, want none")
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
	}
	seeded := Overlay{
		Version: 5,
		Experiments: []OverlayExperiment{
			{ExperimentID: "exp", Variants: []OverlayVariant{{VariantID: "v1", Weight: 9}, {VariantID: "v2", Weight: 1}}},
		},
	}
	if err := store.Save(seeded); err != nil {
		t.Fatalf("Save: %v", err)
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

func TestService_ApplyPersistedOverlay_DisabledIsNoOp(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
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
