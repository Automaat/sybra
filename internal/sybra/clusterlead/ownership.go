package clusterlead

import "github.com/Automaat/sybra/internal/fsutil"

// taskOwnershipLocks serializes every operation that can observe or mutate a
// task's follower ownership. It is deliberately package-wide: reconciliation
// repairs run from Mirror while manual moves and leader edits run from
// Assigner, and separate lock sets would let an old-node repair race a move.
var taskOwnershipLocks fsutil.KeyedLocker

func lockTaskOwnership(taskID string) func() {
	return taskOwnershipLocks.LockLocal(taskID)
}
