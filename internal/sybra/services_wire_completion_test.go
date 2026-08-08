package sybra

import "testing"

func TestPostRunReconciliationSkipsDegradedApp(t *testing.T) {
	t.Parallel()
	if got := (*App)(nil).postRunReconciliation(); got != nil {
		t.Fatalf("nil app reconciler = %v, want nil", got)
	}
	if got := (&App{}).postRunReconciliation(); got != nil {
		t.Fatalf("app without task manager reconciler = %v, want nil", got)
	}
}
