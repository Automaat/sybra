//go:build !unix

package fsutil

// syncDir is a no-op on platforms where opening and syncing a directory is
// unsupported. Sybra's production server and desktop targets are Unix.
func syncDir(_ string) error { return nil }
