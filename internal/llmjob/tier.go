package llmjob

import (
	"maps"

	"github.com/Automaat/sybra/internal/modeltier"
)

// Tier describes the cost/capability class for a short structured LLM job.
type Tier int

const (
	// SuperCheap is the haiku/mini class for small structured jobs.
	SuperCheap Tier = iota
	// Cheap is the sonnet-class default for ordinary coding/reasoning jobs.
	Cheap
	// Standard is kept as a compatibility alias for Cheap.
	Standard
	// Deep is the opus/deep class for high-scrutiny jobs.
	Deep
)

var tierModels = map[Tier]map[string]string{
	SuperCheap: modeltier.Models(modeltier.SuperCheap),
	Cheap:      modeltier.Models(modeltier.Cheap),
	Standard:   modeltier.Models(modeltier.Cheap),
	Deep:       modeltier.Models(modeltier.Expensive),
}

func modelsFor(t Tier) map[string]string {
	row, ok := tierModels[t]
	if !ok {
		row = tierModels[Standard]
	}
	return cloneModels(row)
}

func cloneModels(row map[string]string) map[string]string {
	out := make(map[string]string, len(row))
	maps.Copy(out, row)
	return out
}
