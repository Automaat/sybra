package triage

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestRouteStatus(t *testing.T) {
	tests := []struct {
		name, size, type_, projectType string
		want                           task.Status
	}{
		{"small bug", "small", "bug", "", task.StatusTodo},
		{"small feature", "small", "feature", "", task.StatusTodo},
		{"medium feature", "medium", "feature", "", task.StatusPlanning},
		{"large feature", "large", "feature", "", task.StatusPlanning},
		{"medium feature work", "medium", "feature", "work", task.StatusPlanning},
		{"review any size", "medium", "review", "", task.StatusTodo},
		{"refactor medium", "medium", "refactor", "", task.StatusTodo},
		{"chore large", "large", "chore", "", task.StatusTodo},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RouteStatus(tc.size, tc.type_, tc.projectType)
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRouteMode(t *testing.T) {
	tests := []struct {
		name, llmMode, type_, projectType string
		want                              string
	}{
		{"pet keeps headless", "headless", "feature", "pet", "headless"},
		{"pet keeps interactive", "interactive", "feature", "pet", "interactive"},
		{"work feature forced interactive", "headless", "feature", "work", "interactive"},
		{"work review stays headless", "headless", "review", "work", "headless"},
		{"empty project type", "headless", "feature", "", "headless"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RouteMode(tc.llmMode, tc.type_, tc.projectType)
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}
