package sybra

import (
	"sync"
	"testing"
	"time"
)

func TestWaitGroupTimeout(t *testing.T) {
	tests := []struct {
		name  string
		block bool
		grace time.Duration
		want  bool
	}{
		{name: "completes within grace", block: false, grace: time.Second, want: true},
		{name: "times out when a goroutine never finishes", block: true, grace: 10 * time.Millisecond, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wg sync.WaitGroup
			wg.Add(1)
			if !tt.block {
				go wg.Done()
			}
			if got := waitGroupTimeout(&wg, tt.grace); got != tt.want {
				t.Fatalf("waitGroupTimeout = %v, want %v", got, tt.want)
			}
			if tt.block {
				wg.Done()
			}
		})
	}
}
