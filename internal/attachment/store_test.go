package attachment

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStorePutListPathDelete(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}

	meta, err := store.Put("task-1", UploadRequest{
		FileName: " ../trace log?.txt ",
		Data:     []byte("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if meta.FileName != "trace log-.txt" {
		t.Fatalf("FileName = %q, want sanitized filename", meta.FileName)
	}

	listed, err := store.List("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(List) = %d, want 1", len(listed))
	}
	if listed[0].ID != meta.ID || listed[0].Path != meta.Path {
		t.Fatalf("listed attachment = %+v, want %+v", listed[0], meta)
	}

	path, err := store.Path("task-1", meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte("hello")) {
		t.Fatalf("blob = %q, want hello", string(data))
	}

	if err := store.Delete("task-1", meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Path("task-1", meta.ID); err == nil {
		t.Fatal("Path after Delete returned nil error, want failure")
	}
}

func TestStoreRejectsOversizeUpload(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("task-1", UploadRequest{FileName: "big.bin", Data: []byte("four")}); err == nil {
		t.Fatal("Put oversize upload returned nil error")
	}
}

func TestStoreRejectsEscapingKey(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("../task", UploadRequest{FileName: "x.txt", Data: []byte("x")}); err == nil {
		t.Fatal("Put with escaping task id returned nil error")
	}
}

func TestStoreDeleteTaskRemovesAll(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Put("task-1", UploadRequest{FileName: "a.txt", Data: []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTask("task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(meta.Path))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("task dir still exists or wrong err: %v", err)
	}
}

func TestStoreConcurrentPutsSameTask(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.Put("task-1", UploadRequest{FileName: "file.txt", Data: []byte{byte('a' + i)}}); err != nil {
				t.Errorf("Put(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	listed, err := store.List("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 8 {
		t.Fatalf("len(List) = %d, want 8", len(listed))
	}
}
