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
)

// cmdDoctor inspects and repairs this machine's own disk.
//
// It runs with no server so an operator can reach for it when the server is
// what broke, but every path it would delete belongs to a live task, and only
// the board knows which those are. Without one it reports and refuses to
// delete, rather than treating every worktree as an orphan.
func cmdDoctor(cfg *config.Config, store taskBoard, ownsHome bool, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: doctor <cleanup>")
	}
	if store == nil || !ownsHome {
		return cmdDoctorWithoutBoard(cfg, store, ownsHome, args, jsonOut)
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

type doctorCleanupFindingJSON struct {
	ID            string             `json:"id"`
	Kind          string             `json:"kind"`
	State         string             `json:"state"`
	TaskID        string             `json:"taskId,omitempty"`
	LinkedTaskID  string             `json:"linkedTaskId,omitempty"`
	Path          string             `json:"path"`
	Reason        string             `json:"reason"`
	ObservedHead  string             `json:"observedHead,omitempty"`
	ObservedState string             `json:"observedState,omitempty"`
	BytesRetained int64              `json:"bytesRetained"`
	FirstSeenAt   time.Time          `json:"firstSeenAt"`
	LastSeenAt    time.Time          `json:"lastSeenAt"`
	LastChangedAt time.Time          `json:"lastChangedAt"`
	ResolvedAt    time.Time          `json:"resolvedAt,omitzero"`
	Rescue        cleanup.RescueInfo `json:"rescue,omitzero"`
}

// cmdDoctorCleanup implements `sybra-cli doctor cleanup`. Exit codes: 0 = ok
// (dry run printed, or apply finished with no delete errors); 1 = apply
// finished but at least one path failed to delete; 2 = bad usage (unknown
// flag value, invalid --older-than, unknown --only bucket).
func cmdDoctorCleanup(cfg *config.Config, store taskBoard, args []string, jsonOut bool) int {
	if len(args) > 0 && args[0] == "findings" {
		return cmdDoctorCleanupFindings(store, args[1:], jsonOut)
	}
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

func cmdDoctorCleanupFindings(store taskBoard, args []string, jsonOut bool) int {
	protected := cleanup.DefaultProtectedStore()
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "list":
		findings, err := protected.List()
		if err != nil {
			return fatal(jsonOut, "list findings: %v", err)
		}
		return renderDoctorCleanupFindings(findings, jsonOut)
	case "inspect":
		if len(args) != 1 {
			return fatalUsage(jsonOut, "usage: doctor cleanup findings inspect <id>")
		}
		finding, ok, err := protected.Get(args[0])
		if err != nil {
			return fatal(jsonOut, "inspect finding: %v", err)
		}
		if !ok {
			return fatal(jsonOut, "cleanup finding %q not found", args[0])
		}
		return renderDoctorCleanupFindings([]cleanup.Finding{finding}, jsonOut)
	case "discard":
		if len(args) != 1 {
			return fatalUsage(jsonOut, "usage: doctor cleanup findings discard <id>")
		}
		finding, err := protected.Discard(args[0])
		if err != nil {
			return fatal(jsonOut, "discard finding: %v", err)
		}
		return renderDoctorCleanupFindings([]cleanup.Finding{finding}, jsonOut)
	case "rescue":
		if len(args) != 1 {
			return fatalUsage(jsonOut, "usage: doctor cleanup findings rescue <id>")
		}
		finding, err := protected.Rescue(args[0])
		if err != nil {
			return fatal(jsonOut, "rescue finding: %v", err)
		}
		return renderDoctorCleanupFindings([]cleanup.Finding{finding}, jsonOut)
	case "reattach":
		fs := flag.NewFlagSet("doctor cleanup findings reattach", flag.ContinueOnError)
		taskID := fs.String("task", "", "task id to attach the protected resource finding to")
		if err := fs.Parse(args); err != nil {
			return fatalUsage(jsonOut, "%v", err)
		}
		rest := fs.Args()
		if len(rest) != 1 {
			return fatalUsage(jsonOut, "usage: doctor cleanup findings reattach --task <task-id> <id>")
		}
		if strings.TrimSpace(*taskID) == "" {
			return fatalUsage(jsonOut, "--task is required")
		}
		if _, err := store.Get(*taskID); err != nil {
			return fatal(jsonOut, "reattach target task %q: %v", *taskID, err)
		}
		finding, err := protected.Reattach(rest[0], *taskID)
		if err != nil {
			return fatal(jsonOut, "reattach finding: %v", err)
		}
		return renderDoctorCleanupFindings([]cleanup.Finding{finding}, jsonOut)
	default:
		return fatalUsage(jsonOut, "unknown doctor cleanup findings command %q", sub)
	}
}

func renderDoctorCleanupFindings(findings []cleanup.Finding, jsonOut bool) int {
	if jsonOut {
		out := make([]doctorCleanupFindingJSON, 0, len(findings))
		for i := range findings {
			out = append(out, doctorCleanupFindingJSON{
				ID:            findings[i].ID,
				Kind:          string(findings[i].Kind),
				State:         string(findings[i].State),
				TaskID:        findings[i].TaskID,
				LinkedTaskID:  findings[i].LinkedTaskID,
				Path:          findings[i].Path,
				Reason:        findings[i].Reason,
				ObservedHead:  findings[i].ObservedHead,
				ObservedState: findings[i].ObservedState,
				BytesRetained: findings[i].BytesRetained,
				FirstSeenAt:   findings[i].FirstSeenAt,
				LastSeenAt:    findings[i].LastSeenAt,
				LastChangedAt: findings[i].LastChangedAt,
				ResolvedAt:    findings[i].ResolvedAt,
				Rescue:        findings[i].Rescue,
			})
		}
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tKIND\tSTATE\tTASK\tSIZE\tPATH")
	for i := range findings {
		taskID := findings[i].TaskID
		if findings[i].LinkedTaskID != "" {
			taskID = findings[i].LinkedTaskID
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			findings[i].ID,
			findings[i].Kind,
			findings[i].State,
			taskID,
			humanBytes(findings[i].BytesRetained),
			findings[i].Path,
		)
	}
	_ = w.Flush()
	return 0
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

// cmdDoctorWithoutBoard answers the doctor subcommands that need no board, and
// refuses the scan by name rather than running it against a task list that does
// not describe this disk — which classifies every live worktree as an orphan
// and offers to delete it.
func cmdDoctorWithoutBoard(cfg *config.Config, store taskBoard, ownsHome bool, args []string, jsonOut bool) int {
	if args[0] != "cleanup" {
		return fatal(jsonOut, "unknown doctor command: %s", args[0])
	}
	// The protected-findings record is this machine's own file, so reading it
	// needs no board at all — and it is exactly what an operator asks for when
	// the server is what broke and the disk is filling up.
	if rest := args[1:]; len(rest) > 0 && rest[0] == "findings" && !needsBoard(rest[1:]) {
		return cmdDoctorCleanupFindings(store, rest[1:], jsonOut)
	}
	if !ownsHome {
		return fatal(jsonOut,
			"doctor cleanup deletes paths under this machine's home, and %s names a board on another machine, whose tasks do not describe them; unset it to use this machine's own board",
			serverTargetEnv)
	}
	return fatal(jsonOut,
		"doctor cleanup needs the board to tell a live worktree from an orphan, and no Sybra server is reachable; start one, or set %s",
		serverTargetEnv)
}

// needsBoard reports the findings subcommands that read a task, which is the
// only part of that subtree a board is required for.
func needsBoard(args []string) bool {
	return len(args) > 0 && args[0] == "reattach"
}
