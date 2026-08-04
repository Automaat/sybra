//go:build unix && !darwin

package fsutil

import (
	"fmt"
	"os"
)

// syncDir persists a completed rename. Syncing the replacement file alone
// does not guarantee that its directory entry survives a power loss.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent directory for sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync parent directory: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close parent directory: %w", err)
	}
	return nil
}
