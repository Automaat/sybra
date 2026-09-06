package review

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func TestFactoryCIRequiresCurrentHeadAndSuccessfulNamedChecks(t *testing.T) {
	source := initAutoResolveSourceRepo(t)
	if err := os.WriteFile(filepath.Join(source, ".sybra.yaml"), []byte("checks:\n  ci:\n    enabled: true\n    required_checks: [Tests]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".sybra.yaml"}, {"commit", "-m", "CI policy"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = source
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	projects := newExperienceProjectStore(t, t.TempDir())
	proj, err := projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.CloneBare(t.Context(), source, proj.ClonePath); err != nil {
		t.Fatal(err)
	}
	owner := task.Task{ID: "t1", ProjectID: "o/r"}
	pr := github.PullRequest{Repository: "o/r", Number: 42, HeadSHA: "current"}
	for _, scenario := range []string{"current", "stale", "missing", "pending", "failed", "error"} {
		t.Run(scenario, func(t *testing.T) {
			r := &Handler{projects: projects, logger: slog.New(slog.DiscardHandler)}
			r.fetchCIGate = func(_ context.Context, repo string, number int, required []string) (github.CommitGate, error) {
				if repo != "o/r" || number != 42 || !reflect.DeepEqual(required, []string{"Tests"}) {
					t.Fatalf("wrong required-check request: %s#%d %v", repo, number, required)
				}
				gate := github.CommitGate{SHA: "current", Succeeded: []string{"Tests"}}
				switch scenario {
				case "stale":
					gate.SHA = "old"
				case "missing":
					gate.Missing = []string{"Tests"}
				case "pending":
					gate.Pending = []string{"Tests"}
				case "failed":
					gate.Failed = []string{"Tests"}
				case "error":
					return gate, errors.New("GitHub unavailable")
				}
				return gate, nil
			}
			if got := r.factoryCIReady(t.Context(), owner, pr); got != (scenario == "current") {
				t.Fatalf("gate %s = %v", scenario, got)
			}
			// Native auto-merge cannot hold authority after a later push.
			r.supportsAutoMergeFn = func(string, string) (bool, error) {
				t.Fatal("CI project attempted native auto-merge")
				return false, nil
			}
			if result := r.tryArmNativeAutoMerge(owner, github.PRIssue{PR: pr}, ""); result.attempted || result.armed {
				t.Fatalf("native auto-merge escaped exact-head gate: %+v", result)
			}
		})
	}
}
