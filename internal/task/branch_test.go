package task

import "testing"

func TestBranchOwnedByOther(t *testing.T) {
	tasks := []Task{
		{ID: "a", ProjectID: "owner/repo", Branch: "fix/foo-abcd1234"},
		{ID: "b", ProjectID: "owner/repo", Branch: "fix/bar-ef567890"},
		{ID: "c", ProjectID: "other/repo", Branch: "fix/foo-abcd1234"},
	}

	tests := []struct {
		name          string
		projectID     string
		branch        string
		excludeTaskID string
		wantOwner     string
		wantOK        bool
	}{
		{
			name:      "empty branch never collides",
			projectID: "owner/repo",
			branch:    "",
			wantOK:    false,
		},
		{
			name:      "no owner in project",
			projectID: "owner/repo",
			branch:    "fix/unclaimed-00000000",
			wantOK:    false,
		},
		{
			name:      "different project with same branch string does not collide",
			projectID: "unrelated/repo",
			branch:    "fix/foo-abcd1234",
			wantOK:    false,
		},
		{
			name:      "collides with a different task in the same project",
			projectID: "owner/repo",
			branch:    "fix/foo-abcd1234",
			wantOwner: "a",
			wantOK:    true,
		},
		{
			name:          "excluding the owning task itself is not a collision",
			projectID:     "owner/repo",
			branch:        "fix/foo-abcd1234",
			excludeTaskID: "a",
			wantOK:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, ok := BranchOwnedByOther(tasks, tt.projectID, tt.branch, tt.excludeTaskID)
			if ok != tt.wantOK || owner != tt.wantOwner {
				t.Fatalf("BranchOwnedByOther(%q, %q, %q) = (%q, %v), want (%q, %v)",
					tt.projectID, tt.branch, tt.excludeTaskID, owner, ok, tt.wantOwner, tt.wantOK)
			}
		})
	}
}
