package task

import "testing"

func TestValidateStatus_Valid(t *testing.T) {
	t.Parallel()
	for _, s := range AllStatuses() {
		t.Run(string(s), func(t *testing.T) {
			t.Parallel()
			got, err := ValidateStatus(string(s))
			if err != nil {
				t.Fatalf("ValidateStatus(%q): %v", s, err)
			}
			if got != s {
				t.Errorf("got %q, want %q", got, s)
			}
		})
	}
}

func TestValidateStatus_Invalid(t *testing.T) {
	t.Parallel()
	if _, err := ValidateStatus("invalid-status"); err == nil {
		t.Fatal("expected error for invalid status")
	}
}

func TestAllStatuses(t *testing.T) {
	t.Parallel()
	statuses := AllStatuses()
	if len(statuses) != 8 {
		t.Errorf("got %d statuses, want 8", len(statuses))
	}
	seen := make(map[Status]bool)
	for _, s := range statuses {
		if seen[s] {
			t.Errorf("duplicate status %q", s)
		}
		seen[s] = true
	}
}

func TestValidateTaskType_Valid(t *testing.T) {
	t.Parallel()
	for _, tt := range AllTaskTypes() {
		t.Run(string(tt), func(t *testing.T) {
			t.Parallel()
			got, err := ValidateTaskType(string(tt))
			if err != nil {
				t.Fatalf("ValidateTaskType(%q): %v", tt, err)
			}
			if got != tt {
				t.Errorf("got %q, want %q", got, tt)
			}
		})
	}
}

func TestValidateTaskType_Invalid(t *testing.T) {
	t.Parallel()
	if _, err := ValidateTaskType("unknown"); err == nil {
		t.Fatal("expected error for invalid task type")
	}
}

func TestAllTaskTypes(t *testing.T) {
	t.Parallel()
	types := AllTaskTypes()
	if len(types) != 3 {
		t.Errorf("got %d types, want 3", len(types))
	}
}

func TestTask_DirName_WithSlug(t *testing.T) {
	t.Parallel()
	task := Task{ID: "a1b2c3d4", Slug: "my-task"}
	got := task.DirName()
	want := "my-task-a1b2c3d4"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTask_DirName_NoSlug(t *testing.T) {
	t.Parallel()
	task := Task{ID: "a1b2c3d4"}
	got := task.DirName()
	if got != "a1b2c3d4" {
		t.Errorf("got %q, want %q", got, "a1b2c3d4")
	}
}
