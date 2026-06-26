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
	if len(statuses) != 13 {
		t.Errorf("got %d statuses, want 13", len(statuses))
	}
	seen := make(map[Status]bool)
	for _, s := range statuses {
		if seen[s] {
			t.Errorf("duplicate status %q", s)
		}
		seen[s] = true
	}
	if !seen[StatusBlocked] {
		t.Errorf("StatusBlocked missing from AllStatuses()")
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
	if len(types) != 5 {
		t.Errorf("got %d types, want 5", len(types))
	}
}

func TestValidateAgentMode_Valid(t *testing.T) {
	t.Parallel()
	for _, m := range AllAgentModes() {
		t.Run(m, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateAgentMode(m)
			if err != nil {
				t.Fatalf("ValidateAgentMode(%q): %v", m, err)
			}
			if got != m {
				t.Errorf("got %q, want %q", got, m)
			}
		})
	}
}

func TestValidateAgentMode_Invalid(t *testing.T) {
	t.Parallel()
	cases := []string{"", "supervised", "Headless", "auto", " interactive"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateAgentMode(c); err == nil {
				t.Fatalf("expected error for %q", c)
			}
		})
	}
}

func TestAllAgentModes(t *testing.T) {
	t.Parallel()
	modes := AllAgentModes()
	if len(modes) != 2 {
		t.Errorf("got %d modes, want 2", len(modes))
	}
	seen := make(map[string]bool)
	for _, m := range modes {
		if seen[m] {
			t.Errorf("duplicate mode %q", m)
		}
		seen[m] = true
	}
}

func TestValidateReasoningEffort(t *testing.T) {
	t.Parallel()

	t.Run("empty_ok", func(t *testing.T) {
		t.Parallel()
		got, err := ValidateReasoningEffort("")
		if err != nil {
			t.Fatalf("ValidateReasoningEffort(%q): %v", "", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	valid := []string{"low", "medium", "high", "xhigh"}
	for _, v := range valid {
		t.Run(v+"_ok", func(t *testing.T) {
			t.Parallel()
			got, err := ValidateReasoningEffort(v)
			if err != nil {
				t.Fatalf("ValidateReasoningEffort(%q): %v", v, err)
			}
			if got != v {
				t.Errorf("got %q, want %q", got, v)
			}
		})
	}

	invalid := []string{"max", "ultra", "XHIGH", "extreme", "bogus", " high"}
	for _, v := range invalid {
		t.Run("invalid_"+v, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateReasoningEffort(v); err == nil {
				t.Fatalf("expected error for %q, got nil", v)
			}
		})
	}
}

func TestAllReasoningEfforts(t *testing.T) {
	t.Parallel()
	efforts := AllReasoningEfforts()
	if len(efforts) != 4 {
		t.Errorf("got %d efforts, want 4", len(efforts))
	}
}

func TestValidateAgentProvider(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"", "claude", "codex", "copilot"} {
		t.Run("valid_"+v, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateAgentProvider(v)
			if err != nil {
				t.Fatalf("ValidateAgentProvider(%q): %v", v, err)
			}
			if got != v {
				t.Errorf("got %q, want %q", got, v)
			}
		})
	}

	for _, v := range []string{"gpt", "Claude", " codex "} {
		t.Run("invalid_"+v, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateAgentProvider(v); err == nil {
				t.Fatalf("expected error for %q, got nil", v)
			}
		})
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
