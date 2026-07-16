package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
)

// ownerFileName records the UID/GID Sybra's own process ran as when it
// created a per-task sandbox dir. Written once at creation time (see
// prepareTaskDir) so RemoveContext can later tell "a docker/k8s sandbox left
// files owned by another UID" (typically root, via a bind mount) apart from
// a plain permission-bit issue RemoveAllForce already retries past.
const ownerFileName = ".sybra-owner.json"

// quarantineDirName is the reserved child of Manager.dataDir holding
// QuarantineEntry records. Excluded from CleanupOrphaned's task-dir sweep so
// it is never mistaken for an orphaned task's sandbox dir.
const quarantineDirName = ".quarantine"

// sandboxRemoveBackoffs schedules bounded retries of a transient (busy
// mount/process) removal failure — the same shape immediately after
// StopContext tears down a container or k3d cluster: the bind mount or
// overlay fs backing it can take a moment to actually release. Indirected
// for tests — test init swaps in zeros to skip real waits.
var (
	sandboxRemoveBackoffs = []time.Duration{300 * time.Millisecond, 800 * time.Millisecond, 1500 * time.Millisecond}
	sandboxRemoveSleep    = time.Sleep
)

const (
	dockerChownImage = "alpine:3"
	normalizeTimeout = 30 * time.Second
)

type ownerRecord struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

func writeOwnerRecord(taskDir string) error {
	rec := ownerRecord{UID: os.Getuid(), GID: os.Getgid()}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(taskDir, ownerFileName), data, 0o600)
}

func readOwnerRecord(taskDir string) (ownerRecord, bool) {
	data, err := os.ReadFile(filepath.Join(taskDir, ownerFileName))
	if err != nil {
		return ownerRecord{}, false
	}
	var rec ownerRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return ownerRecord{}, false
	}
	return rec, true
}

func currentOwnerRecord() ownerRecord {
	return ownerRecord{UID: os.Getuid(), GID: os.Getgid()}
}

func cleanupOwnerRecord(taskDir string) (ownerRecord, bool) {
	if rec, ok := readOwnerRecord(taskDir); ok {
		return rec, true
	}
	return currentOwnerRecord(), false
}

// mismatchedOwnership reports whether any entry under taskDir is owned by a
// UID/GID other than want — the signature of a docker/k8s sandbox writing
// host-visible files as a different (often root) user via a bind mount. A
// stat failure on an individual entry is ignored (best effort; the removal
// attempt that follows surfaces any real problem).
func mismatchedOwnership(taskDir string, want ownerRecord) bool {
	mismatch := false
	_ = filepath.WalkDir(taskDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !mismatch {
			if info, infoErr := d.Info(); infoErr == nil {
				if st, ok := info.Sys().(*syscall.Stat_t); ok {
					if int(st.Uid) != want.UID || int(st.Gid) != want.GID {
						mismatch = true
					}
				}
			}
		}
		return nil
	})
	return mismatch
}

// ownershipNormalizer attempts to make every file under path owned by
// uid:gid, using whatever privileged mechanism is available.
type ownershipNormalizer func(ctx context.Context, path string, uid, gid int) error

// dockerChownNormalizer shells out to a throwaway --rm docker container to
// chown path back to uid:gid. Docker containers commonly run as root, which
// gives dockerd the privilege Sybra's own unprivileged process lacks to
// reclaim files a sandboxed container left owned by another UID — Docker is
// the "privileged cleanup boundary" here, already a hard dependency of every
// docker/k8s sandbox this package manages.
func dockerChownNormalizer(ctx context.Context, path string, uid, gid int) error {
	ctx, cancel := context.WithTimeout(ctx, normalizeTimeout)
	defer cancel()
	args := []string{"run", "--rm", "-v", path + ":/target", dockerChownImage, "chown", "-R", fmt.Sprintf("%d:%d", uid, gid), "/target"}
	out, err := runCmd(ctx, "", nil, "docker", args...)
	if err != nil {
		return fmt.Errorf("docker chown normalize: %w\n%s", err, out)
	}
	return nil
}

// isTransientRemoveErr reports whether err is the kind of removal failure
// that is expected to clear on its own shortly — a bind mount or overlay fs
// still tearing down right after StopContext killed the container/cluster
// that used it. Anything else (plain EPERM/EACCES from an unrecoverable
// ownership mismatch, a missing path, ...) is not retried — see
// Manager.quarantine.
func isTransientRemoveErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ETXTBSY) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "device or resource busy") || strings.Contains(msg, "resource busy")
}

func isPermissionRemoveErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "permission denied") || strings.Contains(msg, "operation not permitted")
}

// QuarantineEntry records a sandbox cleanup failure that survived ownership
// normalization and the transient-busy retry budget — genuinely unsafe (or
// at least not automatically recoverable) rather than transient. Reported by
// Manager.QuarantinedEntries for the health checker to surface as a
// deduplicated finding, and not retried again by CleanupOrphaned until the
// owning task is deleted.
type QuarantineEntry struct {
	TaskID        string    `json:"taskId"`
	Path          string    `json:"path"`
	BytesRetained int64     `json:"bytesRetained"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"lastError"`
	FirstFailedAt time.Time `json:"firstFailedAt"`
	LastFailedAt  time.Time `json:"lastFailedAt"`
}

func quarantineDir(dataDir string) string {
	return filepath.Join(dataDir, quarantineDirName)
}

func quarantinePath(dataDir, taskID string) string {
	return filepath.Join(quarantineDir(dataDir), taskID+".json")
}

func (m *Manager) loadQuarantine(taskID string) (QuarantineEntry, bool) {
	data, err := os.ReadFile(quarantinePath(m.dataDir, taskID))
	if err != nil {
		return QuarantineEntry{}, false
	}
	var e QuarantineEntry
	if json.Unmarshal(data, &e) != nil {
		return QuarantineEntry{}, false
	}
	return e, true
}

func (m *Manager) saveQuarantine(e QuarantineEntry) error {
	if err := os.MkdirAll(quarantineDir(m.dataDir), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(quarantinePath(m.dataDir, e.TaskID), data)
}

func (m *Manager) clearQuarantine(taskID string) {
	_ = os.Remove(quarantinePath(m.dataDir, taskID))
}

// QuarantinedEntries returns every currently quarantined sandbox cleanup
// failure, for the health checker to surface as findings.
func (m *Manager) QuarantinedEntries() []QuarantineEntry {
	entries, err := os.ReadDir(quarantineDir(m.dataDir))
	if err != nil {
		return nil
	}
	out := make([]QuarantineEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(quarantineDir(m.dataDir), e.Name()))
		if err != nil {
			continue
		}
		var rec QuarantineEntry
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// dirSize sums the size of every regular file under path, including path
// itself if it is a plain file. Feeds QuarantineEntry.BytesRetained, a
// reporting metric only — the caller treats a walk error as "unknown size"
// rather than failing.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
