package loadscale

import (
	"runtime"
	"testing"
	"time"
)

func TestOversubscriptionFactor(t *testing.T) {
	tests := []struct {
		name    string
		load    float64
		ok      bool
		ceiling int64
		want    int64
	}{
		{name: "unreadable load", load: 0, ok: false, ceiling: 8, want: 1},
		{name: "below one", load: 0.9, ok: true, ceiling: 8, want: 1},
		{name: "exactly one", load: 1, ok: true, ceiling: 8, want: 1},
		{name: "ceil fractional load", load: 1.1, ok: true, ceiling: 8, want: 2},
		{name: "clamp at ceiling", load: 99, ok: true, ceiling: 8, want: 8},
		{name: "bad ceiling cannot shorten", load: 3, ok: true, ceiling: 0, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OversubscriptionFactor(tt.load, tt.ok, tt.ceiling); got != tt.want {
				t.Fatalf("OversubscriptionFactor(%v, %v, %d) = %d, want %d", tt.load, tt.ok, tt.ceiling, got, tt.want)
			}
		})
	}
}

func TestScaleDurationDoesNotShorten(t *testing.T) {
	if got := ScaleDuration(2*time.Second, 0); got != 2*time.Second {
		t.Fatalf("ScaleDuration with zero factor = %s, want 2s", got)
	}
	if got := ScaleDuration(2*time.Second, 3); got != 6*time.Second {
		t.Fatalf("ScaleDuration with factor 3 = %s, want 6s", got)
	}
}

func TestLoadPerCPU(t *testing.T) {
	got, ok := LoadPerCPU(float64(runtime.NumCPU()) * 2)
	if !ok {
		t.Fatal("LoadPerCPU returned ok=false")
	}
	if got != 2 {
		t.Fatalf("LoadPerCPU = %v, want 2", got)
	}
}

func TestLoadPerCPUUsesLowerGOMAXPROCS(t *testing.T) {
	original := runtime.GOMAXPROCS(0)
	runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(original) })

	got, ok := LoadPerCPU(2)
	if !ok {
		t.Fatal("LoadPerCPU returned ok=false")
	}
	if got != 2 {
		t.Fatalf("LoadPerCPU with GOMAXPROCS=1 = %v, want 2", got)
	}
}
