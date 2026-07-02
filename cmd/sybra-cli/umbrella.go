package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// cmdUmbrella expands a GitHub umbrella issue into a gated task DAG: one
// `umbrella` tracker task plus one `blocked` child per sub-issue, with
// dependency edges extracted by an LLM planner. Re-running is idempotent —
// only sub-issues without an existing task are materialized.
func cmdUmbrella(s *task.Manager, args []string, jsonOut bool) int {
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

	res, err := umbrella.Expand(context.Background(), s, umbrella.FallbackPlannerRunner(*model), issueURL)
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
		fmt.Println("WARNING: planner exhausted its retries — fell back to a linear-chain plan (serial, no parallelism).")
	}
	return 0
}
