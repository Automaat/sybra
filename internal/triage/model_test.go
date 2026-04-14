package triage

import (
	"reflect"
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
