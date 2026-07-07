package config

import "path/filepath"

// TaskSnapshotConfig controls the background git snapshotter that versions
// the tasks dir (see internal/tasksnapshot.Snapshotter), giving recovery a
// `git checkout` path for external deleters that bypass task.Store's
// trash-based soft delete (the case #1576's forensics-only recovery
// couldn't catch).
type TaskSnapshotConfig struct {
	// Enabled toggles the background snapshotter. nil means not configured
	// (defaults to true — safe default, matches RequirePermissions'
	// nil-means-on convention). Set to false to disable entirely.
	Enabled *bool `yaml:"enabled" json:"enabled"`
	// IntervalSeconds is the fixed interval between commit attempts. 0 or
	// negative falls back to DefaultTaskSnapshotInterval (30s).
	IntervalSeconds int `yaml:"interval_seconds" json:"intervalSeconds"`
}

// TaskSnapshotGitDir returns the git-dir sibling used to version the tasks
// dir, kept separate from TasksDir itself so the work-tree never contains a
// nested .git the store or watcher would need to ignore.
func TaskSnapshotGitDir() string {
	return filepath.Join(HomeDir(), "tasks-snapshots.git")
}
