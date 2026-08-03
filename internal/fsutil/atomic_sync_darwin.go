//go:build darwin

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// syncDir makes the directory entry durable when the filesystem supports it.
// APFS commonly rejects directory fsync with EINVAL, which must not turn a
// successfully renamed task write into an application failure.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent directory for sync: %w", err)
	}
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		_ = f.Close()
		return fmt.Errorf("sync parent directory: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close parent directory: %w", err)
	}
	return nil
}
