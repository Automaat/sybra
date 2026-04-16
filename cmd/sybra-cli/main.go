package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}

	// Extract global --json flag before subcommand.
	jsonOut := false
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			filtered = append(filtered, a)
		}
	}

	if len(filtered) == 0 {
		usage()
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		return fatal(jsonOut, "load config: %v", err)
	}

	rawStore, err := task.NewStore(cfg.TasksDir)
	if err != nil {
		return fatal(jsonOut, "open store: %v", err)
	}
	store := task.NewManager(rawStore, nil)

	projStore, err := project.NewStore(cfg.ProjectsDir, cfg.ClonesDir)
	if err != nil {
		return fatal(jsonOut, "open project store: %v", err)
	}

	cmd, rest := filtered[0], filtered[1:]
	switch cmd {
	case "list":
		return cmdList(store, rest, jsonOut)
	case "get":
		return cmdGet(store, rest, jsonOut)
	case "create":
		return cmdCreate(store, rest, jsonOut)
	case "update":
		return cmdUpdate(store, rest, jsonOut)
	case "delete":
		return cmdDelete(store, rest, jsonOut)
	case "project":
		return cmdProject(projStore, rest, jsonOut)
	case "audit":
		return cmdAudit(cfg, rest, jsonOut)
	case "board":
		return cmdBoard(store, jsonOut)
	case "health":
		return cmdHealth(cfg, rest, jsonOut)
	case "triage":
		return cmdTriage(cfg, store, projStore, rest, jsonOut)
	case "monitor":
		return cmdMonitor(cfg, store, rest, jsonOut)
	case "selfmonitor":
		return cmdSelfmonitor(cfg, store, rest, jsonOut)
	default:
		return fatal(jsonOut, "unknown command: %s", cmd)
	}
}

func cmdList(s *task.Manager, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	status := fs.String("status", "", "filter by status")
	tag := fs.String("tag", "", "filter by tag")
	proj := fs.String("project", "", "filter by project id")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	tasks, err := s.List()
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if *status != "" {
		if _, err := task.ValidateStatus(*status); err != nil {
			return fatal(jsonOut, "%v", err)
		}
		tasks = filterStatus(tasks, *status)
	}
	if *tag != "" {
		tasks = filterTag(tasks, *tag)
	}
	if *proj != "" {
		tasks = filterProject(tasks, *proj)
	}

	if jsonOut {
		if tasks == nil {
			tasks = []task.Task{}
		}
		return printJSON(tasks)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATUS\tMODE\tTITLE")
	for i := range tasks {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", tasks[i].ID, tasks[i].Status, tasks[i].AgentMode, tasks[i].Title)
	}
	_ = w.Flush()
	return 0
}

func cmdGet(s *task.Manager, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: get <id>")
	}

	t, err := s.Get(args[0])
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(t)
	}

	fmt.Printf("ID:     %s\n", t.ID)
	fmt.Printf("Title:  %s\n", t.Title)
	fmt.Printf("Status: %s\n", t.Status)
	fmt.Printf("Mode:   %s\n", t.AgentMode)
	if t.TaskType != "" {
		fmt.Printf("Type:   %s\n", t.TaskType)
	}
	if len(t.Tags) > 0 {
		fmt.Printf("Tags:   %s\n", strings.Join(t.Tags, ", "))
	}
	if t.ProjectID != "" {
		fmt.Printf("Project: %s\n", t.ProjectID)
	}
	if t.Branch != "" {
		fmt.Printf("Branch: %s\n", t.Branch)
	}
	if t.PRNumber > 0 {
		fmt.Printf("PR: #%d\n", t.PRNumber)
	}
	if t.Issue != "" {
		fmt.Printf("Issue: %s\n", t.Issue)
	}
	fmt.Printf("Created: %s\n", t.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Printf("Updated: %s\n", t.UpdatedAt.Format("2006-01-02 15:04"))
	if t.Body != "" {
		fmt.Printf("\n%s\n", t.Body)
	}
	if t.Plan != "" {
		fmt.Printf("\n## Plan\n\n%s\n", t.Plan)
	}
	if t.PlanCritique != "" {
		fmt.Printf("\n## Plan Critique\n\n%s\n", t.PlanCritique)
	}
	return 0
}

func cmdCreate(s *task.Manager, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task body markdown")
	plan := fs.String("plan", "", "plan content markdown")
	planCritique := fs.String("plan-critique", "", "plan critique markdown")
	mode := fs.String("mode", "headless", "agent mode: headless|interactive")
	ttype := fs.String("type", "normal", "task type: normal|debug|research")
	tags := fs.String("tags", "", "comma-separated tags")
	proj := fs.String("project", "", "project id (owner/repo)")
	branch := fs.String("branch", "", "Git branch name")
	pr := fs.Int("pr", 0, "GitHub PR number")
	issue := fs.String("issue", "", "GitHub issue URL")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *title == "" {
		return fatal(jsonOut, "title is required")
	}
	if _, err := task.ValidateTaskType(*ttype); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	t, err := s.Create(*title, *body, *mode)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	updates := map[string]any{}
	if *ttype != "" && *ttype != string(task.TaskTypeNormal) {
		updates["task_type"] = *ttype
	}
	if *tags != "" {
		tagList := strings.Split(*tags, ",")
		for i := range tagList {
			tagList[i] = strings.TrimSpace(tagList[i])
		}
		updates["tags"] = tagList
	}
	if *proj != "" {
		updates["project_id"] = *proj
	}
	if *branch != "" {
		updates["branch"] = *branch
	}
	if *pr > 0 {
		updates["pr_number"] = float64(*pr)
	}
	if *issue != "" {
		updates["issue"] = *issue
	}
	if *plan != "" {
		updates["plan"] = *plan
	}
	if *planCritique != "" {
		updates["plan_critique"] = *planCritique
	}
	if len(updates) > 0 {
		t, err = s.UpdateMap(t.ID, updates)
		if err != nil {
			return fatal(jsonOut, "update after create: %v", err)
		}
	}

	if jsonOut {
		return printJSON(t)
	}
	fmt.Printf("Created task %s: %s\n", t.ID, t.Title)
	return 0
}

func cmdUpdate(s *task.Manager, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: update <id> [flags]")
	}

	id := args[0]
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	title := fs.String("title", "", "new title")
	status := fs.String("status", "", "new status")
	body := fs.String("body", "", "new body")
	plan := fs.String("plan", "", "plan content markdown (empty string clears plan)")
	planFile := fs.String("plan-file", "", "path to file with plan content")
	planCritique := fs.String("plan-critique", "", "plan critique markdown (empty string clears critique)")
	planCritiqueFile := fs.String("plan-critique-file", "", "path to file with plan critique content")
	mode := fs.String("mode", "", "new agent mode")
	ttype := fs.String("type", "", "new task type: normal|debug|research")
	tags := fs.String("tags", "", "comma-separated tags (replaces existing)")
	proj := fs.String("project", "", "project id (owner/repo)")
	branch := fs.String("branch", "", "Git branch name")
	pr := fs.Int("pr", 0, "GitHub PR number")
	issue := fs.String("issue", "", "GitHub issue URL")
	statusReason := fs.String("status-reason", "", "reason for status change")
	if err := fs.Parse(args[1:]); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	updates := map[string]any{}
	if *title != "" {
		updates["title"] = *title
	}
	if *status != "" {
		updates["status"] = *status
	}
	if *statusReason != "" {
		updates["status_reason"] = *statusReason
	}
	if *body != "" {
		updates["body"] = *body
	}
	if err := applyFileOrStringUpdate(fs, updates, "plan", "plan", *plan, *planFile); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if err := applyFileOrStringUpdate(fs, updates, "plan-critique", "plan_critique", *planCritique, *planCritiqueFile); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *mode != "" {
		updates["agent_mode"] = *mode
	}
	if *ttype != "" {
		if _, err := task.ValidateTaskType(*ttype); err != nil {
			return fatal(jsonOut, "%v", err)
		}
		updates["task_type"] = *ttype
	}
	if *tags != "" {
		tagList := strings.Split(*tags, ",")
		for i := range tagList {
			tagList[i] = strings.TrimSpace(tagList[i])
		}
		updates["tags"] = tagList
	}
	if *proj != "" {
		updates["project_id"] = *proj
	}
	if *branch != "" {
		updates["branch"] = *branch
	}
	if *pr > 0 {
		updates["pr_number"] = float64(*pr)
	}
	if *issue != "" {
		updates["issue"] = *issue
	}

	if len(updates) == 0 {
		return fatal(jsonOut, "no updates specified")
	}

	t, err := s.UpdateMap(id, updates)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(t)
	}
	fmt.Printf("Updated task %s\n", t.ID)
	return 0
}

func cmdDelete(s *task.Manager, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: delete <id>")
	}

	if err := s.Delete(args[0]); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(map[string]string{"deleted": args[0]})
	}
	fmt.Printf("Deleted task %s\n", args[0])
	return 0
}

// applyFileOrStringUpdate populates an updates map from a paired
// `--<flag>` / `--<flag>-file` flag pair. File takes precedence; an
// explicitly empty string flag clears the value (matches the existing
// `--plan` clear-on-empty semantics).
func applyFileOrStringUpdate(fs *flag.FlagSet, updates map[string]any, flagName, updateKey, strVal, fileVal string) error {
	switch {
	case fileVal != "":
		data, err := os.ReadFile(fileVal)
		if err != nil {
			return fmt.Errorf("read %s file: %w", flagName, err)
		}
		updates[updateKey] = string(data)
	case strVal != "":
		updates[updateKey] = strVal
	default:
		fs.Visit(func(f *flag.Flag) {
			if f.Name == flagName {
				updates[updateKey] = ""
			}
		})
	}
	return nil
}

func filterStatus(tasks []task.Task, status string) []task.Task {
	var out []task.Task
	for i := range tasks {
		if string(tasks[i].Status) == status {
			out = append(out, tasks[i])
		}
	}
	return out
}

func filterTag(tasks []task.Task, tag string) []task.Task {
	var out []task.Task
	for i := range tasks {
		if slices.Contains(tasks[i].Tags, tag) {
			out = append(out, tasks[i])
		}
	}
	return out
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, `{"error":"%v"}`+"\n", err)
		return 1
	}
	return 0
}

func fatal(jsonOut bool, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	if jsonOut {
		fmt.Fprintf(os.Stderr, `{"error":"%s"}`+"\n", msg)
	} else {
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}
	return 1
}

func filterProject(tasks []task.Task, projectID string) []task.Task {
	var out []task.Task
	for i := range tasks {
		if tasks[i].ProjectID == projectID {
			out = append(out, tasks[i])
		}
	}
	return out
}

func cmdProject(ps *project.Store, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: project <list|get|create|update|delete> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdProjectList(ps, jsonOut)
	case "get":
		return cmdProjectGet(ps, rest, jsonOut)
	case "create":
		return cmdProjectCreate(ps, rest, jsonOut)
	case "update":
		return cmdProjectUpdate(ps, rest, jsonOut)
	case "delete":
		return cmdProjectDelete(ps, rest, jsonOut)
	default:
		return fatal(jsonOut, "unknown project command: %s", sub)
	}
}

func cmdProjectList(ps *project.Store, jsonOut bool) int {
	projects, err := ps.List()
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		if projects == nil {
			projects = []project.Project{}
		}
		return printJSON(projects)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tTYPE\tNAME\tURL")
	for i := range projects {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", projects[i].ID, projects[i].Type, projects[i].Name, projects[i].URL)
	}
	_ = w.Flush()
	return 0
}

func cmdProjectGet(ps *project.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: project get <id>")
	}
	p, err := ps.Get(args[0])
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(p)
	}
	fmt.Printf("ID:    %s\nName:  %s\nOwner: %s\nRepo:  %s\nURL:   %s\nType:  %s\nClone: %s\n",
		p.ID, p.Name, p.Owner, p.Repo, p.URL, p.Type, p.ClonePath)
	return 0
}

func cmdProjectCreate(ps *project.Store, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("project create", flag.ContinueOnError)
	url := fs.String("url", "", "GitHub repository URL (required)")
	ptype := fs.String("type", "pet", "project type: pet|work")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *url == "" {
		return fatal(jsonOut, "url is required")
	}
	p, err := ps.Create(*url, project.ProjectType(*ptype))
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(p)
	}
	fmt.Printf("Created project %s\n", p.ID)
	return 0
}

func cmdProjectUpdate(ps *project.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: project update <id> [--type work|pet] [--setup-commands cmd1,cmd2]")
	}
	id := args[0]
	fs := flag.NewFlagSet("project update", flag.ContinueOnError)
	ptype := fs.String("type", "", "project type: pet|work")
	setupCmds := fs.String("setup-commands", "", "comma-separated commands to run after worktree creation")
	if err := fs.Parse(args[1:]); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *ptype == "" && *setupCmds == "" {
		return fatal(jsonOut, "at least one of --type or --setup-commands is required")
	}

	var p project.Project
	var err error

	if *ptype != "" {
		p, err = ps.Update(id, project.ProjectType(*ptype))
		if err != nil {
			return fatal(jsonOut, "%v", err)
		}
	}
	if *setupCmds != "" {
		cmds := strings.Split(*setupCmds, ",")
		p, err = ps.SetSetupCommands(id, cmds)
		if err != nil {
			return fatal(jsonOut, "%v", err)
		}
	}

	if jsonOut {
		return printJSON(p)
	}
	fmt.Printf("Updated project %s (type: %s)\n", p.ID, p.Type)
	return 0
}

func cmdProjectDelete(ps *project.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: project delete <id>")
	}
	if err := ps.Delete(args[0]); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(map[string]string{"deleted": args[0]})
	}
	fmt.Printf("Deleted project %s\n", args[0])
	return 0
}

type boardTask struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	ProjectID    string    `json:"project_id,omitempty"`
	AgentID      string    `json:"agent_id,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	RunningForS  int64     `json:"running_for_s,omitempty"`
	StatusReason string    `json:"status_reason,omitempty"`
}

type boardSummary struct {
	Counts        map[string]int `json:"counts"`
	InProgress    []boardTask    `json:"in_progress"`
	PlanReview    []boardTask    `json:"plan_review"`
	HumanRequired []boardTask    `json:"human_required"`
}

func cmdBoard(s *task.Manager, jsonOut bool) int {
	tasks, err := s.List()
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	counts := make(map[string]int)
	for _, st := range task.AllStatuses() {
		counts[string(st)] = 0
	}
	for i := range tasks {
		counts[string(tasks[i].Status)]++
	}

	now := time.Now()
	toBoardTask := func(t task.Task) boardTask {
		bt := boardTask{
			ID:           t.ID,
			Title:        t.Title,
			ProjectID:    t.ProjectID,
			StatusReason: t.StatusReason,
		}
		// Find the latest running agent run.
		for j := len(t.AgentRuns) - 1; j >= 0; j-- {
			run := t.AgentRuns[j]
			if run.State == "running" || (!run.StartedAt.IsZero() && bt.AgentID == "") {
				bt.AgentID = run.AgentID
				bt.StartedAt = run.StartedAt
				bt.RunningForS = int64(now.Sub(run.StartedAt).Seconds())
				break
			}
		}
		return bt
	}

	summary := boardSummary{
		Counts:        counts,
		InProgress:    []boardTask{},
		PlanReview:    []boardTask{},
		HumanRequired: []boardTask{},
	}

	for i := range tasks {
		switch tasks[i].Status {
		case task.StatusInProgress:
			summary.InProgress = append(summary.InProgress, toBoardTask(tasks[i]))
		case task.StatusPlanReview:
			summary.PlanReview = append(summary.PlanReview, toBoardTask(tasks[i]))
		case task.StatusHumanRequired:
			summary.HumanRequired = append(summary.HumanRequired, toBoardTask(tasks[i]))
		default:
		}
	}

	if jsonOut {
		return printJSON(summary)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STATUS\tCOUNT")
	for _, st := range task.AllStatuses() {
		_, _ = fmt.Fprintf(w, "%s\t%d\n", st, counts[string(st)])
	}
	_ = w.Flush()

	if len(summary.InProgress) > 0 {
		fmt.Println("\nIN PROGRESS:")
		w2 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w2, "ID\tAGENT\tRUNNING_FOR\tTITLE")
		for _, t := range summary.InProgress {
			_, _ = fmt.Fprintf(w2, "%s\t%s\t%ds\t%s\n", t.ID, t.AgentID, t.RunningForS, t.Title)
		}
		_ = w2.Flush()
	}

	if len(summary.HumanRequired) > 0 {
		fmt.Println("\nHUMAN REQUIRED:")
		w3 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w3, "ID\tTITLE\tREASON")
		for _, t := range summary.HumanRequired {
			_, _ = fmt.Fprintf(w3, "%s\t%s\t%s\n", t.ID, t.Title, t.StatusReason)
		}
		_ = w3.Flush()
	}

	return 0
}

func cmdHealth(cfg *config.Config, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	severity := fs.String("severity", "", "filter by severity (warning|critical)")
	category := fs.String("category", "", "filter by category")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	path := filepath.Join(config.HomeDir(), "health-report.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fatal(jsonOut, "no health report yet (app must be running)")
		}
		return fatal(jsonOut, "read health report: %v", err)
	}

	var report healthReport
	if err := json.Unmarshal(data, &report); err != nil {
		return fatal(jsonOut, "parse health report: %v", err)
	}

	if *severity != "" || *category != "" {
		var filtered []json.RawMessage
		for _, raw := range report.Findings {
			var f struct {
				Severity string `json:"severity"`
				Category string `json:"category"`
			}
			if err := json.Unmarshal(raw, &f); err != nil {
				continue
			}
			if *severity != "" && f.Severity != *severity {
				continue
			}
			if *category != "" && f.Category != *category {
				continue
			}
			filtered = append(filtered, raw)
		}
		report.Findings = filtered
	}

	if jsonOut {
		return printJSON(report)
	}

	fmt.Printf("Health Report (generated %s)\n", report.GeneratedAt)
	fmt.Printf("Period: %s to %s\n", report.PeriodStart, report.PeriodEnd)
	if report.Score != "" {
		fmt.Printf("Score: %s\n", report.Score)
	}
	fmt.Printf("Findings: %d\n\n", len(report.Findings))

	for _, raw := range report.Findings {
		var f struct {
			Severity string `json:"severity"`
			Category string `json:"category"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		fmt.Printf("  [%s] %s: %s\n", f.Severity, f.Category, f.Title)
	}
	return 0
}

// healthReport mirrors the JSON structure without importing the health package.
type healthReport struct {
	GeneratedAt string            `json:"generatedAt"`
	PeriodStart string            `json:"periodStart"`
	PeriodEnd   string            `json:"periodEnd"`
	Score       string            `json:"score"`
	Findings    []json.RawMessage `json:"findings"`
	Stats       json.RawMessage   `json:"stats"`
}

func cmdMonitor(cfg *config.Config, store *task.Manager, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: monitor <scan> [--json]")
	}
	switch args[0] {
	case "scan":
		return cmdMonitorScan(cfg, store, jsonOut)
	default:
		return fatal(jsonOut, "unknown monitor subcommand: %s", args[0])
	}
}

func cmdMonitorScan(cfg *config.Config, store *task.Manager, jsonOut bool) int {
	svc := monitor.NewService(monitor.Deps{
		Cfg:        cfg.Monitor,
		Tasks:      store,
		Audit:      monitor.AuditDirReader(cfg.AuditDir()),
		Agents:     nil,
		Dispatcher: monitor.NoopDispatcher(),
		Sink:       monitor.NoopSink(),
	})
	report, err := svc.Scan(context.Background())
	if err != nil {
		return fatal(jsonOut, "scan: %v", err)
	}
	if jsonOut {
		return printJSON(report)
	}
	kinds := ""
	for _, a := range report.Anomalies {
		if kinds != "" {
			kinds += " "
		}
		if a.TaskID != "" {
			kinds += string(a.Kind) + ":" + a.TaskID
		} else {
			kinds += string(a.Kind)
		}
	}
	fmt.Printf("monitor: new=%d todo=%d in-progress=%d in-review=%d plan-review=%d human-required=%d done=%d | drift=%d",
		report.Counts.New,
		report.Counts.Todo,
		report.Counts.InProgress,
		report.Counts.InReview,
		report.Counts.PlanReview,
		report.Counts.HumanRequired,
		report.Counts.Done,
		len(report.Anomalies),
	)
	if kinds != "" {
		fmt.Printf(" | %s", kinds)
	}
	fmt.Println()
	return 0
}

func cmdAudit(cfg *config.Config, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	since := fs.String("since", "24h", "start of time window (duration like 24h/7d or date YYYY-MM-DD)")
	until := fs.String("until", "", "end of time window (date YYYY-MM-DD, default: now)")
	eventType := fs.String("type", "", "filter by event type prefix")
	taskID := fs.String("task", "", "filter by task ID")
	summary := fs.Bool("summary", false, "output aggregated summary instead of raw events")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	now := time.Now().UTC()
	sinceTime := parseSince(*since, now)
	untilTime := now
	if *until != "" {
		if t, err := time.Parse(time.DateOnly, *until); err == nil {
			untilTime = t.Add(24*time.Hour - time.Nanosecond)
		}
	}

	q := audit.Query{
		Since:  sinceTime,
		Until:  untilTime,
		Type:   *eventType,
		TaskID: *taskID,
	}

	events, err := audit.Read(cfg.AuditDir(), q)
	if err != nil {
		return fatal(jsonOut, "read audit: %v", err)
	}

	if *summary {
		s := audit.Summarize(events, sinceTime, untilTime)
		return printJSON(s)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		for i := range events {
			_ = enc.Encode(events[i])
		}
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TIMESTAMP\tTYPE\tTASK\tAGENT\tDATA")
	for i := range events {
		e := events[i]
		dataStr := ""
		for k, v := range e.Data {
			if dataStr != "" {
				dataStr += " "
			}
			dataStr += fmt.Sprintf("%s=%v", k, v)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.Timestamp.Format("2006-01-02 15:04:05"),
			e.Type, e.TaskID, e.AgentID, dataStr)
	}
	_ = w.Flush()
	return 0
}

func parseSince(s string, now time.Time) time.Time {
	// Try duration formats: "24h", "7d", "30d"
	if strings.HasSuffix(s, "d") {
		if n, err := fmt.Sscanf(s, "%d", new(int)); err == nil && n == 1 {
			var days int
			_, _ = fmt.Sscanf(s, "%d", &days)
			return now.AddDate(0, 0, -days)
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d)
	}
	// Try date format
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t
	}
	return now.Add(-24 * time.Hour)
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: sybra-cli [--json] <command> [flags]

Commands:
  list     [--status STATUS] [--tag TAG] [--project ID]
           STATUS: new|todo|planning|plan-review|in-progress|in-review|testing|test-plan-review|human-required|done|cancelled
  get      <id>
  create   --title TITLE [--body BODY] [--plan PLAN] [--mode MODE] [--type TYPE] [--tags t1,t2] [--project ID] [--branch B] [--pr N] [--issue URL]
           TYPE: normal|debug|research
  update   <id> [--title T] [--status S] [--status-reason R] [--body B] [--plan PLAN] [--plan-file PATH] [--mode M] [--type TYPE] [--tags T] [--project ID] [--branch B] [--pr N] [--issue URL]
  delete   <id>

  project list
  project get <id>
  project create --url <github-url> [--type pet|work]
  project update <id> --type pet|work
  project delete <id>

  audit    [--since DURATION|DATE] [--until DATE] [--type TYPE] [--task ID] [--summary]
  board    (status counts + in-progress/plan-review/human-required task lists)
  monitor  scan [--json]    one-shot read-only detector pass (no remediation)
  health   [--severity warning|critical] [--category CATEGORY]

  triage classify <id>         Classify a single task via claude -p and apply the verdict.
  triage classify --all        Classify every task with status=new.

Global flags:
  --json   Output as JSON`)
}
