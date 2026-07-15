package harnessevolution

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed testdata/regression/*.json
var defaultCorpusFS embed.FS

type CorpusCase struct {
	ID                    string       `json:"id"`
	Category              string       `json:"category"`
	FailureKind           string       `json:"failureKind"`
	WantKind              ProposalKind `json:"wantKind"`
	RequiresHumanApproval bool         `json:"requiresHumanApproval"`
}

func LoadDefaultCorpus() ([]CorpusCase, error) {
	return loadCorpusFS(defaultCorpusFS, "testdata/regression")
}

func LoadCorpusDir(dir string) ([]CorpusCase, error) {
	if dir == "" {
		return LoadDefaultCorpus()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []CorpusCase
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		cases, err := parseCorpus(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		out = append(out, cases...)
	}
	return out, nil
}

func loadCorpusFS(fsys fs.FS, dir string) ([]CorpusCase, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []CorpusCase
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(fsys, filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		cases, err := parseCorpus(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		out = append(out, cases...)
	}
	return out, nil
}

func parseCorpus(data []byte) ([]CorpusCase, error) {
	var cases []CorpusCase
	if err := json.Unmarshal(data, &cases); err == nil {
		return cases, validateCorpus(cases)
	}
	var single CorpusCase
	if err := json.Unmarshal(data, &single); err != nil {
		return nil, fmt.Errorf("parse corpus: %w", err)
	}
	return []CorpusCase{single}, validateCorpus([]CorpusCase{single})
}

func validateCorpus(cases []CorpusCase) error {
	seen := map[string]bool{}
	for i := range cases {
		c := cases[i]
		if c.ID == "" {
			return fmt.Errorf("case %d has empty id", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case %q", c.ID)
		}
		seen[c.ID] = true
		if c.WantKind == "" {
			return fmt.Errorf("case %q has empty wantKind", c.ID)
		}
	}
	return nil
}
