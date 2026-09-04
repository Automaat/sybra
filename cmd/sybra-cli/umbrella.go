package main

import (
	"encoding/json"
	"flag"
	"fmt"

	"github.com/Automaat/sybra/internal/config"
)

// cmdUmbrella expands a GitHub umbrella issue into a gated task DAG: one
// `umbrella` tracker task plus one `blocked` child per sub-issue, with
// dependency edges extracted by an LLM planner. Re-running is idempotent —
// only sub-issues without an existing task are materialized.
func cmdUmbrella(cfg *config.Config, api *apiClient, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("umbrella", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "umbrella issue URL (or pass as first argument)")
	model := fs.String("model", "", "planner model (default: claude default)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "parse flags: %v", err)
	}
	issueURL := *urlFlag
	if issueURL == "" && fs.NArg() > 0 {
		issueURL = fs.Arg(0)
	}
	if issueURL == "" {
		return fatal(jsonOut, "umbrella: an issue URL is required")
	}

	// Expansion writes many tasks under the server's locks, so the server runs
	// the whole operation rather than this process racing it.
	res, err := callAPIWithin[umbrellaExpandDTO](api, apiSlowCallTimeout, taskServiceName, "ExpandUmbrella", issueURL, *model)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	return reportUmbrella(jsonOut, res.UmbrellaURL, res.Created, res.Skipped, res.Degraded)
}

func reportUmbrella(jsonOut bool, umbrellaURL string, created, skipped int, degraded bool) int {
	if jsonOut {
		out, _ := json.Marshal(map[string]any{
			"umbrella": umbrellaURL,
			"created":  created,
			"skipped":  skipped,
			"degraded": degraded,
		})
		fmt.Println(string(out))
		return 0
	}
	fmt.Printf("Expanded %s: created %d child task(s), %d skipped (done or already present).\n", umbrellaURL, created, skipped)
	if degraded {
		fmt.Println("WARNING: planner exhausted its retries — fell back to an independent-parallel plan (no derived ordering, degraded parallelism cap).")
	}
	return 0
}

// umbrellaExpandDTO mirrors the server's wire shape for an expansion result.
type umbrellaExpandDTO struct {
	UmbrellaURL string `json:"umbrellaUrl"`
	Created     int    `json:"created"`
	Skipped     int    `json:"skipped"`
	Degraded    bool   `json:"degraded"`
	ChildCount  int    `json:"childCount"`
	MaxParallel int    `json:"maxParallel"`
}
