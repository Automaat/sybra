package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrLockUnsupported marks platforms where fsutil cannot provide a
// cross-process file lock primitive. Callers can degrade to a warning/no-op
// instead of turning platform support gaps into startup failures.
var ErrLockUnsupported = errors.New("fsutil: cross-process file locking is not supported on this platform")

// AtomicWrite writes data to path via a temp file + rename to prevent
// partial reads from concurrent goroutines. The temp file is removed on
// every error path — including a failed rename into a read-only target
// directory — so repeated write failures don't fill the disk with orphans.
//
// The rename preserves whatever mode the temp file has, which os.CreateTemp
// sets to 0600 regardless of the target's existing permissions. To avoid
// silently changing an existing file's mode on every write, AtomicWrite chmods
// the temp file to match path's current mode before renaming. Newly created
// files keep CreateTemp's restrictive default, which avoids overriding the
// caller's umask with a broader mode.
func AtomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	if info, err := os.Stat(path); err == nil {
		if err := os.Chmod(tmp, info.Mode().Perm()); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RemoveAllForce removes path and everything under it, tolerating read-only
// files and directories (e.g. Go module cache entries, which ship 0444/0555
// permissions). It tries a plain os.RemoveAll first; on failure it walks the
// tree — confined to path via os.Root to avoid a symlink TOCTOU escape —
// chmod'ing every directory to 0700 and every file to 0600 (best effort:
// per-entry errors are ignored, since the retry below is what surfaces the
// real failure), then retries os.RemoveAll once and returns that result.
func RemoveAllForce(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}

	if root, err := os.OpenRoot(path); err == nil {
		_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				if d.IsDir() {
					_ = root.Chmod(p, 0o700)
				} else {
					_ = root.Chmod(p, 0o600)
				}
			}
			return nil
		})
		_ = root.Close()
	}

	return os.RemoveAll(path)
}

// ListFiles returns absolute paths of non-directory entries in dir whose
// names end with suffix (e.g. ".md", ".yaml", ".json").
func ListFiles(dir, suffix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths, nil
}
