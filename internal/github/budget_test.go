package github

import (
	"context"
	"testing"
	"time"
)

func TestRefreshRateBudget_FeedsGateAndScalesIntervals(t *testing.T) {
	orig := ghGate
	t.Cleanup(func() { ghGate = orig })

	tests := []struct {
		name       string
		body       string
		wantFactor float64
	}{
		{
			name:       "healthy budget keeps intervals",
			body:       `{"resources":{"core":{"limit":5000,"remaining":4800,"reset":0},"search":{"limit":30,"remaining":29,"reset":0}}}`,
			wantFactor: 1,
		},
		{
			name:       "low search budget stretches intervals",
			body:       `{"resources":{"core":{"limit":5000,"remaining":4800,"reset":0},"search":{"limit":30,"remaining":1,"reset":0}}}`,
			wantFactor: 4,
		},
		{
			name:       "moderate pressure scales 2x",
			body:       `{"resources":{"core":{"limit":5000,"remaining":500,"reset":0}}}`,
			wantFactor: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ghGate = newGHRequestGate()
			e := &fakeExecer{output: []byte(tt.body)}
			if err := refreshRateBudgetWith(context.Background(), e); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if got := BudgetPressureFactor(); got != tt.wantFactor {
				t.Fatalf("factor = %v, want %v", got, tt.wantFactor)
			}
			base := 5 * time.Minute
			want := time.Duration(float64(base) * tt.wantFactor)
			if got := ScaleInterval(base); got != want {
				t.Fatalf("ScaleInterval = %v, want %v", got, want)
			}
		})
	}
}

func TestBudgetPressureFactor_UnknownIsNeutral(t *testing.T) {
	orig := ghGate
	t.Cleanup(func() { ghGate = orig })
	ghGate = newGHRequestGate()
	if got := BudgetPressureFactor(); got != 1 {
		t.Fatalf("unknown budget factor = %v, want 1", got)
	}
}
