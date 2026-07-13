package pressure

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
)

func testGate(t *testing.T, cfg config.PressureConfig, sample Sample) *Gate {
	t.Helper()
	g := New(cfg, "/tmp", nil)
	if g == nil {
		return nil
	}
	g.sampleFn = func(string) (Sample, error) { return sample, nil }
	return g
}

func TestAdmit(t *testing.T) {
	baseCfg := config.PressureConfig{
		Enabled:                true,
		MinDiskFreePercent:     5,
		MinMemAvailablePercent: 8,
		MaxLoadPerCPU:          8,
		SampleIntervalSeconds:  15,
	}

	cases := []struct {
		name       string
		cfg        config.PressureConfig
		sample     Sample
		wantAdmit  bool
		wantReason string
	}{
		{
			name:      "healthy sample admits",
			cfg:       baseCfg,
			sample:    Sample{DiskFreePct: 50, MemAvailablePct: 50, LoadPerCPU: 1},
			wantAdmit: true,
		},
		{
			name:       "disk threshold trips independently",
			cfg:        baseCfg,
			sample:     Sample{DiskFreePct: 1, MemAvailablePct: 50, LoadPerCPU: 1},
			wantAdmit:  false,
			wantReason: "disk free",
		},
		{
			name:       "memory threshold trips independently",
			cfg:        baseCfg,
			sample:     Sample{DiskFreePct: 50, MemAvailablePct: 1, LoadPerCPU: 1},
			wantAdmit:  false,
			wantReason: "memory available",
		},
		{
			name:       "load threshold trips independently",
			cfg:        baseCfg,
			sample:     Sample{DiskFreePct: 50, MemAvailablePct: 50, LoadPerCPU: 99},
			wantAdmit:  false,
			wantReason: "load per cpu",
		},
		{
			name:      "NaN signal is skipped even though it would otherwise trip",
			cfg:       baseCfg,
			sample:    Sample{DiskFreePct: math.NaN(), MemAvailablePct: 50, LoadPerCPU: 1},
			wantAdmit: true,
		},
		{
			name:      "sampler failure (all-NaN sample) fails open",
			cfg:       baseCfg,
			sample:    Sample{DiskFreePct: math.NaN(), MemAvailablePct: math.NaN(), LoadPerCPU: math.NaN()},
			wantAdmit: true,
		},
		{
			name: "threshold <= 0 disables that dimension",
			cfg: config.PressureConfig{
				Enabled:                true,
				MinDiskFreePercent:     0, // disabled
				MinMemAvailablePercent: 8,
				MaxLoadPerCPU:          8,
				SampleIntervalSeconds:  15,
			},
			sample:    Sample{DiskFreePct: 0.01, MemAvailablePct: 50, LoadPerCPU: 1},
			wantAdmit: true,
		},
		{
			name:      "disabled gate admits",
			cfg:       config.PressureConfig{Enabled: false},
			sample:    Sample{DiskFreePct: 0, MemAvailablePct: 0, LoadPerCPU: 999},
			wantAdmit: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := testGate(t, tc.cfg, tc.sample)
			ok, reason := g.Admit()
			if ok != tc.wantAdmit {
				t.Fatalf("Admit() ok = %v, want %v (reason=%q)", ok, tc.wantAdmit, reason)
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason = %q, want substring %q", reason, tc.wantReason)
			}
		})
	}
}

func TestAdmitFailsOpenOnSamplerError(t *testing.T) {
	g := New(config.PressureConfig{Enabled: true, MaxLoadPerCPU: 8, SampleIntervalSeconds: 15}, "/tmp", nil)
	if g == nil {
		t.Fatal("New returned nil for enabled gate")
	}
	g.sampleFn = func(string) (Sample, error) {
		return Sample{}, assertError("probe failed")
	}
	if ok, reason := g.Admit(); !ok || reason != "" {
		t.Fatalf("Admit() = (%v, %q), want fail-open true/empty", ok, reason)
	}
}

func TestAdmitCachesSampleWithinTTL(t *testing.T) {
	g := New(config.PressureConfig{Enabled: true, MaxLoadPerCPU: 8, SampleIntervalSeconds: 60}, "/tmp", nil)
	if g == nil {
		t.Fatal("New returned nil for enabled gate")
	}
	calls := 0
	g.sampleFn = func(string) (Sample, error) {
		calls++
		return Sample{DiskFreePct: 50, MemAvailablePct: 50, LoadPerCPU: 1}, nil
	}
	for range 5 {
		if ok, _ := g.Admit(); !ok {
			t.Fatalf("Admit() = false, want true")
		}
	}
	if calls != 1 {
		t.Fatalf("sampleFn called %d times, want 1 (cache should serve repeats within TTL)", calls)
	}
}

func TestNewCoercesNonPositiveSampleInterval(t *testing.T) {
	g := New(config.PressureConfig{Enabled: true, MaxLoadPerCPU: 8, SampleIntervalSeconds: 0}, "/tmp", nil)
	if g == nil {
		t.Fatal("New returned nil for an enabled config")
	}
	if g.interval != DefaultSampleIntervalSeconds*time.Second {
		t.Fatalf("interval = %v, want %v", g.interval, DefaultSampleIntervalSeconds*time.Second)
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }
