package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/selfmonitor"
)

func cmdSelfmonitor(cfg *config.Config, api *apiClient, store taskBoard, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: selfmonitor <scan|investigate|ledger> [flags]")
	}
	switch args[0] {
	case "scan":
		return cmdSelfmonitorScan(api, jsonOut)
	case "investigate":
		return cmdSelfmonitorInvestigate(cfg, api, store, args[1:], jsonOut)
	case "ledger":
		return cmdSelfmonitorLedger(api, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown selfmonitor subcommand: %s", args[0])
	}
}

// cmdSelfmonitorScan reads the persisted report the service writes each
// tick. Fast and side-effect-free — the normal "what did self-monitor find"
// entry point. Errors with a helpful message if no report exists yet.
func cmdSelfmonitorScan(api *apiClient, jsonOut bool) int {
	if api != nil {
		report, err := callAPI[selfmonitor.Report](api, selfMonitorServiceName, "GetSelfMonitorReport")
		if err != nil {
			return fatal(jsonOut, "%v", err)
		}
		return reportSelfmonitor(jsonOut, &report)
	}

	data, err := os.ReadFile(config.SelfMonitorLastReportPath())
	if err != nil {
		if os.IsNotExist(err) {
			return fatal(jsonOut, "no selfmonitor report yet (run `sybra-cli selfmonitor investigate` for a one-shot pass, or start sybra with self_monitor.enabled=true)")
		}
		return fatal(jsonOut, "read selfmonitor report: %v", err)
	}
	var report selfmonitor.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return fatal(jsonOut, "parse selfmonitor report: %v", err)
	}
	return reportSelfmonitor(jsonOut, &report)
}

func reportSelfmonitor(jsonOut bool, report *selfmonitor.Report) int {
	if jsonOut {
		return printJSON(*report)
	}
	printSelfmonitorReport(report)
	return 0
}

// cmdSelfmonitorInvestigate runs a one-shot Scan() using the current health
// report and ledger, without persisting or filing issues. Useful for
// operators wanting to preview a tick before enabling the background loop.
func cmdSelfmonitorInvestigate(cfg *config.Config, api *apiClient, store taskBoard, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	// The health report, the ledger and the logs the scan reads all belong to
	// the instance that owns the board, so a reachable server runs the pass.
	if api != nil {
		report, err := callAPIWithin[selfmonitor.Report](api, apiSlowCallTimeout, selfMonitorServiceName, "InvestigateSelfMonitor")
		if err != nil {
			return fatal(jsonOut, "scan: %v", err)
		}
		return reportSelfmonitor(jsonOut, &report)
	}

	ledger, err := selfmonitor.Open(config.SelfMonitorLedgerPath())
	if err != nil {
		return fatal(jsonOut, "open ledger: %v", err)
	}

	// Force Enabled so Scan() works even when the operator hasn't flipped
	// the config block on yet — investigate is meant to be a preview.
	scfg := cfg.SelfMonitor
	scfg.Enabled = true

	svc := selfmonitor.NewService(selfmonitor.Deps{
		Cfg:     scfg,
		Tasks:   store,
		Health:  selfmonitor.DiskHealthReader{Path: config.HealthReportPath()},
		Ledger:  ledger,
		LogsDir: cfg.Logging.Dir,
	})
	report, err := svc.Scan(context.Background())
	if err != nil {
		return fatal(jsonOut, "scan: %v", err)
	}
	return reportSelfmonitor(jsonOut, &report)
}

// cmdSelfmonitorLedger prints the append-only ledger history, optionally
// filtered to a single fingerprint. Useful when debugging why a finding was
// auto-suppressed or verifying that an action landed.
func cmdSelfmonitorLedger(api *apiClient, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("ledger", flag.ContinueOnError)
	fpFilter := fs.String("fingerprint", "", "filter entries by fingerprint")
	sinceFlag := fs.String("since", "", "only include entries newer than this duration (e.g. 24h, 7d)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	var window time.Duration
	if *sinceFlag != "" {
		parsed, err := parseDurationFlag(*sinceFlag)
		if err != nil {
			return fatal(jsonOut, "parse --since: %v", err)
		}
		window = parsed
	}

	entries, err := readSelfmonitorLedger(api, *fpFilter, window)
	if err != nil {
		return fatal(jsonOut, "open ledger: %v", err)
	}

	if jsonOut {
		return printJSON(entries)
	}

	if len(entries) == 0 {
		fmt.Println("ledger: no matching entries")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TIME\tFINGERPRINT\tVERDICT\tISSUE\tACTION")
	for i := range entries {
		e := &entries[i]
		issueCol := "-"
		if e.IssueNumber > 0 {
			issueCol = fmt.Sprintf("#%d/%s", e.IssueNumber, e.IssueState)
		}
		actionCol := e.Action
		if actionCol == "" {
			actionCol = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.CreatedAt.Format(time.RFC3339),
			e.Fingerprint,
			e.Verdict,
			issueCol,
			actionCol,
		)
	}
	_ = w.Flush()
	return 0
}

func printSelfmonitorReport(r *selfmonitor.Report) {
	fmt.Printf("selfmonitor: findings=%d suppressed=%d duration_ms=%d cost_usd=%.4f\n",
		len(r.Findings), r.Suppressed, r.DurationMS, r.CostUSD)
	if r.HealthScore != "" {
		fmt.Printf("health score: %s\n", r.HealthScore)
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		fmt.Printf("  [%s] %s: %s", f.Finding.Severity, f.Finding.Category, f.Finding.Title)
		if f.LogSummary != nil {
			fmt.Printf("  (tools=%d cost=$%.4f stall=%t)",
				f.LogSummary.TotalToolCalls,
				f.LogSummary.TotalCostUSD,
				f.LogSummary.StallDetected)
		}
		fmt.Println()
	}
}

// parseDurationFlag accepts Go's time.ParseDuration syntax plus the "Nd"
// suffix for days, matching the existing cmdAudit --since convention.
func parseDurationFlag(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		days := s[:len(s)-1]
		n, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, err
		}
		return n * 24, nil
	}
	return time.ParseDuration(s)
}

// readSelfmonitorLedger reads the ledger of whichever instance owns the board. It is an append-only file under that instance's home, so this machine's copy answers for a different board.
func readSelfmonitorLedger(api *apiClient, fingerprint string, window time.Duration) ([]selfmonitor.LedgerEntry, error) {
	if api != nil {
		return callAPI[[]selfmonitor.LedgerEntry](api, selfMonitorServiceName, "ListSelfMonitorLedger",
			fingerprint, int64(window/time.Second))
	}
	ledger, err := selfmonitor.Open(config.SelfMonitorLedgerPath())
	if err != nil {
		return nil, err
	}
	var entries []selfmonitor.LedgerEntry
	if fingerprint != "" {
		entries = ledger.History(fingerprint, window)
	} else {
		entries = ledger.Entries(window)
	}
	slices.SortFunc(entries, func(a, b selfmonitor.LedgerEntry) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return entries, nil
}
