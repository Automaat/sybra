package task

import (
	"fmt"
	"testing"
)

// seedStore writes n tasks into a fresh store and returns the store plus the
// list of IDs in insertion order. Used by all Store benchmarks to produce a
// deterministic working set without touching the real ~/.synapse directory.
func seedStore(b *testing.B, n int) (store *Store, ids []string) {
	b.Helper()
	s, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	ids = make([]string, n)
	for i := range n {
		t, err := s.Create(
			fmt.Sprintf("benchmark task %04d", i),
			fmt.Sprintf("## Description\nSeeded task body %d.\nLine 2\nLine 3\n", i),
			AgentModeHeadless,
		)
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		ids[i] = t.ID
	}
	return s, ids
}

// BenchmarkStoreList measures full-directory listing latency and allocs
// across a range of store sizes. The hot path is fsutil.ListFiles + Parse
// per file — watcher callbacks trigger this on every list refresh, so
// regressions here directly hit the UI refresh budget.
func BenchmarkStoreList(b *testing.B) {
	sizes := []int{10, 100, 1000}
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			s, _ := seedStore(b, n)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				tasks, err := s.List()
				if err != nil {
					b.Fatalf("List: %v", err)
				}
				if len(tasks) != n {
					b.Fatalf("got %d tasks, want %d", len(tasks), n)
				}
			}
		})
	}
}

// BenchmarkStoreGet measures single-task read latency with a large working
// set present. Simulates UI detail-pane opens against a full store.
func BenchmarkStoreGet(b *testing.B) {
	const n = 1000
	s, ids := seedStore(b, n)
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		_, err := s.Get(ids[i%n])
		if err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

// BenchmarkStoreCreate measures task creation throughput. Each iteration
// produces one new file via the atomic-write path — this is the write
// counterpart to BenchmarkStoreList for budgeting bulk imports.
func BenchmarkStoreCreate(b *testing.B) {
	s, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	body := "## Description\ncreate benchmark body\n"
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		if _, err := s.Create(fmt.Sprintf("create bench %d", i), body, AgentModeHeadless); err != nil {
			b.Fatalf("Create: %v", err)
		}
	}
}

// BenchmarkStoreUpdate measures the Get-modify-Marshal-atomicWrite cycle
// used by every TaskService.UpdateTask call. Covers both the read and write
// hot paths.
func BenchmarkStoreUpdate(b *testing.B) {
	const n = 100
	s, ids := seedStore(b, n)
	status := StatusInProgress
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		if _, err := s.Update(ids[i%n], Update{Status: &status}); err != nil {
			b.Fatalf("Update: %v", err)
		}
	}
}
