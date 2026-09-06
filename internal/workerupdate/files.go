package workerupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/version"
)

// Every ancestor is checked: a root-owned file in a worker-writable parent
// is not a trusted deployment input. No recursive cleanup touches run state.
func trustedPath(path string) error {
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("worker updater: deployment inputs must be root-owned, non-symlink, and not group/world writable")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func (r *runner) save(j *journal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteMode(r.journalPath(), data, 0600)
}

func (r *runner) journalPath() string { return filepath.Join(r.cfg.StateDir, "update.json") }

func (r *runner) load() (journal, error) {
	var j journal
	data, err := os.ReadFile(r.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if len(data) > 4096 {
		return j, errors.New("worker updater: oversized journal")
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, err
	}
	if j.Phase == "" || j.WorkerID != r.cfg.WorkerID || j.LeaderHomeID != r.cfg.LeaderHomeID || !validNonce(j.ID) || !version.ValidRevision(j.Revision) || !version.ValidRevision(j.Previous) {
		return j, errors.New("worker updater: invalid journal; operator recovery required")
	}
	return j, nil
}

func (r *runner) pointer() (string, error) {
	target, err := os.Readlink(r.cfg.CurrentLink)
	if err != nil {
		return "", err
	}
	revision := filepath.Base(target)
	if !version.ValidRevision(revision) || target != filepath.Join(r.cfg.ReleaseRoot, revision) {
		return "", errors.New("worker updater: current link is outside retained SHA releases")
	}
	return revision, nil
}

func (r *runner) switchTo(revision string) error {
	if !version.ValidRevision(revision) {
		return errors.New("worker updater: invalid release revision")
	}
	// The temporary link is fixed, private to this flock-serialized updater.
	// Remove only a previously interrupted symlink, never a directory/file.
	tmp := r.cfg.CurrentLink + ".updating"
	if info, err := os.Lstat(tmp); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("worker updater: unexpected temporary pointer")
		}
		if err := os.Remove(tmp); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(filepath.Join(r.cfg.ReleaseRoot, revision), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.cfg.CurrentLink); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(r.cfg.CurrentLink))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
