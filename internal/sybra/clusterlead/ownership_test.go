package clusterlead

import (
	"fmt"
	"sync"
	"testing"
)

func TestTaskOwnershipLocksReclaimBurst(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			unlock := lockTaskOwnership(fmt.Sprintf("task-%d", i))
			unlock()
		})
	}
	wg.Wait()
	if got := taskOwnershipLocks.Len(); got != 0 {
		t.Fatalf("ownership lock entries = %d, want burst tasks reclaimed", got)
	}
}
