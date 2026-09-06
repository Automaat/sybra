package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPartitionIsCompleteDisjointAndStable(t *testing.T) {
	var names []string
	for _, prefix := range []string{"Test", "Example", "Fuzz"} {
		for i := range 200 {
			names = append(names, fmt.Sprintf("%sCase%d", prefix, i))
		}
	}
	input := strings.Join(names, "\n") + "\nBenchmarkNotRun\nok example/package 0.01s\nfixture log\n"
	seen := make(map[string]bool)
	for index := range 4 {
		got, err := selectTests(input, index, 4)
		if err != nil {
			t.Fatal(err)
		}
		pattern := regexp.MustCompile(runPattern(got))
		for _, name := range got {
			if seen[name] || !pattern.MatchString(name) || pattern.MatchString(name+"Extra") {
				t.Fatalf("invalid assignment/match for %s", name)
			}
			seen[name] = true
		}
		reversed := slices.Clone(names)
		slices.Reverse(reversed)
		again, err := selectTests(strings.Join(reversed, "\n"), index, 4)
		if err != nil || !slices.Equal(got, again) {
			t.Fatalf("discovery order changed shard: %v", err)
		}
	}
	if len(seen) != len(names) {
		t.Fatalf("assigned %d of %d tests", len(seen), len(names))
	}
}

func TestSelectionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		index, total int
	}{
		{"empty", "ok package 0.01s", 0, 4},
		{"duplicate", "TestOne\nTestOne", 0, 1},
		{"negative", "TestOne", -1, 4},
		{"outside", "TestOne", 4, 4},
		{"zero", "TestOne", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := selectTests(tc.output, tc.index, tc.total); err == nil {
				t.Fatal("invalid discovery or shard was accepted")
			}
		})
	}
}

func TestRunPatternQuotesNames(t *testing.T) {
	pattern := regexp.MustCompile(runPattern([]string{"TestLiteral.Dot", "ExampleUnicode_世界", "FuzzInput"}))
	if !pattern.MatchString("TestLiteral.Dot") || pattern.MatchString("TestLiteralXDot") || pattern.MatchString("FuzzInputExtra") {
		t.Fatal("selection pattern broadened the discovered names")
	}
}

func TestWorkflowKeepsCompleteFailClosedGate(t *testing.T) {
	content, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Name     string   `yaml:"name"`
			If       string   `yaml:"if"`
			Needs    []string `yaml:"needs"`
			Strategy struct {
				Matrix struct {
					Shard []int `yaml:"shard"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(content, &workflow); err != nil {
		t.Fatal(err)
	}
	shards := workflow.Jobs["test-go-e2e-shards"]
	if !slices.Equal(shards.Strategy.Matrix.Shard, []int{0, 1, 2, 3}) || shards.If != "" {
		t.Fatal("the application matrix must run all four shards unconditionally")
	}
	if !strings.Contains(shards.Steps[len(shards.Steps)-1].Run, "-total 4") {
		t.Fatal("runner partition count must match the matrix")
	}
	gate := workflow.Jobs["test-go-e2e"]
	if gate.Name != "Go E2E Tests" || gate.If != "${{ always() }}" ||
		!slices.Equal(gate.Needs, []string{"test-go-e2e-shards", "test-go-e2e-packages"}) {
		t.Fatal("protected gate must depend unconditionally on every E2E job")
	}
	if workflow.Jobs["test-go"].Name != "Go Tests" || workflow.Jobs["test-go-e2e-packages"].If != "" {
		t.Fatal("unit gate or child-package coverage changed")
	}
	children := workflow.Jobs["test-go-e2e-packages"].Steps
	if !strings.Contains(children[len(children)-1].Run, "go list -race -tags e2e ./internal/sybra/...") {
		t.Fatal("child package discovery must use the same build constraints as execution")
	}
	for _, shardResult := range []string{"success", "failure", "cancelled", "skipped", ""} {
		for _, packageResult := range []string{"success", "failure", "cancelled", "skipped", ""} {
			cmd := exec.Command("bash", "-e", "-c", gate.Steps[0].Run)
			cmd.Env = append(os.Environ(), "SHARDS_RESULT="+shardResult, "PACKAGES_RESULT="+packageResult)
			passed := cmd.Run() == nil
			if passed != (shardResult == "success" && packageResult == "success") {
				t.Fatalf("incorrect gate for %q / %q", shardResult, packageResult)
			}
		}
	}
}
