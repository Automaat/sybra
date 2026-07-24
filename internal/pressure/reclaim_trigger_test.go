package pressure

import (
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestAdmitTriggersReclaimAtWarningWatermark(t *testing.T) {
	cfg := config.PressureConfig{
		Enabled:                true,
		MinDiskFreePercent:     5,
		WarningDiskFreePercent: 15,
		SampleIntervalSeconds:  15,
	}
	g := testGate(t, cfg, Sample{DiskFreePct: 10, MemAvailablePct: 50, LoadPerCPU: 1})

	var triggered int
	g.SetReclaimTrigger(func() { triggered++ })

	ok, reason := g.Admit()
	if !ok {
		t.Fatalf("Admit() = (false, %q), want admit (10%% free is above the 5%% critical threshold)", reason)
	}
	if triggered != 1 {
		t.Fatalf("reclaim trigger fired %d times, want 1", triggered)
	}
}

func TestAdmitDoesNotTriggerReclaimAboveWarningWatermark(t *testing.T) {
	cfg := config.PressureConfig{
		Enabled:                true,
		MinDiskFreePercent:     5,
		WarningDiskFreePercent: 15,
		SampleIntervalSeconds:  15,
	}
	g := testGate(t, cfg, Sample{DiskFreePct: 50, MemAvailablePct: 50, LoadPerCPU: 1})

	var triggered int
	g.SetReclaimTrigger(func() { triggered++ })

	if ok, _ := g.Admit(); !ok {
		t.Fatal("Admit() = false, want true for a healthy sample")
	}
	if triggered != 0 {
		t.Fatalf("reclaim trigger fired %d times, want 0 (disk free is above the warning watermark)", triggered)
	}
}

func TestAdmitDoesNotTriggerReclaimWhenWarningWatermarkUnset(t *testing.T) {
	cfg := config.PressureConfig{
		Enabled:                true,
		MinDiskFreePercent:     5,
		WarningDiskFreePercent: 0, // disabled
		SampleIntervalSeconds:  15,
	}
	g := testGate(t, cfg, Sample{DiskFreePct: 1, MemAvailablePct: 50, LoadPerCPU: 1})

	var triggered int
	g.SetReclaimTrigger(func() { triggered++ })

	ok, _ := g.Admit()
	if ok {
		t.Fatal("Admit() = true, want false (1% free is below the 5% critical threshold)")
	}
	if triggered != 0 {
		t.Fatalf("reclaim trigger fired %d times, want 0 (WarningDiskFreePercent=0 disables the trigger)", triggered)
	}
}

func TestAdmitStillTriggersReclaimAtCriticalWatermark(t *testing.T) {
	// Fix requirement: "run cleanup automatically at warning AND critical
	// watermarks" — a host that is already below MinDiskFreePercent still
	// benefits from every reclaim attempt, not just the first crossing.
	cfg := config.PressureConfig{
		Enabled:                true,
		MinDiskFreePercent:     5,
		WarningDiskFreePercent: 15,
		SampleIntervalSeconds:  15,
	}
	g := testGate(t, cfg, Sample{DiskFreePct: 1, MemAvailablePct: 50, LoadPerCPU: 1})

	var triggered int
	g.SetReclaimTrigger(func() { triggered++ })

	ok, _ := g.Admit()
	if ok {
		t.Fatal("Admit() = true, want false (1% free is below the 5% critical threshold)")
	}
	if triggered != 1 {
		t.Fatalf("reclaim trigger fired %d times, want 1", triggered)
	}
}

func TestSetReclaimTriggerNilGateIsSafe(t *testing.T) {
	var g *Gate
	g.SetReclaimTrigger(func() { t.Fatal("trigger must never be invoked on a nil gate") })
	if ok, reason := g.Admit(); !ok || reason != "" {
		t.Fatalf("Admit() on nil gate = (%v, %q), want (true, \"\")", ok, reason)
	}
}

func TestStatusReturnsCachedSample(t *testing.T) {
	cfg := config.PressureConfig{Enabled: true, MinDiskFreePercent: 5, SampleIntervalSeconds: 60}
	want := Sample{DiskFreePct: 42, MemAvailablePct: 33, LoadPerCPU: 2}
	g := testGate(t, cfg, want)

	got := g.Status()
	if got != want {
		t.Fatalf("Status() = %+v, want %+v", got, want)
	}
}

func TestStatusNilGateReturnsAllNaN(t *testing.T) {
	var g *Gate
	s := g.Status()
	if !isNaN(s.DiskFreePct) || !isNaN(s.MemAvailablePct) || !isNaN(s.LoadPerCPU) {
		t.Fatalf("Status() on nil gate = %+v, want all-NaN", s)
	}
}

func TestThresholdsReturnsConfiguredValues(t *testing.T) {
	cfg := config.PressureConfig{
		Enabled:                true,
		MinDiskFreePercent:     5,
		WarningDiskFreePercent: 15,
		SampleIntervalSeconds:  15,
	}
	g := testGate(t, cfg, Sample{DiskFreePct: 50, MemAvailablePct: 50, LoadPerCPU: 1})

	warning, critical := g.Thresholds()
	if warning != 15 || critical != 5 {
		t.Fatalf("Thresholds() = (%v, %v), want (15, 5)", warning, critical)
	}
}

func TestThresholdsNilGateReturnsZero(t *testing.T) {
	var g *Gate
	warning, critical := g.Thresholds()
	if warning != 0 || critical != 0 {
		t.Fatalf("Thresholds() on nil gate = (%v, %v), want (0, 0)", warning, critical)
	}
}

func isNaN(f float64) bool { return f != f }
