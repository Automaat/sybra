package stats

import "testing"

func TestEstimateCost(t *testing.T) {
	tests := []struct {
		model   string
		in, out int
		want    float64
	}{
		{"o4-mini", 1_000_000, 1_000_000, 1.10 + 4.40},
		{"o3", 1_000_000, 0, 10.00},
		{"gpt-4o", 0, 1_000_000, 10.00},
		{"gpt-4o-mini", 1_000_000, 1_000_000, 0.15 + 0.60},
		{"unknown-model", 1_000_000, 1_000_000, 0},
		{"", 100, 50, 0},
		{"o4-mini", 100, 50, 100.0/1_000_000*1.10 + 50.0/1_000_000*4.40},
	}
	for _, tt := range tests {
		got := EstimateCost(tt.model, tt.in, tt.out)
		diff := got - tt.want
		if diff > 1e-9 || diff < -1e-9 {
			t.Errorf("EstimateCost(%q, %d, %d) = %g, want %g", tt.model, tt.in, tt.out, got, tt.want)
		}
	}
}
