package task

// BranchOwnedByOther reports whether some task in tasks, other than
// excludeTaskID, already holds branch within projectID. Ingestion paths that
// derive a task's Branch from an external source (a linked PR's head ref) use
// this before persisting it, so a PR that cross-references more than one
// GitHub issue can never leave two sybra tasks claiming the same branch — the
// second task's worktree would otherwise fail to provision ("branch already
// used by worktree at ...").
func BranchOwnedByOther(tasks []Task, projectID, branch, excludeTaskID string) (ownerID string, ok bool) {
	if branch == "" {
		return "", false
	}
	for i := range tasks {
		t := &tasks[i]
		if t.ID == excludeTaskID {
			continue
		}
		if t.ProjectID == projectID && t.Branch == branch {
			return t.ID, true
		}
	}
	return "", false
}
