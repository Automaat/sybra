//go:build unix

package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The mode is explicit precisely so it does not vary per machine: an
// executable shim on the agent PATH must stay executable whatever the
// operator's umask is.
func TestAtomicWriteMode_SurvivesRestrictiveUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	path := filepath.Join(t.TempDir(), "shim")
	if err := AtomicWriteMode(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("AtomicWriteMode: %v", err)
		panic("unreachable")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %v, want %v under umask 0077", got, os.FileMode(0o755))
	}
}

// AtomicWrite is the opposite contract, and the distinction is why record
// stores use it: a new record must inherit the umask rather than be forced
// world-readable.
func TestAtomicWrite_DoesNotWidenModeUnderRestrictiveUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	path := filepath.Join(t.TempDir(), "rec.json")
	if err := AtomicWrite(path, []byte("{}")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("mode = %v, want no group/other bits under umask 0077", got)
	}
}
