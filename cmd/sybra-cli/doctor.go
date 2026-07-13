package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// cmdDoctor dispatches `sybra-cli doctor <sub>`. Currently only `cleanup`.
func cmdDoctor(cfg *config.Config, store *task.Manager, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: doctor <cleanup>")
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "cleanup":
		return cmdDoctorCleanup(cfg, store, rest, jsonOut)
	default:
		return fatal(jsonOut, "unknown doctor command: %s", sub)
	}
}

type doctorCleanupBucketJSON struct {
	Name        string   `json:"name"`
	Risk        string   `json:"risk"`
	Description string   `json:"description"`
	Items       int      `json:"items"`
	Bytes       int64    `json:"bytes"`
	Paths       []string `json:"paths,omitempty"`
}

type doctorCleanupSkipJSON struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type doctorCleanupErrorJSON struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type doctorCleanupBucketResultJSON struct {
	Name           string                   `json:"name"`
	ReclaimedBytes int64                    `json:"reclaimedBytes"`
	Removed        int                      `json:"removed"`
	Skipped        []doctorCleanupSkipJSON  `json:"skipped,omitempty"`
	Errors         []doctorCleanupErrorJSON `json:"errors,omitempty"`
}

// doctorCleanupReport is the --json shape for `doctor cleanup`. Applied is
// false for a dry run (Results is then always empty); Buckets always
// reflects what Scan found eligible under the resolved flags.
type doctorCleanupReport struct {
	Applied bool                            `json:"applied"`
	Buckets []doctorCleanupBucketJSON       `json:"buckets"`
	Results []doctorCleanupBucketResultJSON `json:"results,omitempty"`
}

// cmdDoctorCleanup implements `sybra-cli doctor cleanup`. Exit codes: 0 = ok
// (dry run printed, or apply finished with no delete errors); 1 = apply
// finished but at least one path failed to delete; 2 = bad usage (unknown
// flag value, invalid --older-than, unknown --only bucket).
func cmdDoctorCleanup(cfg *config.Config, store *task.Manager, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("doctor cleanup", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "delete eligible resources instead of only reporting them (default: dry-run)")
	only := fs.String("only", "", "comma-separated bucket names to limit to ("+strings.Join(cleanup.AllBucketNames(), ", ")+")")
	worktrees := fs.Bool("worktrees", false, "include the destructive worktrees bucket (git worktrees deleted irreversibly)")
	external := fs.Bool("external", false, "include the destructive shared-cache bucket plus the report-only external (docker) bucket")
	force := fs.Bool("force", false, "bypass the dirty-worktree safety check for the destructive worktrees bucket")
	olderThan := fs.String("older-than", "", "override the log/audit file age threshold (e.g. 72h); does not affect sandbox/worktree/go-build-cache eligibility")
	if err := fs.Parse(args); err != nil {
		return fatalUsage(jsonOut, "%v", err)
	}

	var onlyNames []string
	if strings.TrimSpace(*only) != "" {
		for n := range strings.SplitSeq(*only, ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if !cleanup.ValidBucketName(n) {
				return fatalUsage(jsonOut, "unknown --only bucket %q (valid: %s)", n, strings.Join(cleanup.AllBucketNames(), ", "))
			}
			onlyNames = append(onlyNames, n)
		}
	}

	var olderThanDur time.Duration
	if strings.TrimSpace(*olderThan) != "" {
		d, err := time.ParseDuration(*olderThan)
		if err != nil {
			return fatalUsage(jsonOut, "invalid --older-than %q: %v", *olderThan, err)
		}
		olderThanDur = d
	}

	opts := cleanup.Options{
		Only:      onlyNames,
		Worktrees: *worktrees,
		External:  *external,
		Force:     *force,
		OlderThan: olderThanDur,
	}

	scanner := cleanup.NewScanner(cfg, store)
	scanResult, err := scanner.Scan(opts)
	if err != nil {
		return fatal(jsonOut, "scan: %v", err)
	}

	report := doctorCleanupReport{Applied: *apply}
	for _, b := range scanResult.Buckets {
		report.Buckets = append(report.Buckets, doctorCleanupBucketJSON{
			Name:        b.Name,
			Risk:        string(b.Risk),
			Description: b.Description,
			Items:       b.Items,
			Bytes:       b.Bytes,
			Paths:       b.Paths,
		})
	}

	exitCode := 0
	if *apply {
		applyResult, err := scanner.Apply(scanResult.Buckets, opts)
		if err != nil {
			return fatal(jsonOut, "apply: %v", err)
		}
		for _, br := range applyResult.Buckets {
			rj := doctorCleanupBucketResultJSON{Name: br.Name, ReclaimedBytes: br.ReclaimedBytes, Removed: br.Removed}
			for _, sk := range br.Skipped {
				rj.Skipped = append(rj.Skipped, doctorCleanupSkipJSON{Path: sk.Path, Reason: sk.Reason})
			}
			for _, e := range br.Errors {
				rj.Errors = append(rj.Errors, doctorCleanupErrorJSON{Path: e.Path, Error: e.Err})
				exitCode = 1
			}
			report.Results = append(report.Results, rj)
		}
	}

	if jsonOut {
		if code := printJSON(report); code != 0 {
			return code
		}
		return exitCode
	}

	renderDoctorCleanupHuman(report)
	return exitCode
}

func renderDoctorCleanupHuman(report doctorCleanupReport) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "BUCKET\tRISK\tITEMS\tSIZE")
	for _, b := range report.Buckets {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", b.Name, b.Risk, b.Items, humanBytes(b.Bytes))
	}
	_ = w.Flush()

	if !report.Applied {
		fmt.Println("\nDry run — nothing was deleted. Pass --apply to delete eligible resources;")
		fmt.Println("destructive buckets also need --worktrees / --external.")
		return
	}

	fmt.Println()
	for _, r := range report.Results {
		fmt.Printf("%s: removed %d, reclaimed %s\n", r.Name, r.Removed, humanBytes(r.ReclaimedBytes))
		for _, sk := range r.Skipped {
			fmt.Printf("  skip  %s: %s\n", sk.Path, sk.Reason)
		}
		for _, e := range r.Errors {
			fmt.Printf("  error %s: %s\n", e.Path, e.Error)
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// fatalUsage mirrors fatal but exits 2 — reserved for bad flags/arguments
// (as opposed to a runtime failure), so scripts can tell "you asked for
// something invalid" apart from "the operation itself failed".
func fatalUsage(jsonOut bool, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	if jsonOut {
		fmt.Fprintf(os.Stderr, `{"error":"%s"}`+"\n", msg)
	} else {
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}
	return 2
}
