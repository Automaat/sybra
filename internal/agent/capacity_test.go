package agent

import "testing"

func TestAdmitClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		class    WorkloadClass
		live     map[WorkloadClass]int
		reserved map[WorkloadClass]int
		floors   map[WorkloadClass]int
		max      int
		want     bool
	}{
		{
			name:  "unlimited pool always admits",
			class: ClassSystem,
			live:  map[WorkloadClass]int{ClassSystem: 1000},
			max:   0,
			want:  true,
		},
		{
			name:  "negative max always admits",
			class: ClassSystem,
			live:  map[WorkloadClass]int{ClassSystem: 1000},
			max:   -1,
			want:  true,
		},
		{
			name:  "zero floors: below cap admits",
			class: ClassImplementation,
			live:  map[WorkloadClass]int{ClassImplementation: 2, ClassSystem: 1},
			max:   5,
			want:  true,
		},
		{
			name:  "zero floors: at cap rejects, matches plain liveCount+reserved<max rule",
			class: ClassImplementation,
			live:  map[WorkloadClass]int{ClassImplementation: 2, ClassSystem: 3},
			max:   5,
			want:  false,
		},
		{
			name:   "class below its own floor admits even though pool otherwise looks saturated by another class",
			class:  ClassCompletion,
			live:   map[WorkloadClass]int{ClassSystem: 4},
			floors: map[WorkloadClass]int{ClassCompletion: 1},
			max:    5,
			want:   true,
		},
		{
			name:   "starved class rejected once pool is fully saturated regardless of its own floor",
			class:  ClassCompletion,
			live:   map[WorkloadClass]int{ClassSystem: 5},
			floors: map[WorkloadClass]int{ClassCompletion: 1},
			max:    5,
			want:   false,
		},
		{
			name:   "borrowing: idle reserved class's unused floor is available to another class",
			class:  ClassSystem,
			live:   map[WorkloadClass]int{ClassSystem: 3},
			floors: map[WorkloadClass]int{ClassCompletion: 1, ClassSystem: 1},
			max:    5,
			// used[system]=3 >= floor(1), so must satisfy borrow check:
			// protected = deficit(completion) = 1-0 = 1; total=3; 3+1=4 < 5 -> admit.
			want: true,
		},
		{
			name:   "borrowing denied once it would strand another class's floor",
			class:  ClassSystem,
			live:   map[WorkloadClass]int{ClassSystem: 4},
			floors: map[WorkloadClass]int{ClassCompletion: 1, ClassSystem: 1},
			max:    5,
			// used[system]=4 >= floor(1); protected = deficit(completion) = 1;
			// total=4; 4+1=5, not < 5 -> reject (must leave room for completion's floor).
			want: false,
		},
		{
			name:     "reserved counts toward used same as live",
			class:    ClassImplementation,
			live:     map[WorkloadClass]int{ClassImplementation: 1},
			reserved: map[WorkloadClass]int{ClassImplementation: 1},
			floors:   map[WorkloadClass]int{ClassImplementation: 1},
			max:      2,
			// used[implementation] = 2 >= floor(1); total=2 >= max=2 -> rejected by the
			// total<max gate before the floor/borrow check is even reached.
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := admitClass(tt.class, tt.live, tt.reserved, tt.floors, tt.max)
			if got != tt.want {
				t.Fatalf("admitClass(%q, live=%v, reserved=%v, floors=%v, max=%d) = %v, want %v",
					tt.class, tt.live, tt.reserved, tt.floors, tt.max, got, tt.want)
			}
		})
	}
}

func TestBorrowsSharedCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		class    WorkloadClass
		live     map[WorkloadClass]int
		reserved map[WorkloadClass]int
		floors   map[WorkloadClass]int
		want     bool
	}{
		{
			name:  "no reservations configured: never borrowing",
			class: ClassImplementation,
			live:  map[WorkloadClass]int{ClassImplementation: 5},
			want:  false,
		},
		{
			name:   "below own floor: not borrowing",
			class:  ClassCompletion,
			live:   map[WorkloadClass]int{ClassCompletion: 0},
			floors: map[WorkloadClass]int{ClassCompletion: 1, ClassSystem: 1},
			want:   false,
		},
		{
			name:   "at own floor with reservations configured: borrowing",
			class:  ClassCompletion,
			live:   map[WorkloadClass]int{ClassCompletion: 1},
			floors: map[WorkloadClass]int{ClassCompletion: 1, ClassSystem: 1},
			want:   true,
		},
		{
			name:   "own floor zero but reservations exist elsewhere: borrowing",
			class:  ClassImplementation,
			live:   map[WorkloadClass]int{ClassImplementation: 1},
			floors: map[WorkloadClass]int{ClassSystem: 1},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := borrowsSharedCapacity(tt.class, tt.live, tt.reserved, tt.floors)
			if got != tt.want {
				t.Fatalf("borrowsSharedCapacity(%q, live=%v, reserved=%v, floors=%v) = %v, want %v",
					tt.class, tt.live, tt.reserved, tt.floors, got, tt.want)
			}
		})
	}
}

// TestAdmitClass_ZeroFloorsMatchesPlainRule proves the zero-regression
// contract directly: with class_reservations unset (all floors zero),
// admitClass must agree with the pre-class rule "total used < max" across a
// spread of live/reserved combinations.
func TestAdmitClass_ZeroFloorsMatchesPlainRule(t *testing.T) {
	t.Parallel()

	classes := AllWorkloadClasses()
	for poolMax := range 5 {
		for liveTotal := range 6 {
			for reservedTotal := range 6 {
				live := map[WorkloadClass]int{classes[0]: liveTotal}
				reserved := map[WorkloadClass]int{classes[0]: reservedTotal}
				total := liveTotal + reservedTotal
				want := poolMax <= 0 || total < poolMax
				for _, class := range classes {
					got := admitClass(class, live, reserved, nil, poolMax)
					if got != want {
						t.Fatalf("admitClass(%q, live=%d, reserved=%d, max=%d) = %v, want %v (plain rule)",
							class, liveTotal, reservedTotal, poolMax, got, want)
					}
				}
			}
		}
	}
}
