package procstat

import (
	"reflect"
	"testing"
	"time"
)

func TestSampleSortsMarksAndTruncates(t *testing.T) {
	t.Parallel()

	restore := stubSampler(t, []Process{
		{PID: 11, PGID: 101, Name: "worker", CPUPercent: 10, MemPercent: 2},
		{PID: 12, PGID: 102, Name: "browser", CPUPercent: 40, MemPercent: 12},
		{PID: 13, PGID: 101, Name: "child", CPUPercent: 20, MemPercent: 30},
	}, true)
	defer restore()

	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	prevNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = prevNow }()

	got := Sample(2, func(pid, pgid int) bool { return pid == 11 || pgid == 101 })

	if !got.Available {
		t.Fatal("Available = false, want true")
	}
	if got.SampledAt != now {
		t.Fatalf("SampledAt = %s, want %s", got.SampledAt, now)
	}

	wantCPU := []Process{
		{PID: 12, PGID: 102, Name: "browser", CPUPercent: 40, MemPercent: 12, Owned: false},
		{PID: 13, PGID: 101, Name: "child", CPUPercent: 20, MemPercent: 30, Owned: true},
	}
	if !reflect.DeepEqual(got.TopCPU, wantCPU) {
		t.Fatalf("TopCPU = %#v, want %#v", got.TopCPU, wantCPU)
	}

	wantMem := []Process{
		{PID: 13, PGID: 101, Name: "child", CPUPercent: 20, MemPercent: 30, Owned: true},
		{PID: 12, PGID: 102, Name: "browser", CPUPercent: 40, MemPercent: 12, Owned: false},
	}
	if !reflect.DeepEqual(got.TopMem, wantMem) {
		t.Fatalf("TopMem = %#v, want %#v", got.TopMem, wantMem)
	}
}

func TestSampleNilOwnedAndUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("nil owned", func(t *testing.T) {
		restore := stubSampler(t, []Process{{PID: 21, PGID: 201, Name: "one", CPUPercent: 1, MemPercent: 2}}, true)
		defer restore()

		got := Sample(5, nil)
		if len(got.TopCPU) != 1 {
			t.Fatalf("len(TopCPU) = %d, want 1", len(got.TopCPU))
		}
		if got.TopCPU[0].Owned {
			t.Fatal("Owned = true, want false")
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		restore := stubSampler(t, nil, false)
		defer restore()

		got := Sample(5, nil)
		if got.Available {
			t.Fatal("Available = true, want false")
		}
		if len(got.TopCPU) != 0 || len(got.TopMem) != 0 {
			t.Fatalf("expected empty slices, got TopCPU=%d TopMem=%d", len(got.TopCPU), len(got.TopMem))
		}
	})
}

func TestParseProcessOutputSkipsMalformedRows(t *testing.T) {
	t.Parallel()

	got := parseProcessOutput(`
  101  100  12.5  3.0 sybra-server
  nope  100  2.0  1.0 bad-pid
  102  100 nope  1.0 bad-cpu
  103  100  3.5  2.5 browser helper
  104
`)

	want := []Process{
		{PID: 101, PGID: 100, CPUPercent: 12.5, MemPercent: 3.0, Name: "sybra-server"},
		{PID: 103, PGID: 100, CPUPercent: 3.5, MemPercent: 2.5, Name: "browser helper"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcessOutput = %#v, want %#v", got, want)
	}
}

func stubSampler(t *testing.T, processes []Process, available bool) func() {
	t.Helper()

	prev := readProcessesFn
	readProcessesFn = func() ([]Process, bool) {
		return append([]Process(nil), processes...), available
	}
	return func() {
		readProcessesFn = prev
	}
}
