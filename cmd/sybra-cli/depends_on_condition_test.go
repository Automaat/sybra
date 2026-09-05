package main

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestUpdateDependsOnConditionSetsField(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "depcond test")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, out = runCLI(t, "--json", "update", created.ID,
		"--depends-on-condition", "o/r#1=note:confirm scope covers permutation contract tests",
	)
	if code != 0 {
		t.Fatalf("update exit %d: %s", code, out)
	}

	var updated task.Task
	mustUnmarshal(t, out, &updated)
	if len(updated.DependsOnConditions) != 1 {
		t.Fatalf("DependsOnConditions = %+v, want 1 entry", updated.DependsOnConditions)
	}
	got := updated.DependsOnConditions[0]
	want := task.DepCondition{Ref: "o/r#1", Kind: task.DepConditionKindNote, Value: "confirm scope covers permutation contract tests"}
	if got != want {
		t.Fatalf("condition = %+v, want %+v", got, want)
	}
}

func TestUpdateDependsOnConditionWarnsWhenRefNotDependedOn(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "inert condition test")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, _, stderr := runCLIWithStderr(t, "--json", "update", created.ID,
		"--depends-on-condition", "o/r#1=label:scope-confirmed",
	)
	if code != 0 {
		t.Fatalf("update exit %d", code)
	}
	if !strings.Contains(stderr, "inert") || !strings.Contains(stderr, "o/r#1") {
		t.Fatalf("stderr = %q, want an inert-ref warning naming o/r#1", stderr)
	}
}

func TestUpdateDependsOnConditionRejectsUnknownKind(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "bad kind test")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, out = runCLI(t, "--json", "update", created.ID,
		"--depends-on-condition", "o/r#1=pr-merged:x",
	)
	if code == 0 {
		t.Fatalf("update exit 0, want failure for unknown kind: %s", out)
	}
}

func TestUpdateDependsOnConditionRejectsMalformedInput(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "malformed test")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	for _, raw := range []string{
		"missing-equals-sign",
		"o/r#1=missing-colon",
		"=note:no-ref",
		"o/r#1=note:",
	} {
		code, out = runCLI(t, "--json", "update", created.ID, "--depends-on-condition", raw)
		if code == 0 {
			t.Fatalf("--depends-on-condition %q: exit 0, want failure: %s", raw, out)
		}
	}
}

func TestUpdateDependsOnConditionRejectsDuplicateRef(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "dup ref test")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, out = runCLI(t, "--json", "update", created.ID,
		"--depends-on-condition", "o/r#1=note:a",
		"--depends-on-condition", "o/r#1=label:b",
	)
	if code == 0 {
		t.Fatalf("update exit 0, want failure for duplicate ref: %s", out)
	}
}

func TestCreateDependsOnCondition(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "create with condition",
		"--depends-on-condition", "o/r#1=note:confirm scope",
	)
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)
	if len(created.DependsOnConditions) != 1 || created.DependsOnConditions[0].Ref != "o/r#1" {
		t.Fatalf("DependsOnConditions = %+v, want one entry for o/r#1", created.DependsOnConditions)
	}
}
