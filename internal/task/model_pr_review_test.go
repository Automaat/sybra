package task

import "testing"

func TestIsPRReview(t *testing.T) {
	tests := []struct {
		name string
		task Task
		want bool
	}{
		{"review tag with pr", Task{PRNumber: 7, Tags: []string{TagReview}}, true},
		{"review tag without pr", Task{Tags: []string{TagReview}}, false},
		{"pr without review tag", Task{PRNumber: 7, Tags: []string{"backend"}}, false},
		{"no tags", Task{PRNumber: 7}, false},
		{"another author branch", Task{PRNumber: 7, Tags: []string{TagReview}, Branch: "feat/theirs"}, true},
		{"branch minted for another task", Task{ID: "aa11bb22", PRNumber: 7, Tags: []string{TagReview}, Branch: "fix/adopted-orphan-696bc049"}, false},
		{"branch minted for this task", Task{ID: "696bc049", PRNumber: 7, Tags: []string{TagReview}, Branch: "chore/review-their-work-696bc049"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.task.IsPRReview(); got != tc.want {
				t.Fatalf("IsPRReview() = %v, want %v", got, tc.want)
			}
		})
	}
}
