package task

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/reject"
)

// TrashDir returns the directory soft-deleted tasks are moved into by
// Delete, sibling to the tasks dir (e.g. ~/.sybra/trash for
// ~/.sybra/tasks). Exposed for CLI diagnostics and tests.
func (s *Store) TrashDir() string {
	return s.trashDir
}

// newTrashGeneration creates and returns a fresh, empty directory under
// trashDir/<yyyy-mm-dd>/ to hold one Delete's worth of files for id. The
// first delete of id on a given date uses the bare id as the generation
// name; same-day redeletes (delete → restore → delete again) get a
// "<id>--<HHMMSS>-<nn>" suffix so they don't collide with — or silently
// overwrite — the earlier generation.
func (s *Store) newTrashGeneration(id string) (string, error) {
	dateDir := filepath.Join(s.trashDir, time.Now().UTC().Format(time.DateOnly))
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		return "", fmt.Errorf("create trash date dir: %w", err)
	}
	base := filepath.Join(dateDir, id)
	if err := os.Mkdir(base, 0o755); err == nil {
		return base, nil
	} else if !os.IsExist(err) {
		return "", err
	}
	for n := 1; ; n++ {
		candidate := filepath.Join(dateDir, fmt.Sprintf("%s--%s-%02d", id, time.Now().UTC().Format("150405"), n))
		if err := os.Mkdir(candidate, 0o755); err == nil {
			return candidate, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
}

// Delete moves task id's file, along with every sidecar it may own
// (comments, plans, contracts, critiques, research, decisions, brief, code
// reviews, plan drafts), into a dated generation directory under the trash
// dir instead of unlinking them — see TrashDir. Sidecars are moved before
// the primary "<id>.md" file so that a crash or rename failure partway
// through leaves the primary file as the source of truth for whether the
// task still "exists": either it's still in place (delete didn't complete)
// or it's gone (delete completed). If any rename fails partway through,
// every file already moved into genDir is rolled back to s.dir and genDir
// is removed, so a partial failure never leaves an orphaned generation
// (unlistable/unrestorable/unprunable — trashGenerationID requires the
// primary file to identify a generation) or a live task missing sidecars.
// There is deliberately no copy/delete fallback if a rename itself fails
// (e.g. trash on a different filesystem) — that error is returned as-is.
func (s *Store) Delete(id string) error {
	unlock, err := s.lockTask(id)
	if err != nil {
		return err
	}
	defer unlock()

	t, err := s.read(id)
	if err != nil {
		return err
	}
	files, err := s.taskFiles(id)
	if err != nil {
		return err
	}
	genDir, err := s.newTrashGeneration(id)
	if err != nil {
		return fmt.Errorf("create trash generation: %w", err)
	}
	primary := id + ".md"
	moved := make([]string, 0, len(files))
	rollback := func() {
		for _, base := range moved {
			_ = os.Rename(filepath.Join(genDir, base), filepath.Join(s.dir, base))
		}
		_ = os.Remove(genDir)
	}
	for _, base := range files {
		if err := os.Rename(filepath.Join(s.dir, base), filepath.Join(genDir, base)); err != nil {
			rollback()
			return fmt.Errorf("move %s to trash: %w", base, err)
		}
		moved = append(moved, base)
	}
	if err := os.Rename(t.FilePath, filepath.Join(genDir, primary)); err != nil {
		rollback()
		return fmt.Errorf("move task file to trash: %w", err)
	}
	s.planDrafts.invalidateIndex()
	s.deleteCachedTask(id)
	return nil
}

// TrashEntry describes one soft-deleted task generation for ListTrash and
// PruneTrash callers (CLI table/JSON output, prune logging).
type TrashEntry struct {
	ID          string    `json:"id"`
	Generation  string    `json:"generation"`
	DeletedDate string    `json:"deleted_date"`
	DeletedAt   time.Time `json:"deleted_at"`
	Title       string    `json:"title"`
}

// trashGenerationID returns the task id owned by a trash generation
// directory, identified by the one file inside it that ends in ".md" but is
// not itself a sidecar (IsSidecarFile) — i.e. the primary "<id>.md". This
// avoids relying on the generation directory's name (which is a convenience
// naming scheme, not a contract) to recover id, so list/restore/prune never
// prefix-match an arbitrary id. ok is false for an empty, already-restored,
// or corrupt generation. Read errors are returned so callers can surface
// unreadable generations instead of silently skipping them.
func trashGenerationID(genDir string) (id string, ok bool, err error) {
	entries, err := os.ReadDir(genDir)
	if err != nil {
		return "", false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if strings.HasSuffix(base, ".md") && !IsSidecarFile(base) {
			return strings.TrimSuffix(base, ".md"), true, nil
		}
	}
	return "", false, nil
}

// walkTrashGenerations calls fn for every generation directory under
// trashDir/<yyyy-mm-dd>/, passing the deletion date, the generation
// directory's basename, and its full path. fn's error stops the walk for
// that generation only (recorded via the returned slice) — one unreadable
// generation must not abort ListTrash/PruneTrash for the rest.
func (s *Store) walkTrashGenerations(fn func(date, generation, path string) error) []error {
	var errs []error
	dateEntries, err := os.ReadDir(s.trashDir)
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("read trash dir: %w", err))
		}
		return errs
	}
	for _, de := range dateEntries {
		if !de.IsDir() {
			continue
		}
		date := de.Name()
		dateDir := filepath.Join(s.trashDir, date)
		genEntries, err := os.ReadDir(dateDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read trash date dir %s: %w", date, err))
			continue
		}
		for _, ge := range genEntries {
			if !ge.IsDir() {
				continue
			}
			if err := fn(date, ge.Name(), filepath.Join(dateDir, ge.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// trashEntryFor builds a TrashEntry for a generation directory, reading its
// title from the trashed primary file (best-effort — a parse failure leaves
// Title empty rather than failing the whole entry).
func trashEntryFor(date, generation, path, id string) TrashEntry {
	entry := TrashEntry{ID: id, Generation: generation, DeletedDate: date}
	if info, err := os.Stat(path); err == nil {
		entry.DeletedAt = info.ModTime()
	}
	if t, err := Parse(filepath.Join(path, id+".md")); err == nil {
		entry.Title = t.Title
	}
	return entry
}

// ListTrash returns one entry per trashed generation across all dates,
// newest first. Multiple generations for the same id (repeated
// delete/restore cycles) each get their own entry so callers can
// distinguish them.
func (s *Store) ListTrash() ([]TrashEntry, error) {
	var out []TrashEntry
	errs := s.walkTrashGenerations(func(date, generation, path string) error {
		id, ok, err := trashGenerationID(path)
		if err != nil {
			return fmt.Errorf("read trash generation %s/%s: %w", date, generation, err)
		}
		if !ok {
			return nil
		}
		out = append(out, trashEntryFor(date, generation, path, id))
		return nil
	})
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	slices.SortFunc(out, func(a, b TrashEntry) int {
		return b.DeletedAt.Compare(a.DeletedAt)
	})
	return out, nil
}

// newestGeneration returns the path to the most recently deleted trash
// generation owned by id, or "" if none exist.
func (s *Store) newestGeneration(id string) (string, error) {
	var newest string
	var newestAt time.Time
	errs := s.walkTrashGenerations(func(date, generation, path string) error {
		gid, ok, err := trashGenerationID(path)
		if err != nil {
			return fmt.Errorf("read trash generation %s/%s: %w", date, generation, err)
		}
		if !ok || gid != id {
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return fmt.Errorf("stat trash generation %s: %w", path, statErr)
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest = path
			newestAt = info.ModTime()
		}
		return nil
	})
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	return newest, nil
}

// removeEmptyTrashDirs removes genDir (expected empty after a restore) and,
// if that leaves its parent date directory empty too, removes that as well.
func removeEmptyTrashDirs(genDir string) {
	if err := os.Remove(genDir); err != nil {
		return
	}
	dateDir := filepath.Dir(genDir)
	entries, err := os.ReadDir(dateDir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(dateDir)
	}
}

// RestoreFromTrash moves id's newest trash generation back into the tasks
// dir under the same per-task lock Delete uses, restoring sidecars before
// the primary "<id>.md" file (mirror of Delete's ordering). If any rename
// fails partway through, every file already restored to s.dir is rolled
// back into genDir so a partial failure never leaves orphaned "<id>.*"
// sidecars in the live tasks dir with no primary file — such litter would
// be invisible to List, ListTrash, and PruneTrash, and unreachable by a
// future Delete(id), which requires the primary file to exist. Refuses if
// a live task with id already exists rather than silently overwriting it.
func (s *Store) RestoreFromTrash(id string) (Task, error) {
	unlock, err := s.lockTask(id)
	if err != nil {
		return Task{}, err
	}
	defer unlock()

	livePath, err := s.safePath(id)
	if err != nil {
		return Task{}, err
	}
	if _, err := os.Stat(livePath); err == nil {
		return Task{}, reject.New("task %s already exists, refusing to overwrite with trashed copy", id)
	} else if !os.IsNotExist(err) {
		return Task{}, fmt.Errorf("stat live task: %w", err)
	}

	genDir, err := s.newestGeneration(id)
	if err != nil {
		return Task{}, err
	}
	if genDir == "" {
		return Task{}, fmt.Errorf("no trashed task found for id %s: %w", id, os.ErrNotExist)
	}

	entries, err := os.ReadDir(genDir)
	if err != nil {
		return Task{}, fmt.Errorf("read trash generation: %w", err)
	}
	primary := id + ".md"
	restored := make([]string, 0, len(entries))
	rollback := func() {
		for _, base := range restored {
			_ = os.Rename(filepath.Join(s.dir, base), filepath.Join(genDir, base))
		}
	}
	for _, e := range entries {
		base := e.Name()
		if base == primary {
			continue
		}
		if err := os.Rename(filepath.Join(genDir, base), filepath.Join(s.dir, base)); err != nil {
			rollback()
			return Task{}, fmt.Errorf("restore %s: %w", base, err)
		}
		restored = append(restored, base)
	}
	if err := os.Rename(filepath.Join(genDir, primary), livePath); err != nil {
		rollback()
		return Task{}, fmt.Errorf("restore task file: %w", err)
	}
	removeEmptyTrashDirs(genDir)

	s.planDrafts.invalidateIndex()
	t, err := s.Get(id)
	if err != nil {
		return Task{}, err
	}
	s.storeTaskCache(t)
	return t, nil
}

// TrashPruneReport summarizes one PruneTrash run.
type TrashPruneReport struct {
	Scanned int
	Removed int
	Entries []TrashEntry
	Errors  []error
}

// pruneGeneration removes genDir under id's per-task lock, rechecking that
// it still exists once the lock is held — a concurrent RestoreFromTrash may
// have already moved it out from under the unlocked scan in PruneTrash.
// removed is false (with a nil error) when the recheck lost the race, so
// callers can distinguish "actually deleted" from "already gone" instead of
// double-counting a generation a concurrent restore already consumed.
func (s *Store) pruneGeneration(id, genDir string) (removed bool, err error) {
	unlock, err := s.lockTask(id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Stat(genDir); os.IsNotExist(err) {
		return false, nil
	}
	if err := os.RemoveAll(genDir); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteTrashedGeneration permanently removes id's newest trashed
// generation right away, bypassing the retention window. This is the
// escape hatch PruneTrash can't provide: a compliance request or a leaked
// credential needs trashed content gone immediately, not after
// RetentionDays or the next prune sweep.
func (s *Store) DeleteTrashedGeneration(id string) (bool, error) {
	genDir, err := s.newestGeneration(id)
	if err != nil {
		return false, err
	}
	if genDir == "" {
		return false, fmt.Errorf("no trashed task found for id %s: %w", id, os.ErrNotExist)
	}
	return s.pruneGeneration(id, genDir)
}

// PruneTrash permanently removes trash generations deleted more than
// retentionDays ago. A negative retentionDays disables pruning entirely
// (no-op). Each generation is removed under its own id's per-task lock, so
// pruning can never race a concurrent restore of the same id — unlike a
// naive "remove the whole date directory" sweep, which could not take a
// single lock covering every id it touches.
func (s *Store) PruneTrash(retentionDays int) (TrashPruneReport, error) {
	if retentionDays < 0 {
		return TrashPruneReport{}, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.DateOnly)
	return s.pruneTrashBefore(cutoff)
}

// PruneAllTrash permanently removes every trash generation regardless of
// age — the "empty the trash now" counterpart to PruneTrash's
// retention-gated sweep, for a `trash empty` CLI command.
func (s *Store) PruneAllTrash() (TrashPruneReport, error) {
	// No real deletion date sorts >= this, so every date dir qualifies.
	return s.pruneTrashBefore("9999-99-99")
}

// pruneTrashBefore is the shared body of PruneTrash and PruneAllTrash: it
// removes every trash generation dated strictly before cutoff (a
// time.DateOnly string).
func (s *Store) pruneTrashBefore(cutoff string) (TrashPruneReport, error) {
	var rep TrashPruneReport
	dateEntries, err := os.ReadDir(s.trashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, fmt.Errorf("read trash dir: %w", err)
	}
	for _, de := range dateEntries {
		if !de.IsDir() || de.Name() >= cutoff {
			continue
		}
		date := de.Name()
		dateDir := filepath.Join(s.trashDir, date)
		genEntries, err := os.ReadDir(dateDir)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("read trash date dir %s: %w", date, err))
			continue
		}
		for _, ge := range genEntries {
			if !ge.IsDir() {
				continue
			}
			rep.Scanned++
			genDir := filepath.Join(dateDir, ge.Name())
			id, ok, err := trashGenerationID(genDir)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("read trash generation %s/%s: %w", date, ge.Name(), err))
				continue
			}
			if !ok {
				continue
			}
			entry := trashEntryFor(date, ge.Name(), genDir, id)
			removed, err := s.pruneGeneration(id, genDir)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("prune %s/%s: %w", date, ge.Name(), err))
				continue
			}
			if !removed {
				continue
			}
			rep.Removed++
			rep.Entries = append(rep.Entries, entry)
		}
		if remaining, err := os.ReadDir(dateDir); err == nil && len(remaining) == 0 {
			_ = os.Remove(dateDir)
		}
	}
	return rep, nil
}
