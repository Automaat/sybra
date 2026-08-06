package task

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
)

func TestStoreUpdateMap_LockTimeoutIsBoundedAndTyped(t *testing.T) {
	t.Parallel()

	store, err := NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("lock timeout", "", AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}

	unlock, err := fsutil.LockFile(created.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	start := time.Now()
	_, err = store.UpdateMap(created.ID, map[string]any{"title": "blocked"})
	if !errors.Is(err, fsutil.ErrLockTimeout) {
		t.Fatalf("UpdateMap error = %v, want ErrLockTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > taskLockTimeout+500*time.Millisecond {
		t.Fatalf("UpdateMap took %s, want bounded wait near %s", elapsed, taskLockTimeout)
	}

	lockPath := created.FilePath + ".lock"
	if got := err.Error(); !strings.Contains(got, lockPath) {
		t.Fatalf("error %q missing lock path %q", got, lockPath)
	}
	if got := err.Error(); !strings.Contains(got, strconv.Itoa(os.Getpid())) {
		t.Fatalf("error %q missing holder pid %d", got, os.Getpid())
	}
}

func TestStoreCreateFull_LockTimeoutIsTyped(t *testing.T) {
	t.Parallel()

	store, err := NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	store.newTaskID = func() string { return "contended" }

	createLockPath := filepath.Join(filepath.Dir(store.dir), ".task-create-locks", "contended")
	if err := os.MkdirAll(filepath.Dir(createLockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.LockFile(createLockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, err = store.CreateFull("blocked create", "", AgentModeHeadless, Update{})
	if !errors.Is(err, fsutil.ErrLockTimeout) {
		t.Fatalf("CreateFull error = %v, want ErrLockTimeout", err)
	}
	if got := err.Error(); !strings.Contains(got, createLockPath+".lock") {
		t.Fatalf("error %q missing create lock path", got)
	}
}
