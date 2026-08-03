package clusterlead

import "sync"

// taskOwnershipLocks serializes every operation that can observe or mutate a
// task's follower ownership. It is deliberately package-wide: reconciliation
// repairs run from Mirror while manual moves and leader edits run from
// Assigner, and separate lock sets would let an old-node repair race a move.
var taskOwnershipLocks sync.Map

func lockTaskOwnership(taskID string) func() {
	v, _ := taskOwnershipLocks.LoadOrStore(taskID, &sync.Mutex{})
	mu, ok := v.(*sync.Mutex)
	if !ok {
		panic("clusterlead: invalid ownership lock")
	}
	mu.Lock()
	return mu.Unlock
}
