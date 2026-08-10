package task

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/workflow"
)

// listFileState is the (size, modTime) pair List's cache uses to detect
// whether a task or sidecar file changed on disk since the cache was warmed.
type listFileState struct {
	size    int64
	modTime time.Time
}

// InvalidatePath clears any cached task/list state for the given task file.
// Non-task files are ignored.
func (s *Store) InvalidatePath(path string) {
	base := filepath.Base(path)
	if IsSidecarFile(base) {
		// An external plan-draft write/delete must drop the draft index so a
		// draft-less negative-cache hit can't mask a draft that appeared on
		// disk out-of-process.
		if IsPlanDraftFile(base) {
			s.planDrafts.invalidateIndex()
		}
		s.invalidateListCache()
		return
	}
	if !strings.HasSuffix(base, ".md") {
		return
	}
	id := strings.TrimSuffix(base, ".md")
	if id == "" {
		return
	}
	// Targeted refresh instead of a blanket invalidate: a single task file
	// changed (commonly the fsnotify echo of our OWN AtomicWrite ~200ms
	// earlier), so re-read just that file and patch its one cache entry rather
	// than dropping the whole list and forcing the next List() to re-parse and
	// re-clone every task. Keeps the list cache warm under active agent write
	// load, where it was perpetually cold.
	s.refreshCachedTask(id)
}

// refreshCachedTask re-reads a single task (with sidecars, so List output is
// identical to a full rebuild) and patches its entry in the warm list cache.
// A vanished file removes the entry; an unexpected read error falls back to a
// full invalidate. No-op when the cache is cold (storeTaskCache guards on it).
func (s *Store) refreshCachedTask(id string) {
	s.cacheMu.RLock()
	warm := s.listValid
	s.cacheMu.RUnlock()
	if !warm {
		return
	}
	if s.refreshBeforeLock != nil {
		s.refreshBeforeLock()
	}
	unlock, err := s.lockTask(id)
	if err != nil {
		s.invalidateListCache()
		return
	}
	defer unlock()

	t, err := s.Get(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.deleteCachedTask(id)
			return
		}
		s.invalidateListCache()
		return
	}
	s.storeTaskCache(t)
}

func (s *Store) cachedList() ([]Task, bool) {
	snapshot, ok := s.currentListSnapshot()
	if !ok {
		s.invalidateListCache()
		return nil, false
	}

	s.cacheMu.RLock()
	if !s.listValid || !sameListSnapshot(s.listSnapshot, snapshot) {
		s.cacheMu.RUnlock()
		return nil, false
	}
	tasks := cloneTasks(s.listCache)
	s.cacheMu.RUnlock()
	return tasks, true
}

func (s *Store) storeListCache(tasks []Task, snapshot map[string]listFileState) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.listCache = cloneTasks(tasks)
	s.listValid = true
	s.listSnapshot = cloneListSnapshot(snapshot)
}

func (s *Store) storeListCacheIfSnapshotFresh(tasks []Task, startSnapshot map[string]listFileState) bool {
	snapshot, ok := s.currentListSnapshot()
	if !ok {
		s.invalidateListCache()
		return false
	}
	if !sameListSnapshot(startSnapshot, snapshot) {
		s.invalidateListCache()
		return false
	}
	s.storeListCache(tasks, startSnapshot)
	return true
}

func (s *Store) storeTaskCache(t Task) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cloned := cloneTask(t)
	if !s.listValid {
		return
	}
	for i := range s.listCache {
		if s.listCache[i].ID != t.ID {
			continue
		}
		s.listCache[i] = cloned
		s.refreshListSnapshotLocked(t.ID)
		return
	}
	s.listCache = append(s.listCache, cloned)
	s.refreshListSnapshotLocked(t.ID)
}

func (s *Store) deleteCachedTask(id string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !s.listValid {
		return
	}
	for i := range s.listCache {
		if s.listCache[i].ID != id {
			continue
		}
		s.listCache = append(s.listCache[:i], s.listCache[i+1:]...)
		s.refreshListSnapshotLocked(id)
		return
	}
	s.refreshListSnapshotLocked(id)
}

func (s *Store) invalidateListCache() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.listValid = false
	s.listSnapshot = nil
}

func (s *Store) currentListSnapshot() (map[string]listFileState, bool) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, false
	}
	return s.listSnapshotFromEntries(entries)
}

func (s *Store) listSnapshotFromEntries(entries []os.DirEntry) (map[string]listFileState, bool) {
	snapshot := map[string]listFileState{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if !isListCacheFile(base) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, false
		}
		snapshot[base] = listFileState{
			size:    info.Size(),
			modTime: info.ModTime(),
		}
	}
	return snapshot, true
}

func isListCacheFile(base string) bool {
	if IsSidecarFile(base) {
		return true
	}
	return strings.HasSuffix(base, ".md")
}

func sameListSnapshot(a, b map[string]listFileState) bool {
	return sameListSnapshotExceptOwned(a, b, "")
}

func sameListSnapshotExceptOwned(a, b map[string]listFileState, id string) bool {
	for base, state := range a {
		if isOwnedListCacheFile(base, id) {
			continue
		}
		other, ok := b[base]
		if !ok || !sameListFileState(state, other) {
			return false
		}
	}
	for base := range b {
		if isOwnedListCacheFile(base, id) {
			continue
		}
		if _, ok := a[base]; !ok {
			return false
		}
	}
	return true
}

func sameListFileState(a, b listFileState) bool {
	return a.size == b.size && a.modTime.Equal(b.modTime)
}

func isOwnedListCacheFile(base, id string) bool {
	if id == "" {
		return false
	}
	if base == id+".md" || strings.HasPrefix(base, id+PlanDraftSidecarPrefix) {
		return true
	}
	for _, suffix := range SidecarFileSuffixes {
		if base == id+suffix {
			return true
		}
	}
	return false
}

func cloneListSnapshot(snapshot map[string]listFileState) map[string]listFileState {
	if snapshot == nil {
		return nil
	}
	return maps.Clone(snapshot)
}

func (s *Store) refreshListSnapshotLocked(id string) {
	if snapshot, ok := s.currentListSnapshot(); ok {
		if !sameListSnapshotExceptOwned(s.listSnapshot, snapshot, id) {
			s.listValid = false
			s.listSnapshot = nil
			return
		}
		s.listSnapshot = snapshot
		return
	}
	s.listValid = false
	s.listSnapshot = nil
}

func cloneTasks(tasks []Task) []Task {
	out := make([]Task, len(tasks))
	for i := range tasks {
		out[i] = cloneTask(tasks[i])
	}
	return out
}

func cloneTask(t Task) Task {
	clone := t
	clone.AllowedTools = slices.Clone(t.AllowedTools)
	clone.Tags = slices.Clone(t.Tags)
	clone.DependsOn = slices.Clone(t.DependsOn)
	clone.DependsOnConditions = slices.Clone(t.DependsOnConditions)
	clone.Attachments = slices.Clone(t.Attachments)
	clone.AgentRuns = slices.Clone(t.AgentRuns)
	if t.DueDate != nil {
		d := *t.DueDate
		clone.DueDate = &d
	}
	if t.ClosedAt != nil {
		c := *t.ClosedAt
		clone.ClosedAt = &c
	}
	if t.MirrorUpdatedAt != nil {
		m := *t.MirrorUpdatedAt
		clone.MirrorUpdatedAt = &m
	}
	if t.Workflow != nil {
		wfClone := cloneWorkflow(*t.Workflow)
		clone.Workflow = &wfClone
	}
	return clone
}

func cloneWorkflow(wf workflow.Execution) workflow.Execution {
	clone := wf
	clone.StepHistory = slices.Clone(wf.StepHistory)
	if wf.Variables != nil {
		clone.Variables = make(map[string]string, len(wf.Variables))
		maps.Copy(clone.Variables, wf.Variables)
	}
	if wf.StepCounts != nil {
		clone.StepCounts = make(map[string]int, len(wf.StepCounts))
		maps.Copy(clone.StepCounts, wf.StepCounts)
	}
	if wf.CompletedAt != nil {
		ts := *wf.CompletedAt
		clone.CompletedAt = &ts
	}
	// Deep-copy ParallelInflight: the outer map and every *ParallelChildren +
	// nested *ChildStatus must be independent. Without this, a Task fetched
	// via List() shares the in-flight bookkeeping with the listCache entry,
	// so a caller that mutates wf.ParallelInflight on a returned clone
	// silently corrupts cached state — and any subsequent List() observes the
	// torn maps until the cache is invalidated.
	if wf.ParallelInflight != nil {
		clone.ParallelInflight = make(map[string]*workflow.ParallelChildren, len(wf.ParallelInflight))
		for k, v := range wf.ParallelInflight {
			if v == nil {
				clone.ParallelInflight[k] = nil
				continue
			}
			pcClone := *v
			if v.Children != nil {
				pcClone.Children = make(map[string]*workflow.ChildStatus, len(v.Children))
				for ck, cv := range v.Children {
					if cv == nil {
						pcClone.Children[ck] = nil
						continue
					}
					csClone := *cv
					pcClone.Children[ck] = &csClone
				}
			}
			clone.ParallelInflight[k] = &pcClone
		}
	}
	// Deep-copy BestOfNInflight for the same reason as ParallelInflight above:
	// each attempt's *AttemptStatus must be independent across List()/Get()
	// clones, or a caller mutating a returned clone's attempt slots (e.g. while
	// dispatching the next attempt) would silently corrupt the cached copy.
	if wf.BestOfNInflight != nil {
		clone.BestOfNInflight = make(map[string]*workflow.BestOfNInflight, len(wf.BestOfNInflight))
		for k, v := range wf.BestOfNInflight {
			if v == nil {
				clone.BestOfNInflight[k] = nil
				continue
			}
			bnClone := *v
			if v.Attempts != nil {
				bnClone.Attempts = make(map[string]*workflow.AttemptStatus, len(v.Attempts))
				for ak, av := range v.Attempts {
					if av == nil {
						bnClone.Attempts[ak] = nil
						continue
					}
					asClone := *av
					bnClone.Attempts[ak] = &asClone
				}
			}
			clone.BestOfNInflight[k] = &bnClone
		}
	}
	return clone
}
