package triage

import (
	"reflect"
	"slices"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"canonical", []string{"backend", "small", "bug"}, []string{"backend", "small", "bug"}, false},
		{"aliases", []string{"be", "fe", "bugfix"}, []string{"backend", "frontend", "bug"}, false},
		{"unknown dropped", []string{"backend", "mystery"}, []string{"backend"}, true},
		{"dedupe", []string{"backend", "backend", "BE"}, []string{"backend"}, false},
		{"whitespace+case", []string{" Backend ", "SMALL"}, []string{"backend", "small"}, false},
		{"empty", []string{}, []string{}, false},
		{"escape-hatch kept", []string{"backend", "noplan", "nocritic", "trivial", "skip-testing"}, []string{"backend", "noplan", "nocritic", "trivial", "skip-testing"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTags(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateVerdict(t *testing.T) {
	tests := []struct {
		name    string
		v       Verdict
		wantErr bool
	}{
		{"valid", Verdict{Title: "t", Size: "small", Type: "bug", Mode: "headless", Tags: []string{"backend"}}, false},
		{"empty title", Verdict{Title: " ", Size: "small", Type: "bug", Mode: "headless"}, true},
		{"bad size", Verdict{Title: "t", Size: "huge", Type: "bug", Mode: "headless"}, true},
		{"bad type", Verdict{Title: "t", Size: "small", Type: "nonsense", Mode: "headless"}, true},
		{"bad mode", Verdict{Title: "t", Size: "small", Type: "bug", Mode: "auto"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.v
			err := ValidateVerdict(&v)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateVerdictNoplanFloor(t *testing.T) {
	tests := []struct {
		name       string
		size       string
		typ        string
		wantNoplan bool
	}{
		{"small chore keeps", "small", "chore", true},
		{"small bug keeps", "small", "bug", true},
		{"small refactor keeps", "small", "refactor", true},
		{"small feature strips", "small", "feature", false},
		{"medium chore strips", "medium", "chore", false},
		{"large bug strips", "large", "bug", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Verdict{
				Title: "t", Size: tc.size, Type: tc.typ, Mode: "headless",
				Tags: []string{"backend", tc.size, tc.typ, "noplan"},
			}
			if err := ValidateVerdict(&v); err != nil {
				t.Fatalf("ValidateVerdict: %v", err)
			}
			gotNoplan := slices.Contains(v.Tags, "noplan")
			if gotNoplan != tc.wantNoplan {
				t.Errorf("noplan present = %v, want %v (tags %v)", gotNoplan, tc.wantNoplan, v.Tags)
			}
		})
	}
}

func TestValidateVerdictTrivialFloor(t *testing.T) {
	tests := []struct {
		name        string
		size        string
		typ         string
		wantTrivial bool
	}{
		{"small chore keeps", "small", "chore", true},
		{"small bug strips", "small", "bug", false},
		{"small refactor keeps", "small", "refactor", true},
		{"small feature strips", "small", "feature", false},
		{"medium chore strips", "medium", "chore", false},
		{"large bug strips", "large", "bug", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Verdict{
				Title: "t", Size: tc.size, Type: tc.typ, Mode: "headless",
				Tags: []string{"backend", tc.size, tc.typ, "trivial"},
			}
			if err := ValidateVerdict(&v); err != nil {
				t.Fatalf("ValidateVerdict: %v", err)
			}
			gotTrivial := slices.Contains(v.Tags, "trivial")
			if gotTrivial != tc.wantTrivial {
				t.Errorf("trivial present = %v, want %v (tags %v)", gotTrivial, tc.wantTrivial, v.Tags)
			}
		})
	}
}

// TestValidateVerdictTrivialBugFloorIndependentOfNoplan proves the stricter
// trivial-vs-bug floor doesn't leak into noplan: a small bug fix may still
// skip planning but must not also skip review + testing unattended.
func TestValidateVerdictTrivialBugFloorIndependentOfNoplan(t *testing.T) {
	v := Verdict{
		Title: "t", Size: "small", Type: "bug", Mode: "headless",
		Tags: []string{"backend", "small", "bug", "noplan", "trivial"},
	}
	if err := ValidateVerdict(&v); err != nil {
		t.Fatalf("ValidateVerdict: %v", err)
	}
	if !slices.Contains(v.Tags, "noplan") {
		t.Errorf("expected noplan to survive on small bug fix, got %v", v.Tags)
	}
	if slices.Contains(v.Tags, "trivial") {
		t.Errorf("expected trivial to be stripped on small bug fix, got %v", v.Tags)
	}
}

func TestValidateVerdictStripsClassifierEmittedSkipTesting(t *testing.T) {
	v := Verdict{
		Title: "t", Size: "small", Type: "chore", Mode: "headless",
		Tags: []string{"backend", "small", "chore", "skip-testing"},
	}
	if err := ValidateVerdict(&v); err != nil {
		t.Fatalf("ValidateVerdict: %v", err)
	}
	if slices.Contains(v.Tags, "skip-testing") {
		t.Errorf("expected classifier-emitted skip-testing to be stripped, got %v", v.Tags)
	}
}
