package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordingAddImport(seen *[]string) func(string, string) string {
	return func(module, name string) string {
		*seen = append(*seen, module+"."+name)
		return name
	}
}

func TestMapType(t *testing.T) {
	bindingImports := map[string]string{
		"task$0":     "internal/task/models",
		"artifact$0": "internal/artifact/models",
	}
	tests := []struct {
		name string
		in   string
		want string
		imp  string
	}{
		{"primitive", "string", "string", ""},
		{"void", "void", "void", ""},
		{"namespaced", "task$0.Task", "Task", "internal/task/models.Task"},
		{"array", "task$0.Task[]", "Array<Task>", "internal/task/models.Task"},
		{"array_other_pkg", "artifact$0.ProgressEntry[]", "Array<ProgressEntry>", "internal/artifact/models.ProgressEntry"},
		{"strip_null", "task$0.Task | null", "Task", "internal/task/models.Task"},
		{"nullable_union_array", "(task$0.Task | null)[]", "Array<Task>", "internal/task/models.Task"},
		{"nullable_single_paren", "(task$0.Task | null)", "Task", "internal/task/models.Task"},
		{"models_alias", "$models.TamperReportDTO", "TamperReportDTO", "internal/sybra/models.TamperReportDTO"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen []string
			got := mapType(tt.in, bindingImports, recordingAddImport(&seen))
			if got != tt.want {
				t.Fatalf("mapType(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if tt.imp != "" && (len(seen) != 1 || seen[0] != tt.imp) {
				t.Fatalf("mapType(%q) imports = %v, want [%q]", tt.in, seen, tt.imp)
			}
			if tt.imp == "" && len(seen) != 0 {
				t.Fatalf("mapType(%q) unexpected imports %v", tt.in, seen)
			}
		})
	}
}

func TestSplitTopLevel(t *testing.T) {
	got := splitTopLevel("title: string, init: Map<string, Update>, n: number")
	want := []string{"title: string", "init: Map<string, Update>", "n: number"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("splitTopLevel = %v, want %v", got, want)
	}
}

func TestFillAPITS(t *testing.T) {
	src := strings.Join([]string{
		"import * as TaskSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/taskservice.js'",
		"import * as http from './api-http.js'",
		"",
		"// TaskService",
		"export const GetTask = pick(TaskSvc.GetTask, http.GetTask)",
	}, "\n")
	services := []service{{name: "TaskService", methods: []string{"GetTask", "DeleteTask"}}}

	out, added, err := fillAPITS(src, services, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "DeleteTask" {
		t.Fatalf("added = %v, want [DeleteTask]", added)
	}
	if !strings.Contains(out, "export const DeleteTask = pick(TaskSvc.DeleteTask, http.DeleteTask)") {
		t.Fatalf("missing DeleteTask shim:\n%s", out)
	}
	if !strings.Contains(out, "export const GetTask = pick(TaskSvc.GetTask, http.GetTask)") {
		t.Fatalf("clobbered existing GetTask:\n%s", out)
	}

	out2, added2, err := fillAPITS(out, services, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(added2) != 0 || out2 != out {
		t.Fatalf("second pass not a no-op: added=%v", added2)
	}
}

func TestFillAPIHTTP(t *testing.T) {
	bindingDir := t.TempDir()
	binding := strings.Join([]string{
		`import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";`,
		`import * as task$0 from "../task/models.js";`,
		`export function DeleteTask(id: string): $CancellablePromise<void> {`,
		`    return $Call.ByID(1, id);`,
		`}`,
		`export function CreateTaskWithInit(title: string, init: task$0.Update): $CancellablePromise<task$0.Task> {`,
		`    return $Call.ByID(2, title, init);`,
		`}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(bindingDir, "taskservice.ts"), []byte(binding), 0o644); err != nil {
		t.Fatal(err)
	}

	src := strings.Join([]string{
		"import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'",
		"",
		"async function call<T>(): Promise<T> { return undefined as T }",
		"",
		"// TaskService",
		"export function GetTask(arg1: string): Promise<Task> { return call('TaskService', 'GetTask', arg1) }",
	}, "\n")
	services := []service{{name: "TaskService", methods: []string{"GetTask", "DeleteTask", "CreateTaskWithInit"}}}

	out, added, err := fillAPIHTTP(src, services, bindingDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added = %v, want 2", added)
	}
	wantLines := []string{
		"export function DeleteTask(arg1: string): Promise<void> { return call('TaskService', 'DeleteTask', arg1) }",
		"export function CreateTaskWithInit(arg1: string, arg2: Update): Promise<Task> { return call('TaskService', 'CreateTaskWithInit', arg1, arg2) }",
		"import type { Task, Update } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'",
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Fatalf("missing line:\n%s\n--- output ---\n%s", w, out)
		}
	}
	if strings.Count(out, "internal/task/models.js'") != 1 {
		t.Fatalf("duplicate task/models import:\n%s", out)
	}

	out2, added2, err := fillAPIHTTP(out, services, bindingDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(added2) != 0 || out2 != out {
		t.Fatalf("second pass not a no-op: added=%v", added2)
	}
}

func TestParseServices(t *testing.T) {
	dir := t.TempDir()
	src := strings.Join([]string{
		"package sybra",
		"func x() {",
		`	"TaskService": httpapi.NewService(a.taskSvc,`,
		`		"GetTask",`,
		`		"DeleteTask",`,
		`	),`,
		`	"App": httpapi.NewService(a,`,
		`		"StartAgent",`,
		`	),`,
		"}",
	}, "\n")
	path := filepath.Join(dir, "services.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseServices(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].name != "TaskService" || len(got[0].methods) != 2 || got[1].name != "App" {
		t.Fatalf("parseServices = %+v", got)
	}
}

func TestUnresolvableMethods(t *testing.T) {
	t.Parallel()
	bindingDir := t.TempDir()
	binding := strings.Join([]string{
		`import { CancellablePromise as $CancellablePromise } from "@wailsio/runtime";`,
		`export function GetTask(id: string): $CancellablePromise<string> {`,
		`    return $Call.ByID(1, id);`,
		`}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(bindingDir, "taskservice.ts"), []byte(binding), 0o644); err != nil {
		t.Fatal(err)
	}
	services := []service{
		{name: "TaskService", methods: []string{"GetTask", "NewlyAdded"}},
		{name: "QueueService", methods: []string{"SnapshotDepth"}},
	}

	skip, reasons := unresolvableMethods(services, bindingDir)
	if skip["GetTask"] {
		t.Error("GetTask has a binding signature and must not be skipped")
	}
	if !skip["NewlyAdded"] {
		t.Error("NewlyAdded has no binding signature and must be skipped")
	}
	if !skip["SnapshotDepth"] {
		t.Error("SnapshotDepth's service has no binding file and must be skipped")
	}
	if len(reasons) != 2 {
		t.Fatalf("reasons = %v, want 2", reasons)
	}
}

func TestFillAPIHTTP_SkipsMissingBinding(t *testing.T) {
	t.Parallel()
	bindingDir := t.TempDir()
	src := strings.Join([]string{
		"async function call<T>(): Promise<T> { return undefined as T }",
		"",
		"// QueueService",
		"export function Existing(): Promise<void> { return call('QueueService', 'Existing') }",
	}, "\n")
	services := []service{{name: "QueueService", methods: []string{"Existing", "SnapshotDepth"}}}
	skip := map[string]bool{"SnapshotDepth": true}

	out, added, err := fillAPIHTTP(src, services, bindingDir, skip)
	if err != nil {
		t.Fatalf("must not error on a skipped missing-binding method: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want none (SnapshotDepth skipped, Existing already present)", added)
	}
	if strings.Contains(out, "SnapshotDepth") {
		t.Fatalf("skipped method must not be emitted:\n%s", out)
	}
}
