package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/codexhook"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/skills"
	"github.com/Automaat/sybra/internal/skillsync"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/tasksnapshot"
	"github.com/Automaat/sybra/internal/workflow"
	"gopkg.in/yaml.v3"
)

// hookTaskIDRe mirrors agent.safeArgRe: alphanumerics, dot, underscore,
// hyphen, forward-slash. Shared allowlist keeps arg builder and hook receiver
// in sync without an import cycle.
var hookTaskIDRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}

	// Extract global --json and --home flags before subcommand.
	jsonOut := false
	filtered := make([]string, 0, len(args))
	homeOverride := ""
	homeErr := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--home":
			if i+1 >= len(args) {
				homeErr = true
				continue
			}
			i++
			homeOverride = args[i]
		case strings.HasPrefix(a, "--home="):
			homeOverride = strings.TrimPrefix(a, "--home=")
		default:
			filtered = append(filtered, a)
		}
	}

	// Detect the hook subcommand before config.Load can abort: codex lifecycle
	// hooks must fail open (see cmdHook) — a malformed config must never make
	// `sybra-cli hook` exit non-zero and stall an agent run.
	isHook := len(filtered) >= 1 && filtered[0] == "hook"

	if homeErr {
		if isHook {
			fmt.Fprintln(os.Stderr, "hook: --home requires a value (continuing fail-open)")
			return 0
		}
		return fatal(jsonOut, "--home requires a value")
	}

	if len(filtered) == 0 {
		usage()
		return 1
	}

	// Home precedence: --home > SYBRA_CONTROL_HOME (the real operator store,
	// injected into task-scoped agent subprocesses) > SYBRA_HOME (ambient,
	// e.g. the per-task sandbox) > config.Load's own default resolution.
	// Bare `sybra-cli` calls from inside an agent land on SYBRA_CONTROL_HOME so
	// task CRUD reaches the real board even though the agent's own SYBRA_HOME
	// points at its sandbox; `--home` lets an agent explicitly inspect the
	// sandbox/app-under-test store instead (see docs/manual-testing.md).
	effectiveHome := homeOverride
	fromControlHome := false
	fromSybraHome := false
	if effectiveHome == "" {
		if controlHome := os.Getenv("SYBRA_CONTROL_HOME"); controlHome != "" {
			effectiveHome = controlHome
			fromControlHome = true
		}
	}
	if effectiveHome == "" {
		if sybraHome := os.Getenv("SYBRA_HOME"); sybraHome != "" {
			effectiveHome = sybraHome
			fromSybraHome = true
		}
	}

	restoreHome := func() {}
	if effectiveHome != "" {
		prevHome, hadHome := os.LookupEnv("SYBRA_HOME")
		if err := os.Setenv("SYBRA_HOME", effectiveHome); err != nil {
			if isHook {
				fmt.Fprintf(os.Stderr, "hook: apply --home: %v (continuing fail-open)\n", err)
				return 0
			}
			return fatal(jsonOut, "apply --home: %v", err)
		}
		restoreHome = func() {
			if hadHome {
				_ = os.Setenv("SYBRA_HOME", prevHome)
			} else {
				_ = os.Unsetenv("SYBRA_HOME")
			}
		}
	}
	defer restoreHome()

	cfg, err := config.Load()
	if err != nil {
		if isHook {
			fmt.Fprintf(os.Stderr, "hook: load config: %v (continuing fail-open)\n", err)
			return 0
		}
		return fatal(jsonOut, "load config: %v", err)
	}

	// Fast path for hook subcommand — only needs cfg for AuditDir(), not stores.
	// Branch before store construction so cold start is cheap (hook is invoked
	// per-event by codex and must complete quickly).
	if isHook {
		return cmdHook(cfg, filtered[1:])
	}

	store, projStore, err := openStores(cfg)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	cmd, rest := filtered[0], filtered[1:]
	// HTTP auto-detect is only safe on the default control path: either no
	// explicit home override at all, or the task-scoped control-home bridge
	// back to the real operator store. A bare SYBRA_HOME override means the
	// caller explicitly targeted an on-disk store (common in tests/manual
	// harnesses), so reaching some unrelated reachable server would violate that
	// contract.
	allowHTTP := homeOverride == "" && (fromControlHome || !fromSybraHome)
	return dispatch(cmd, rest, cfg, store, projStore, allowHTTP, jsonOut)
}

// dispatch routes a parsed subcommand (with its own args and the global
// --json flag already extracted) to the matching cmdXxx handler.
func dispatch(cmd string, rest []string, cfg *config.Config, store *task.Manager, projStore *project.Store, allowHTTP, jsonOut bool) int {
	var api *apiClient
	switch cmd {
	case "create", "update", "link-pr", "delete":
		if allowHTTP {
			if c, ok := newAPIClient(cfg); ok && c.reachable(context.Background()) {
				api = c
			}
		}
	}
	switch cmd {
	case "list":
		return cmdList(store, rest, jsonOut)
	case "get":
		return cmdGet(store, rest, jsonOut)
	case "create":
		return cmdCreate(store, api, rest, jsonOut)
	case "handoff":
		return cmdHandoff(store, projStore, rest, jsonOut)
	case "umbrella":
		return cmdUmbrella(cfg, store, projStore, rest, jsonOut)
	case "update":
		return cmdUpdate(store, api, rest, jsonOut)
	case "link-pr":
		return cmdLinkPR(store, api, rest, jsonOut)
	case "delete":
		return cmdDelete(store, api, rest, jsonOut)
	case "reopen":
		return cmdReopen(store, rest, jsonOut)
	case "project":
		return cmdProject(projStore, rest, jsonOut)
	case "cluster":
		return cmdCluster(cfg, store, projStore, rest, jsonOut)
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
	case "evaluation":
		return cmdEvaluation(cfg, store, rest, jsonOut)
	case "harness-evolution":
		return cmdHarnessEvolution(cfg, store, rest, jsonOut)
	case "prompt-lab":
		return cmdPromptLab(cfg, store, projStore, rest, jsonOut)
	case "stats":
		return cmdStats(cfg, rest, jsonOut)
	case "install-skills":
		return cmdInstallSkills(cfg, jsonOut)
	case "artifact":
		return cmdArtifact(rest, jsonOut)
	case "progress":
		return cmdProgress(store, projStore, rest, jsonOut)
	case "config":
		return cmdConfig(cfg, rest, jsonOut)
	case "doctor":
		return cmdDoctor(cfg, store, rest, jsonOut)
	case "trash":
		return cmdTrash(store, rest, jsonOut)
	case "tasks-history":
		return cmdTasksHistory(cfg, rest, jsonOut)
	default:
		return fatal(jsonOut, "unknown command: %s", cmd)
	}
}

func openStores(cfg *config.Config) (*task.Manager, *project.Store, error) {
	rawStore, err := task.NewStore(cfg.TasksDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	projStore, err := project.NewStore(cfg.ProjectsDir, cfg.ClonesDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open project store: %w", err)
	}
	return task.NewManager(rawStore, nil), projStore, nil
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
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	compact := fs.Bool("compact", false, "omit planning support sidecars for implementation agents")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if fs.NArg() < 1 {
		return fatal(jsonOut, "usage: get [--compact] <id>")
	}

	t, err := s.Get(fs.Arg(0))
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *compact {
		if err := stripPlanningSupport(&t); err != nil {
			return fatal(jsonOut, "%v", err)
		}
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
	if t.RefIssue != "" {
		fmt.Printf("Ref Issue: %s\n", t.RefIssue)
	}
	if t.HandoffSourceProvider != "" {
		fmt.Printf("Source: %s\n", t.HandoffSourceProvider)
	}
	fmt.Printf("Created: %s\n", t.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Printf("Updated: %s\n", t.UpdatedAt.Format("2006-01-02 15:04"))
	if t.Body != "" {
		fmt.Printf("\n%s\n", t.Body)
	}
	if t.Plan != "" {
		fmt.Printf("\n## Plan\n\n%s\n", t.Plan)
	}
	if t.PlanContract != "" {
		fmt.Printf("\n## Plan Contract\n\n```json\n%s\n```\n", t.PlanContract)
	}
	if t.PlanCritique != "" {
		fmt.Printf("\n## Plan Critique\n\n%s\n", t.PlanCritique)
	}
	if t.PlanBrief != "" {
		fmt.Printf("\n## Plan Brief\n\n%s\n", t.PlanBrief)
	}
	if t.PlanDecisions != "" {
		fmt.Printf("\n## Plan Decisions\n\n%s\n", t.PlanDecisions)
	}
	if t.PlanResearch != "" {
		fmt.Printf("\n## Plan Research\n\n%s\n", t.PlanResearch)
	}
	if t.CodeReview != "" {
		fmt.Printf("\n## Code Review\n\n%s\n", t.CodeReview)
	}
	return 0
}

func stripPlanningSupport(t *task.Task) error {
	if t.PlanContract != "" {
		if sanitized, err := workflow.PlanContractPromptJSON(t.PlanContract, t.ID); err == nil {
			t.PlanContract = sanitized
		} else {
			return fmt.Errorf("sanitize plan contract for compact output: %w", err)
		}
	}
	t.PlanCritique = ""
	t.PlanResearch = ""
	t.PlanDecisions = ""
	t.PlanBrief = ""
	return nil
}

func cmdCreate(s *task.Manager, api *apiClient, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task body markdown")
	plan := fs.String("plan", "", "plan content markdown")
	planContract := fs.String("plan-contract", "", "executable plan contract JSON")
	planCritique := fs.String("plan-critique", "", "plan critique markdown")
	planResearch := fs.String("plan-research", "", "plan research markdown")
	planDecisions := fs.String("plan-decisions", "", "plan decisions markdown")
	planBrief := fs.String("plan-brief", "", "plan brief markdown")
	mode := fs.String("mode", "headless", "agent mode: headless|interactive")
	ttype := fs.String("type", "normal", "task type: normal|debug|research")
	tags := fs.String("tags", "", "comma-separated tags")
	proj := fs.String("project", "", "project id (owner/repo)")
	branch := fs.String("branch", "", "Git branch name")
	pr := fs.Int("pr", 0, "GitHub PR number")
	issue := fs.String("issue", "", "GitHub issue URL")
	allowDup := fs.Bool("allow-dup", false, "skip duplicate-dispatch check (project+issue+title)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *title == "" {
		return fatal(jsonOut, "title is required")
	}
	if _, err := task.ValidateTaskType(*ttype); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if !*allowDup {
		if dup, ok, err := findActiveDuplicate(s, *proj, *issue, *title); err != nil {
			return fatal(jsonOut, "dedup check: %v", err)
		} else if ok {
			fmt.Fprintf(os.Stderr, "warning: duplicate dispatch — returning existing task %s (status=%s) for project=%s issue=%s; pass --allow-dup to override\n",
				dup.ID, dup.Status, *proj, *issue)
			if jsonOut {
				return printJSON(dup)
			}
			fmt.Printf("Existing task %s: %s\n", dup.ID, dup.Title)
			return 0
		}
	}

	t, err := createTaskViaAPIOrFS(s, api, *title, *body, *mode)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	updates := buildCreateUpdateMap(*ttype, *tags, *proj, *branch, *pr, *issue,
		*plan, *planContract, *planCritique, *planResearch, *planDecisions, *planBrief)
	if len(updates) > 0 {
		t, err = updateTaskViaAPIOrFS(s, api, t.ID, updates)
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

func buildCreateUpdateMap(ttype, tags, proj, branch string, pr int, issue,
	plan, planContract, planCritique, planResearch, planDecisions, planBrief string) map[string]any {
	updates := map[string]any{}
	if ttype != "" && ttype != string(task.TaskTypeNormal) {
		updates["task_type"] = ttype
	}
	if tags != "" {
		tagList := strings.Split(tags, ",")
		for i := range tagList {
			tagList[i] = strings.TrimSpace(tagList[i])
		}
		updates["tags"] = tagList
	}
	if proj != "" {
		updates["project_id"] = proj
	}
	if branch != "" {
		updates["branch"] = branch
	}
	if pr > 0 {
		updates["pr_number"] = float64(pr)
	}
	if issue != "" {
		updates["issue"] = issue
	}
	if plan != "" {
		updates["plan"] = plan
	}
	if planContract != "" {
		updates["plan_contract"] = planContract
	}
	if planCritique != "" {
		updates["plan_critique"] = planCritique
	}
	if planResearch != "" {
		updates["plan_research"] = planResearch
	}
	if planDecisions != "" {
		updates["plan_decisions"] = planDecisions
	}
	if planBrief != "" {
		updates["plan_brief"] = planBrief
	}
	return updates
}

func createTaskViaAPIOrFS(s *task.Manager, api *apiClient, title, body, mode string) (task.Task, error) {
	if created, handled, apiErr := viaAPI[task.Task](api, "TaskService", "CreateTask", title, body, mode); handled {
		return created, apiErr
	}
	return s.Create(title, body, mode)
}

func updateTaskViaAPIOrFS(s *task.Manager, api *apiClient, id string, updates map[string]any) (task.Task, error) {
	if updated, handled, apiErr := viaAPI[task.Task](api, "TaskService", "UpdateTask", id, updates); handled {
		return updated, apiErr
	}
	return s.UpdateMap(id, updates)
}

// cmdHandoff creates a task pre-tagged for a Sybra workflow entry point. It
// bypasses triage/planning and either starts the requested agentic stage in an
// existing worktree or places the task in a raw status without workflow dispatch.
func cmdHandoff(s *task.Manager, ps *project.Store, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("handoff", flag.ContinueOnError)
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task body / research context markdown")
	plan := fs.String("plan", "", "approved plan markdown")
	planFile := fs.String("plan-file", "", "path to a file holding the approved plan (wins over --plan)")
	proj := fs.String("project", "", "project id (owner/repo); derived from the worktree origin remote when omitted")
	wtDir := fs.String("worktree-dir", "", "git worktree Sybra should reuse (default: current directory)")
	mode := fs.String("mode", "headless", "agent mode: headless|interactive")
	stage := fs.String("stage", "implement", "workflow entry stage: "+handoffStageCompactList())
	rawStatus := fs.String("status", "", "raw task status to create without starting a workflow")
	sourceProvider := fs.String("source-provider", "", "provider that produced the handed-off work: claude|codex|copilot|opencode")
	pr := fs.Int("pr", 0, "existing PR number to link when using --stage ready-pr")
	extraTags := fs.String("tags", "", "extra comma-separated tags")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *title == "" {
		return fatal(jsonOut, "title is required")
	}
	stageCfg, status, rawStatusMode, modeErr := resolveHandoffMode(fs, *stage, *rawStatus, *pr)
	if modeErr != nil {
		return fatal(jsonOut, "%v", modeErr)
	}
	handoffSource, srcErr := normalizeHandoffSourceProvider(*sourceProvider)
	if srcErr != nil {
		return fatal(jsonOut, "%v", srcErr)
	}
	if !rawStatusMode && handoffSource == "" && handoffStageRequiresSource(stageCfg.name) {
		return fatal(jsonOut, "--stage %s requires --source-provider <claude|codex|copilot|opencode> so cross-provider review/testing is deterministic", *stage)
	}

	dir, dErr := resolveWorktreeDir(*wtDir)
	if dErr != nil {
		return fatal(jsonOut, "%v", dErr)
	}

	planContent, planErr := resolveHandoffPlan(*plan, *planFile)
	if planErr != nil {
		return fatal(jsonOut, "%v", planErr)
	}

	projectID, projRec, projErr := resolveHandoffProject(ps, dir, *proj)
	if projErr != nil {
		return fatal(jsonOut, "%v", projErr)
	}

	tags := append([]string{}, stageCfg.tags...)
	tags = append(tags, parseExtraTags(*extraTags, tags)...)
	init := task.Update{
		ProjectID: task.Ptr(projectID),
		Tags:      &tags,
	}
	if planContent != "" {
		init.Plan = &planContent
	}
	if handoffSource != "" {
		init.HandoffSourceProvider = &handoffSource
	}
	if rawStatusMode {
		init.Status = &status
	}
	// Handoff always adopts the current worktree and routes through the internal
	// Sybra task pipeline. It must never create the inbound PR-review lane's
	// `review` task shape, because that represents external PRs awaiting human
	// review rather than Sybra-authored work.
	if wtProj, e := deriveProjectID(dir); e == nil && !strings.EqualFold(wtProj, projectID) {
		return fatal(jsonOut, "worktree origin is %q but --project is %q — refusing to push agent work to a different repo", wtProj, projectID)
	}
	if e := assertFeatureBranch(dir, projRec); e != nil {
		return fatal(jsonOut, "%v", e)
	}
	init.WorktreeDir = task.Ptr(dir)
	if *pr > 0 {
		init.PRNumber = task.Ptr(*pr)
	}

	t, err := s.CreateFull(*title, *body, *mode, init)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(t)
	}
	if rawStatusMode {
		printHandoffStatusResult(t, status, projectID, dir)
	} else {
		printHandoffResult(t, stageCfg.name, projectID, dir)
	}
	return 0
}

func resolveHandoffMode(fs *flag.FlagSet, stage, rawStatus string, pr int) (handoffStageConfig, task.Status, bool, error) {
	stageProvided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "stage" {
			stageProvided = true
		}
	})
	if rawStatus != "" && stageProvided {
		return handoffStageConfig{}, "", false, fmt.Errorf("--status is raw board placement and cannot be combined with --stage")
	}
	if rawStatus != "" {
		status, err := task.ValidateStatus(rawStatus)
		if err != nil {
			return handoffStageConfig{}, "", false, err
		}
		return handoffStageConfig{name: "manual", tags: []string{handoffManualTag}}, status, true, nil
	}

	stageCfg, ok := handoffStageConfigFor(stage)
	if !ok {
		if isExternalPRHandoffStage(stage) {
			return handoffStageConfig{}, "", false, fmt.Errorf("--stage %s is not supported by handoff: handoff only creates internal Sybra tasks; use --stage ready-pr --pr N to link an existing PR from this worktree", stage)
		}
		return handoffStageConfig{}, "", false, handoffStageUsageError(stage)
	}
	if pr > 0 && stageCfg.name != "ready-pr" {
		return handoffStageConfig{}, "", false, fmt.Errorf("--pr is only valid with --stage ready-pr so the PR stays linked to an internal Sybra task")
	}
	return stageCfg, "", false, nil
}

func resolveHandoffPlan(plan, planFile string) (string, error) {
	if planFile == "" {
		return plan, nil
	}
	data, err := os.ReadFile(planFile)
	if err != nil {
		return "", fmt.Errorf("read plan file: %w", err)
	}
	return string(data), nil
}

func resolveHandoffProject(ps *project.Store, dir, projectID string) (string, project.Project, error) {
	if projectID == "" {
		derived, err := deriveProjectID(dir)
		if err != nil {
			return "", project.Project{}, fmt.Errorf("derive project from %q: %w (pass --project owner/repo)", dir, err)
		}
		projectID = derived
	}
	projRec, err := ps.Get(projectID)
	if err != nil {
		return "", project.Project{}, fmt.Errorf("project %q not registered: %w (run: sybra-cli project create --url <github-url>)", projectID, err)
	}
	return projectID, projRec, nil
}

type handoffStageConfig struct {
	name string
	tags []string
}

const handoffManualTag = "handoff-manual"

// handoffStageDef is one entry in the handoff-stage registry: a canonical
// name, its input aliases, and the tags that route a task into the matching
// Sybra lane (simple-task-handoff-<name>.yaml) on creation. Adding a stage
// is a data edit here — handoffStageConfigFor, its error message, and the
// --stage usage text all derive from this slice, so nothing else needs to
// change to keep a new stage reachable.
type handoffStageDef struct {
	name           string
	aliases        []string
	tags           []string
	usage          string
	requiresSource bool
}

var handoffStageRegistry = []handoffStageDef{
	// implement: simple-task-handoff → in-progress → implement → review → testing → PR
	{name: "implement", aliases: []string{"", "in-progress"}, tags: []string{"handoff"},
		usage: "have a plan -> Sybra implements, reviews, tests, opens the PR"},
	// review: simple-task-handoff-review → ready-review → review → testing → PR
	{name: "review", aliases: []string{"ready-review", "agentic-review"}, tags: []string{"handoff", "handoff-review"},
		usage: "implemented locally -> Sybra enters agentic review", requiresSource: true},
	// testing: simple-task-handoff-testing → testing → adversarial test → PR
	{name: "testing", aliases: []string{"test"}, tags: []string{"handoff", "handoff-testing"},
		usage: "reviewed locally -> Sybra tests, then opens the PR", requiresSource: true},
	// ready-pr: simple-task-handoff-ready-pr → ready-pr → open/update PR
	{name: "ready-pr", aliases: []string{"open-pr", "create-pr"}, tags: []string{"handoff", "handoff-ready-pr"},
		usage: "tested locally -> Sybra opens or updates the PR; pass --pr N\n" +
			strings.Repeat(" ", 19) + "only to link an existing same-branch PR"},
}

// handoffStageConfigFor maps a handoff stage (or one of its aliases) to the
// tags that route the task into the right Sybra lane on creation, or false
// for an unknown stage. See handoffStageRegistry for the stage definitions.
func handoffStageConfigFor(stage string) (handoffStageConfig, bool) {
	normalized := strings.ToLower(strings.TrimSpace(stage))
	for _, def := range handoffStageRegistry {
		if normalized == def.name || slices.Contains(def.aliases, normalized) {
			return handoffStageConfig{name: def.name, tags: def.tags}, true
		}
	}
	return handoffStageConfig{}, false
}

// handoffStageUsageError renders the "invalid --stage" error from the same
// registry that drives handoffStageConfigFor, so the valid-stage/alias list
// can never drift from what the CLI actually accepts.
func handoffStageUsageError(stage string) error {
	names := make([]string, 0, len(handoffStageRegistry))
	var aliases []string
	for _, def := range handoffStageRegistry {
		names = append(names, def.name)
		for _, a := range def.aliases {
			if a != "" {
				aliases = append(aliases, a)
			}
		}
	}
	return fmt.Errorf("invalid --stage %q (valid: %s; aliases: %s)", stage, strings.Join(names, ", "), strings.Join(aliases, ", "))
}

func isExternalPRHandoffStage(stage string) bool {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "pr", "in-review", "pull-request", "pull_request":
		return true
	default:
		return false
	}
}

func handoffStageRequiresSource(stage string) bool {
	for _, def := range handoffStageRegistry {
		if def.name == stage {
			return def.requiresSource
		}
	}
	return false
}

// handoffStageCompactList renders the "implement|review|testing|ready-pr"
// summary used in the --stage flag help, derived from handoffStageRegistry.
func handoffStageCompactList() string {
	names := make([]string, len(handoffStageRegistry))
	for i, def := range handoffStageRegistry {
		names[i] = def.name
	}
	return strings.Join(names, "|")
}

// handoffStageUsageLines renders one "  name  usage" row per registry stage
// for the handoff command's long usage text.
func handoffStageUsageLines() string {
	var b strings.Builder
	for _, def := range handoffStageRegistry {
		fmt.Fprintf(&b, "    %-14s %s\n", def.name, def.usage)
	}
	return strings.TrimRight(b.String(), "\n")
}

// handoffStageSourceRequirementList renders the "|"-joined list of stages
// that require --source-provider, derived from handoffStageRegistry.
func handoffStageSourceRequirementList() string {
	var required []string
	for _, def := range handoffStageRegistry {
		if def.requiresSource {
			required = append(required, def.name)
		}
	}
	return strings.Join(required, "|")
}

func normalizeHandoffSourceProvider(raw string) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(raw))
	switch provider {
	case "none", "clear":
		provider = ""
	}
	if _, err := task.ValidateAgentProvider(provider); err != nil {
		return "", err
	}
	return provider, nil
}

// resolveWorktreeDir resolves the handoff worktree (default: cwd) to an
// absolute path and verifies it is an existing directory.
func resolveWorktreeDir(wtDir string) (string, error) {
	dir := wtDir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working dir: %w", err)
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve worktree dir: %w", err)
	}
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		return "", fmt.Errorf("worktree dir %q is not a directory", abs)
	}
	return abs, nil
}

// assertFeatureBranch fails when the worktree is in detached HEAD (no branch to
// push) or on the repo's default branch (would push agent commits to origin's
// default branch with no PR).
func assertFeatureBranch(dir string, proj project.Project) error {
	branch, err := project.CurrentBranch(context.Background(), dir)
	if err != nil {
		return fmt.Errorf("resolve worktree branch: %w", err)
	}
	if branch == "" {
		return fmt.Errorf("worktree %q is in detached HEAD — check out a feature branch before handoff", dir)
	}
	// Fail closed: if the default branch can't be determined we can't prove the
	// worktree isn't on it, so refuse rather than risk pushing agent commits to
	// the default branch with no PR.
	def, dErr := project.DefaultBranch(context.Background(), proj.ClonePath)
	if dErr != nil {
		return fmt.Errorf("cannot determine default branch for %q: %w", proj.ID, dErr)
	}
	if branch == def {
		return fmt.Errorf("worktree %q is on the default branch %q — create a feature branch before handoff", dir, def)
	}
	return nil
}

// parseExtraTags splits a comma-separated tag list, trimming blanks and any tag
// already present in exclude (the stage tags).
func parseExtraTags(extra string, exclude []string) []string {
	var out []string
	for raw := range strings.SplitSeq(extra, ",") {
		tg := strings.TrimSpace(raw)
		if tg == "" || slices.Contains(exclude, tg) {
			continue
		}
		out = append(out, tg)
	}
	return out
}

func printHandoffResult(t task.Task, stage, projectID, dir string) {
	fmt.Printf("Handed off task %s: %s\n", t.ID, t.Title)
	fmt.Printf("  project:  %s\n", projectID)
	if t.HandoffSourceProvider != "" {
		fmt.Printf("  source:   %s\n", t.HandoffSourceProvider)
	}
	switch stage {
	case "review":
		fmt.Printf("  worktree: %s\n", dir)
		fmt.Println("  Sybra will skip to review and open the PR from this worktree.")
	case "testing":
		fmt.Printf("  worktree: %s\n", dir)
		fmt.Println("  Sybra will skip straight to adversarial testing of this worktree.")
	case "ready-pr":
		fmt.Printf("  worktree: %s\n", dir)
		fmt.Println("  Sybra will skip straight to opening or updating the PR from this worktree.")
	default:
		fmt.Printf("  worktree: %s\n", dir)
		fmt.Println("  Sybra will skip planning and start implementing in this worktree.")
	}
}

func printHandoffStatusResult(t task.Task, status task.Status, projectID, dir string) {
	fmt.Printf("Handed off task %s: %s\n", t.ID, t.Title)
	fmt.Printf("  project:  %s\n", projectID)
	fmt.Printf("  status:   %s\n", status)
	fmt.Printf("  worktree: %s\n", dir)
	fmt.Println("  Sybra created the task in that status without starting a workflow.")
}

// deriveProjectID reads the origin remote of a git worktree and converts it to
// a Sybra project id (owner/repo).
func deriveProjectID(dir string) (string, error) {
	out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	owner, repo, err := project.ParseGitHubURL(strings.TrimSpace(string(out)))
	if err != nil {
		return "", err
	}
	return owner + "/" + repo, nil
}

// cmdInstallSkills mirrors Sybra's bundled skills into the per-agent skill
// directories (~/.claude/skills, ~/.codex/skills, ~/.agents/skills) plus the
// app's skills dir, so the skills are available in interactive Claude Code and
// Codex sessions in any repo. Reuses the same skillsync.Syncer the app runs at
// startup, so behaviour (frontmatter validation, orphan pruning) is identical.
func cmdInstallSkills(cfg *config.Config, jsonOut bool) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return fatal(jsonOut, "resolve home dir: %v", err)
	}

	// Resolve the skill source. Prefer a configured RepoDir; otherwise fall
	// back to cwd ONLY when it is genuinely the sybra source repo (matching the
	// app's launched-from-repo behaviour, which provides the richer dir-based
	// skills). When cwd is some other repo, point at a path without a go.mod so
	// the syncer uses the embedded bundle instead of installing the wrong
	// repo's skills.
	repoDir := cfg.RepoDir
	if repoDir == "" {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil && isSybraRepo(cwd) {
			repoDir = cwd
		} else {
			repoDir = config.HomeDir()
		}
	}

	var logger *slog.Logger
	if !jsonOut {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	(&skillsync.Syncer{Logger: logger}).Run(skillsync.Options{
		RepoDir:              repoDir,
		SkillsFS:             skills.FS,
		PrimaryDst:           cfg.SkillsDir,
		SybraHomeDir:         config.HomeDir(),
		UserHomeDir:          home,
		DowngradeCommitFlags: !project.GPGSigningAvailable(context.Background()),
	})

	dsts := []string{
		cfg.SkillsDir,
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".codex", "skills"),
		filepath.Join(home, ".copilot", "skills"),
		filepath.Join(home, ".agents", "skills"),
	}
	if jsonOut {
		return printJSON(map[string]any{"status": "ok", "destinations": dsts})
	}
	fmt.Println("Installed Sybra skills to:")
	for _, d := range dsts {
		fmt.Printf("  %s\n", d)
	}
	return 0
}

// isSybraRepo reports whether dir is the sybra source checkout, by matching the
// module path in its go.mod. Used to gate the cwd fallback in install-skills so
// it never installs an unrelated repo's skills.
func isSybraRepo(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "module github.com/Automaat/sybra")
}

// findActiveDuplicate looks for an existing non-terminal task that matches
// project + issue + title. Used to suppress double-dispatch from the
// orchestrator brain (which calls `sybra-cli create` externally and has no
// view of the in-flight task list when a previous dispatch is still running).
//
// Match requires all three fields to be non-empty and equal, so it cannot
// collapse legitimate distinct subtasks of an umbrella issue (those have
// different titles).
func findActiveDuplicate(s *task.Manager, projectID, issue, title string) (task.Task, bool, error) {
	if projectID == "" || issue == "" || title == "" {
		return task.Task{}, false, nil
	}
	tasks, err := s.List()
	if err != nil {
		return task.Task{}, false, err
	}
	for i := range tasks {
		if task.IsTerminalStatus(tasks[i].Status) {
			continue
		}
		if tasks[i].ProjectID == projectID && tasks[i].Issue == issue && tasks[i].Title == title {
			return tasks[i], true, nil
		}
	}
	return task.Task{}, false, nil
}

func cmdUpdate(s *task.Manager, api *apiClient, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: update <id> [flags]")
	}

	id := args[0]
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	flags := newUpdateFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	updates, uErr := buildUpdateMap(fs, flags)
	if uErr != nil {
		return fatal(jsonOut, "%v", uErr)
	}

	if len(updates) == 0 {
		return fatal(jsonOut, "no updates specified")
	}

	t, err := updateTaskViaAPIOrFS(s, api, id, updates)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(t)
	}
	fmt.Printf("Updated task %s\n", t.ID)
	return 0
}

type updateFlags struct {
	title             *string
	status            *string
	body              *string
	plan              *string
	planFile          *string
	planContract      *string
	planContractFile  *string
	planCritique      *string
	planCritiqueFile  *string
	planResearch      *string
	planResearchFile  *string
	planDecisions     *string
	planDecisionsFile *string
	planBrief         *string
	planBriefFile     *string
	codeReview        *string
	codeReviewFile    *string
	mode              *string
	taskType          *string
	tags              *string
	project           *string
	branch            *string
	pr                *int
	issue             *string
	sourceProvider    *string
	statusReason      *string
	maxTurns          *int
	reasoningEffort   *string
}

func newUpdateFlags(fs *flag.FlagSet) updateFlags {
	return updateFlags{
		title:             fs.String("title", "", "new title"),
		status:            fs.String("status", "", "new status"),
		body:              fs.String("body", "", "new body"),
		plan:              fs.String("plan", "", "plan content markdown (empty string clears plan)"),
		planFile:          fs.String("plan-file", "", "path to file with plan content"),
		planContract:      fs.String("plan-contract", "", "executable plan contract JSON (empty string clears contract)"),
		planContractFile:  fs.String("plan-contract-file", "", "path to file with executable plan contract JSON"),
		planCritique:      fs.String("plan-critique", "", "plan critique markdown (empty string clears critique)"),
		planCritiqueFile:  fs.String("plan-critique-file", "", "path to file with plan critique content"),
		planResearch:      fs.String("plan-research", "", "plan research markdown (empty string clears research)"),
		planResearchFile:  fs.String("plan-research-file", "", "path to file with plan research content"),
		planDecisions:     fs.String("plan-decisions", "", "plan decisions markdown (empty string clears decisions)"),
		planDecisionsFile: fs.String("plan-decisions-file", "", "path to file with plan decisions content"),
		planBrief:         fs.String("plan-brief", "", "plan brief markdown (empty string clears brief)"),
		planBriefFile:     fs.String("plan-brief-file", "", "path to file with plan brief content"),
		codeReview:        fs.String("code-review", "", "code review markdown (empty string clears review)"),
		codeReviewFile:    fs.String("code-review-file", "", "path to file with code review content"),
		mode:              fs.String("mode", "", "new agent mode"),
		taskType:          fs.String("type", "", "new task type: normal|debug|research"),
		tags:              fs.String("tags", "", "comma-separated tags (replaces existing)"),
		project:           fs.String("project", "", "project id (owner/repo)"),
		branch:            fs.String("branch", "", "Git branch name"),
		pr:                fs.Int("pr", 0, "GitHub PR number"),
		issue:             fs.String("issue", "", "ad-hoc reference issue URL annotation — does not affect PR auto-close linkage (see task.Issue, set only at creation)"),
		sourceProvider:    fs.String("source-provider", "", "handoff source provider: claude|codex|copilot|opencode|none"),
		statusReason:      fs.String("status-reason", "", "reason for status change"),
		maxTurns:          fs.Int("max-turns", -1, "per-task max turns override (0 clears override, >0 sets limit)"),
		reasoningEffort:   fs.String("reasoning-effort", "", "reasoning effort (all providers): low|medium|high|xhigh ('default' or 'none' clears the override)"),
	}
}

func buildUpdateMap(fs *flag.FlagSet, f updateFlags) (map[string]any, error) {
	updates := map[string]any{}
	applyBasicUpdateFlags(updates, f)
	if err := applySidecarUpdateFlags(fs, updates, f); err != nil {
		return nil, err
	}
	if err := applyTypedUpdateFlags(fs, updates, f); err != nil {
		return nil, err
	}
	return updates, nil
}

func applyBasicUpdateFlags(updates map[string]any, f updateFlags) {
	if *f.title != "" {
		updates["title"] = *f.title
	}
	if *f.status != "" {
		updates["status"] = *f.status
	}
	if *f.statusReason != "" {
		updates["status_reason"] = *f.statusReason
	}
	if *f.body != "" {
		updates["body"] = *f.body
	}
	if *f.mode != "" {
		updates["agent_mode"] = *f.mode
	}
	if *f.tags != "" {
		updates["tags"] = parseTags(*f.tags)
	}
	if *f.project != "" {
		updates["project_id"] = *f.project
	}
	if *f.branch != "" {
		updates["branch"] = *f.branch
	}
	if *f.pr > 0 {
		updates["pr_number"] = float64(*f.pr)
	}
	if *f.issue != "" {
		updates["ref_issue"] = *f.issue
	}
}

func applySidecarUpdateFlags(fs *flag.FlagSet, updates map[string]any, f updateFlags) error {
	for _, fu := range []struct{ flag, key, str, file string }{
		{"plan", "plan", *f.plan, *f.planFile},
		{"plan-contract", "plan_contract", *f.planContract, *f.planContractFile},
		{"plan-critique", "plan_critique", *f.planCritique, *f.planCritiqueFile},
		{"plan-research", "plan_research", *f.planResearch, *f.planResearchFile},
		{"plan-decisions", "plan_decisions", *f.planDecisions, *f.planDecisionsFile},
		{"plan-brief", "plan_brief", *f.planBrief, *f.planBriefFile},
		{"code-review", "code_review", *f.codeReview, *f.codeReviewFile},
	} {
		if err := applyFileOrStringUpdate(fs, updates, fu.flag, fu.key, fu.str, fu.file); err != nil {
			return err
		}
	}
	return nil
}

func applyTypedUpdateFlags(fs *flag.FlagSet, updates map[string]any, f updateFlags) error {
	if *f.taskType != "" {
		if _, err := task.ValidateTaskType(*f.taskType); err != nil {
			return err
		}
		updates["task_type"] = *f.taskType
	}
	if flagWasProvided(fs, "source-provider") {
		v, err := normalizeHandoffSourceProvider(*f.sourceProvider)
		if err != nil {
			return err
		}
		updates["handoff_source_provider"] = v
	}
	if *f.maxTurns >= 0 {
		updates["max_turns"] = float64(*f.maxTurns)
	}
	if *f.reasoningEffort != "" {
		v, err := normalizeReasoningEffort(*f.reasoningEffort)
		if err != nil {
			return err
		}
		updates["reasoning_effort"] = v
	}
	return nil
}

func cmdDelete(s *task.Manager, api *apiClient, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: delete <id>")
	}

	if _, handled, apiErr := viaAPI[struct{}](api, "TaskService", "DeleteTask", args[0]); handled {
		if apiErr != nil {
			return fatal(jsonOut, "%v", apiErr)
		}
	} else if fsErr := s.Delete(args[0]); fsErr != nil {
		return fatal(jsonOut, "%v", fsErr)
	}

	if jsonOut {
		return printJSON(map[string]string{"deleted": args[0]})
	}
	fmt.Printf("Deleted task %s\n", args[0])
	return 0
}

func cmdReopen(s *task.Manager, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("reopen", flag.ContinueOnError)
	force := fs.Bool("force", false, "reopen even if the task landed (outcome merged)")
	projectID := fs.String("project", "", "restore project_id (for tasks whose project link was lost)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	ids := fs.Args()
	if len(ids) == 0 {
		return fatal(jsonOut, "usage: reopen [--force] [--project owner/repo] <id>...")
	}
	reopened := make([]string, 0, len(ids))
	for _, id := range ids {
		t, err := s.Get(id)
		if err != nil {
			return fatal(jsonOut, "%v", err)
		}
		if !*force && (t.Outcome == "merged" || t.Outcome == "merged_with_edits") {
			return fatal(jsonOut, "task %s landed (outcome=%s); pass --force to reopen anyway", id, t.Outcome)
		}
		u := task.Update{
			Status:       task.Ptr(task.StatusTodo),
			Workflow:     task.Ptr[*workflow.Execution](nil),
			WorktreeDir:  task.Ptr(""),
			StatusReason: task.Ptr(""),
			Outcome:      task.Ptr(""),
		}
		if *projectID != "" {
			u.ProjectID = task.Ptr(*projectID)
		}
		if _, err := s.Update(id, u); err != nil {
			return fatal(jsonOut, "%v", err)
		}
		reopened = append(reopened, id)
	}
	if jsonOut {
		return printJSON(map[string]any{"reopened": reopened})
	}
	fmt.Printf("Reopened %d task(s): %s\n", len(reopened), strings.Join(reopened, ", "))
	return 0
}

// cmdLinkPR links a GitHub PR number to a task and advances non-terminal tasks
// to in-review so the PR monitor loop can take over (auto-merge / done on
// merge). Use when a PR was opened outside of Sybra (manually or by an external
// tool) and the task's pr_number is still 0.
func cmdLinkPR(s *task.Manager, api *apiClient, args []string, jsonOut bool) int {
	if len(args) < 2 {
		return fatal(jsonOut, "usage: link-pr <task-id> <pr-number>")
	}
	id := args[0]
	prNum, err := strconv.Atoi(args[1])
	if err != nil || prNum <= 0 {
		return fatal(jsonOut, "pr-number must be a positive integer, got %q", args[1])
	}

	t, err := s.Get(id)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	advanceToReview := !task.IsTerminalStatus(t.Status) && t.Status != task.StatusInReview
	updates := map[string]any{"pr_number": float64(prNum)}
	if advanceToReview {
		updates["status"] = string(task.StatusInReview)
		updates["status_reason"] = ""
	}

	t, err = updateTaskViaAPIOrFS(s, api, id, updates)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(t)
	}
	if advanceToReview {
		fmt.Printf("linked PR #%d to task %s → in-review\n", prNum, t.ID)
	} else {
		fmt.Printf("linked PR #%d to task %s (status: %s)\n", prNum, t.ID, t.Status)
	}
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

func flagWasProvided(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func parseTags(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// normalizeReasoningEffort converts "default"/"none" to "" (use model default) then validates.
func normalizeReasoningEffort(s string) (string, error) {
	if s == "default" || s == "none" {
		s = ""
	}
	return task.ValidateReasoningEffort(s)
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
		for i := range slices.Backward(t.AgentRuns) {
			run := &t.AgentRuns[i]
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
	if report.Docker != nil {
		fmt.Println()
		printDockerBlock(*report.Docker)
	}
	if report.Processes == nil {
		return 0
	}
	fmt.Println()
	if !report.Processes.Available {
		fmt.Println("Processes: unavailable")
		return 0
	}
	printProcessBlock("Top CPU", report.Processes.TopCPU)
	fmt.Println()
	printProcessBlock("Top Memory", report.Processes.TopMem)
	return 0
}

// healthReport mirrors the JSON structure without importing the health package.
type healthReport struct {
	GeneratedAt string                 `json:"generatedAt"`
	PeriodStart string                 `json:"periodStart"`
	PeriodEnd   string                 `json:"periodEnd"`
	Score       string                 `json:"score"`
	Findings    []json.RawMessage      `json:"findings"`
	Stats       json.RawMessage        `json:"stats"`
	Docker      *healthDockerDiskUsage `json:"docker,omitempty"`
	Processes   *healthProcessSummary  `json:"processes,omitempty"`
}

type healthDockerDiskUsage struct {
	Available        bool   `json:"available"`
	ReclaimableBytes int64  `json:"reclaimableBytes"`
	TotalBytes       int64  `json:"totalBytes,omitempty"`
	ManualCommand    string `json:"manualCommand,omitempty"`
	SampledAt        string `json:"sampledAt"`
}

type healthProcessSummary struct {
	TopCPU    []healthProcess `json:"topCpu"`
	TopMem    []healthProcess `json:"topMem"`
	SampledAt string          `json:"sampledAt"`
	Available bool            `json:"available"`
}

type healthProcess struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpuPercent"`
	MemPercent float64 `json:"memPercent"`
	Owned      bool    `json:"owned"`
}

func printProcessBlock(title string, processes []healthProcess) {
	fmt.Printf("%s:\n", title)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "OWNER\tPID\tNAME\tCPU%\tMEM%")
	for _, p := range processes {
		owner := "[ext]"
		if p.Owned {
			owner = "[sybra]"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%.1f\t%.1f\n", owner, p.PID, p.Name, p.CPUPercent, p.MemPercent)
	}
	_ = w.Flush()
}

func printDockerBlock(docker healthDockerDiskUsage) {
	fmt.Println("Docker:")
	if !docker.Available {
		fmt.Println("  unavailable")
		return
	}
	fmt.Printf("  Reclaimable: %s\n", humanBytes(docker.ReclaimableBytes))
	if docker.TotalBytes > 0 {
		fmt.Printf("  Total: %s\n", humanBytes(docker.TotalBytes))
	}
	if docker.ManualCommand != "" {
		fmt.Printf("  Manual cleanup: %s\n", docker.ManualCommand)
	}
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

// cmdHook is the fast-path handler for "sybra-cli hook <Event> --task <id>".
// It is invoked by codex lifecycle hooks — once per event, as a short-lived
// subprocess. Only config.Load() is needed (no task/project stores). Always
// exits 0 (fail-open): hook errors must never stall a codex agent run.
func cmdHook(cfg *config.Config, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "hook: missing event name")
		return 0
	}
	event := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	taskID := fs.String("task", "", "task id")
	if err := fs.Parse(rest); err != nil {
		return 0 // flag already wrote to stderr
	}

	if *taskID == "" {
		fmt.Fprintln(os.Stderr, "hook: --task required")
		return 0
	}
	if !hookTaskIDRe.MatchString(*taskID) {
		fmt.Fprintln(os.Stderr, "hook: invalid --task value")
		return 0
	}
	// Read the hook payload from stdin (codex pipes JSON here).
	// Read up to 64 KiB + 1: if more than 64 KiB are available, the extra
	// byte will be read and len(payload) > maxPayloadBytes, signalling
	// overflow without silently truncating a valid exactly-64-KiB payload.
	const maxPayloadBytes = 64 * 1024
	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maxPayloadBytes+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook: read stdin: %v\n", err)
		logHookFailure(cfg, *taskID, "read_error")
		return 0
	}
	if len(payload) > maxPayloadBytes {
		fmt.Fprintln(os.Stderr, "hook: stdin payload exceeds size limit")
		logHookFailure(cfg, *taskID, "oversized_payload")
		return 0
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		fmt.Fprintln(os.Stderr, "hook: empty stdin payload")
		logHookFailure(cfg, *taskID, "empty_payload")
		return 0
	}

	auditEvent, err := codexhook.Map(payload, *taskID, event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook: map payload: %v\n", err)
		logHookFailure(cfg, *taskID, "map_error")
		return 0
	}

	logger, err := audit.NewLogger(cfg.AuditDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook: create audit logger: %v\n", err)
		return 0
	}
	defer func() { _ = logger.Close() }()

	if err := logger.Log(auditEvent); err != nil {
		fmt.Fprintf(os.Stderr, "hook: log: %v\n", err)
		_ = logger.Log(audit.Event{
			Timestamp: time.Now().UTC(),
			Type:      audit.EventCodexHookFailed,
			TaskID:    *taskID,
			Data:      map[string]any{"reason": "log_error"},
		})
	}
	return 0
}

// logHookFailure writes a diagnostic audit event for hook receiver errors.
// It is best-effort: all errors are silently discarded so the caller always
// exits 0 and the hook stays fail-open. The reason field is a categorical
// label — never the raw error message — so no sensitive content is persisted.
func logHookFailure(cfg *config.Config, taskID, reason string) {
	logger, err := audit.NewLogger(cfg.AuditDir())
	if err != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.Log(audit.Event{
		Timestamp: time.Now().UTC(),
		Type:      audit.EventCodexHookFailed,
		TaskID:    taskID,
		Data:      map[string]any{"reason": reason},
	})
}

// statusListForUsage renders task.AllStatuses() as a pipe-joined string so
// the CLI help text can never drift from the enum's single source of truth.
func statusListForUsage() string {
	statuses := task.AllStatuses()
	names := make([]string, len(statuses))
	for i, s := range statuses {
		names[i] = string(s)
	}
	return strings.Join(names, "|")
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: sybra-cli [--json] [--home DIR] <command> [flags]

--home DIR overrides which Sybra home this invocation reads/writes, taking
precedence over SYBRA_CONTROL_HOME and SYBRA_HOME. Inside a task-scoped
agent, bare sybra-cli reaches the real operator board via SYBRA_CONTROL_HOME;
pass --home "$SYBRA_HOME" to inspect the agent's own sandbox/app-under-test
store instead.

Commands:
  list     [--status STATUS] [--tag TAG] [--project ID]
           STATUS: %s
  get      [--compact] <id>
  create   --title TITLE [--body BODY] [--plan PLAN] [--plan-contract JSON] [--mode MODE] [--type TYPE] [--tags t1,t2] [--project ID] [--branch B] [--pr N] [--issue URL] [--allow-dup]
           TYPE: normal|debug|research
  handoff  --title TITLE [--body BODY] [--plan PLAN | --plan-file PATH] [--project ID] [--worktree-dir DIR] [--stage STAGE | --status STATUS] [--source-provider claude|codex|copilot|opencode] [--pr N] [--mode MODE] [--tags t1,t2]
           Hand a task to Sybra at a workflow entry point, reusing the given git worktree
           (default: cwd). Project is derived from the worktree's origin remote
           when --project is omitted. STAGE (default implement):
%s
           --source-provider records which local agent produced handed-off work
           so review/testing/PR steps can run on a different provider.
           Required for --stage %s; optional for the other stages.
           --status STATUS creates the task directly in that status without workflow dispatch
  umbrella <issue-url> [--model M]
           Expand a GitHub umbrella issue into a gated task DAG: one umbrella tracker
           plus one blocked child per sub-issue, with dependency edges extracted by an
           LLM planner. Re-running only materializes sub-issues without an existing task.
  update   <id> [--title T] [--status S] [--status-reason R] [--body B] [--plan PLAN] [--plan-file PATH] [--plan-contract JSON|--plan-contract-file PATH] [--plan-research TEXT|--plan-research-file PATH] [--plan-decisions TEXT|--plan-decisions-file PATH] [--plan-brief TEXT|--plan-brief-file PATH] [--mode M] [--type TYPE] [--tags T] [--project ID] [--branch B] [--pr N] [--issue URL] [--source-provider P|none] [--max-turns N] [--reasoning-effort E]
           --issue sets ref_issue, an ad-hoc reference annotation — it never
           overwrites the task's canonical (auto-close) issue set at creation
  link-pr  <id> <pr-number>
           Link a PR number to a task and advance it to in-review. Use when a PR
           was opened outside of Sybra; the PR monitor will then auto-merge or
           advance the task to done once the PR lands.
  delete   <id>
           Soft-deletes: moves the task file and its sidecars into the trash
           dir instead of unlinking them. See trash list / trash restore.

  trash list
  trash restore <id>
           Restore the newest trashed generation for id back into the tasks
           dir. Refuses if a live task with that id already exists.
  trash delete <id>
           Permanently purge id's newest trashed generation right away,
           bypassing the retention window.
  trash empty
           Permanently purge every trashed generation, regardless of age.

  tasks-history [--limit N]
           List commits from the tasks-dir git snapshot repo (see
           internal/tasksnapshot). Recovery is a plain git checkout against
           that repo — see docs/tasks-snapshots.md.

  project list
  project get <id>
  project create --url <github-url> [--type pet|work]
  project update <id> --type pet|work
  project delete <id>

  cluster nodes
  cluster reassign <task-id> --node <name>   ("local" brings it back to the leader)
  cluster gen-cert --host <name> [--host ...] [--out DIR]
           self-signed follower cert + the tls_pin the leader must trust

  audit    [--since DURATION|DATE] [--until DATE] [--type TYPE] [--task ID] [--summary]
  board    (status counts + in-progress/plan-review/human-required task lists)
  monitor  scan [--json]    one-shot read-only detector pass (no remediation)
  evaluation scan [--json]  fleet scorecard (autonomy, throughput, efficiency)
  harness-evolution run [--lookback 168h] [--min-cluster-size 2] [--file] [--json]
           Cluster selfmonitor failures into governed harness-change proposals.
  prompt-lab run [--lookback 168h] [--min-samples 5] [--file] [--dry-run] [--json]
           Scaffold versioned prompt/skill variant proposals from fleet evidence.
  stats lifecycle [--since 30d] [--slowest N] [--json]
           Per-phase lead-time breakdown (planning/implementing/testing/review/
           waiting) for tasks that landed in the window — where time is spent.
  health   [--severity warning|critical] [--category CATEGORY]

  triage classify <id>         Classify a single task (must have status=new) via claude -p
                               and apply the verdict.
  triage classify --all        Classify every task with status=new.

  install-skills               Install/refresh Sybra's bundled skills into
                               ~/.claude/skills, ~/.codex/skills, ~/.copilot/skills,
                               ~/.agents/skills (and the app skills dir) for
                               Claude Code, Codex, and Copilot.

  artifact list <task-id>      List artifacts for a task.
  artifact get  <task-id> <name>  Print raw artifact bytes to stdout.
  artifact reindex <task-id>   Rebuild index.json from *.meta.json files.

  config dump                  Print the resolved ~/.sybra/config.yaml (env
                               overrides applied, secrets redacted).
  config doctor                Sanity-check config: data dirs, agent.provider,
                               agent.headless_permission_mode,
                               agent.sandbox_mode, and enabled integrations
                               missing required credentials.%s

Global flags:
  --json   Output as JSON
`, statusListForUsage(), handoffStageUsageLines(), handoffStageSourceRequirementList(), doctorUsageBlock())
}

func doctorUsageBlock() string {
	return `

  doctor cleanup [--apply] [--only b1,b2] [--worktrees] [--external] [--force]
                 [--older-than DURATION] [--json]
           Report (default: dry-run) or delete (--apply) reclaimable disk
           usage: logs, audit, sandboxes, go-build-cache (safe, cleaned by
           default under --apply) plus worktrees/shared-cache/external
           (destructive — each needs its own --worktrees/--external
           flag before --apply will touch it, or even include it in the
           report; --force only bypasses dirty worktree protection).
           Deletion is irreversible; dry-run first. Exit codes:
           0 ok, 1 a delete failed, 2 bad flags/arguments.`
}

func cmdArtifact(args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "artifact: subcommand required (list|get|reindex)")
	}
	sub, rest := args[0], args[1:]
	store := artifact.New(config.ArtifactsDir())
	switch sub {
	case "list":
		return cmdArtifactList(store, rest, jsonOut)
	case "get":
		return cmdArtifactGet(store, rest)
	case "reindex":
		return cmdArtifactReindex(store, rest, jsonOut)
	default:
		return fatal(jsonOut, "artifact: unknown subcommand %q", sub)
	}
}

func cmdArtifactList(store *artifact.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "artifact list: task-id required")
	}
	taskID := args[0]
	metas, err := store.List(taskID)
	if err != nil {
		return fatal(jsonOut, "artifact list: %v", err)
	}
	if jsonOut {
		return printJSON(metas)
	}
	if len(metas) == 0 {
		fmt.Println("(no artifacts)")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tSIZE\tCREATED")
	for i := range metas {
		m := &metas[i]
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", m.Name, m.Kind, m.Size, m.CreatedAt.Format(time.RFC3339))
	}
	_ = w.Flush()
	return 0
}

func cmdArtifactGet(store *artifact.Store, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "artifact get: task-id and name required")
		return 1
	}
	taskID, name := args[0], args[1]
	data, _, err := store.Read(taskID, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_, _ = os.Stdout.Write(data)
	return 0
}

func cmdArtifactReindex(store *artifact.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "artifact reindex: task-id required")
	}
	taskID := args[0]
	if err := store.Reindex(taskID); err != nil {
		return fatal(jsonOut, "artifact reindex: %v", err)
	}
	if jsonOut {
		return printJSON(map[string]string{"status": "ok", "task_id": taskID})
	}
	fmt.Printf("reindexed %s\n", taskID)
	return 0
}

func cmdProgress(s *task.Manager, projStore *project.Store, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "progress: subcommand required (add|list)")
	}
	sub, rest := args[0], args[1:]
	store := artifact.New(config.ArtifactsDir())
	switch sub {
	case "add":
		return cmdProgressAdd(s, projStore, store, rest, jsonOut)
	case "list":
		return cmdProgressList(store, rest, jsonOut)
	default:
		return fatal(jsonOut, "progress: unknown subcommand %q", sub)
	}
}

func cmdProgressAdd(s *task.Manager, projStore *project.Store, store *artifact.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "progress add: task-id required")
	}
	taskID := args[0]
	fs := flag.NewFlagSet("progress add", flag.ContinueOnError)
	kind := fs.String("kind", artifact.ProgressKindProgress, "entry kind: "+strings.Join(artifact.ProgressKinds(), "|"))
	message := fs.String("message", "", "progress message (required)")
	role := fs.String("role", "", "authoring agent role (optional)")
	if err := fs.Parse(args[1:]); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if strings.TrimSpace(*message) == "" {
		return fatal(jsonOut, "progress add: --message is required")
	}
	if !artifact.ValidProgressKind(*kind) {
		return fatal(jsonOut, "progress add: invalid --kind %q (want %s)", *kind, strings.Join(artifact.ProgressKinds(), "|"))
	}

	t, err := s.Get(taskID)
	if err != nil {
		return fatal(jsonOut, "progress add: %v", err)
	}

	msg := *message
	if t.ProjectID != "" && projStore != nil {
		if p, pErr := projStore.Get(t.ProjectID); pErr == nil {
			if bl := p.WorkBlocklist(); bl != nil {
				msg, _ = scrub.Scrub(msg, bl)
			}
		}
	}

	entry := artifact.ProgressEntry{Ts: time.Now().UTC(), Kind: *kind, Role: *role, Message: msg}
	if err := store.AppendProgress(taskID, entry); err != nil {
		return fatal(jsonOut, "progress add: %v", err)
	}
	if _, tErr := s.Touch(taskID); tErr != nil {
		slog.Warn("progress.add.touch", "task_id", taskID, "err", tErr)
	}

	if jsonOut {
		return printJSON(entry)
	}
	fmt.Printf("Recorded %s on task %s\n", *kind, taskID)
	return 0
}

func cmdProgressList(store *artifact.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "progress list: task-id required")
	}
	taskID := args[0]
	entries, err := store.ReadProgress(taskID)
	if err != nil {
		return fatal(jsonOut, "progress list: %v", err)
	}
	if jsonOut {
		return printJSON(entries)
	}
	if len(entries) == 0 {
		fmt.Println("(no progress entries)")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tKIND\tROLE\tMESSAGE")
	for i := range entries {
		e := &entries[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Ts.Format(time.RFC3339), e.Kind, e.Role, e.Message)
	}
	_ = w.Flush()
	return 0
}

// cmdTrash handles `sybra-cli trash list|restore <id>|delete <id>|empty` —
// recovery and permanent-purge for tasks soft-deleted by Store.Delete (see
// internal/task.Store's ListTrash/RestoreFromTrash/DeleteTrashedGeneration/
// PruneAllTrash).
func cmdTrash(s *task.Manager, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: trash <list|restore|delete|empty>")
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "list":
		return cmdTrashList(s, jsonOut)
	case "restore":
		return cmdTrashRestore(s, rest, jsonOut)
	case "delete":
		return cmdTrashDelete(s, rest, jsonOut)
	case "empty":
		return cmdTrashEmpty(s, jsonOut)
	default:
		return fatal(jsonOut, "unknown trash command: %s", sub)
	}
}

func cmdTrashList(s *task.Manager, jsonOut bool) int {
	entries, err := s.ListTrash()
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		if entries == nil {
			entries = []task.TrashEntry{}
		}
		return printJSON(entries)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tDELETED\tGENERATION\tTITLE")
	for i := range entries {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", entries[i].ID, entries[i].DeletedDate, entries[i].Generation, entries[i].Title)
	}
	_ = w.Flush()
	return 0
}

func cmdTrashRestore(s *task.Manager, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: trash restore <id>")
	}
	t, err := s.RestoreFromTrash(args[0])
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(t)
	}
	fmt.Printf("Restored task %s\n", t.ID)
	return 0
}

type trashPruneReportJSON struct {
	Scanned int               `json:"scanned"`
	Removed int               `json:"removed"`
	Entries []task.TrashEntry `json:"entries"`
	Errors  []string          `json:"errors"`
}

func newTrashPruneReportJSON(rep task.TrashPruneReport) trashPruneReportJSON {
	out := trashPruneReportJSON{
		Scanned: rep.Scanned,
		Removed: rep.Removed,
		Entries: rep.Entries,
	}
	if len(rep.Errors) > 0 {
		out.Errors = make([]string, 0, len(rep.Errors))
		for _, err := range rep.Errors {
			out.Errors = append(out.Errors, err.Error())
		}
	}
	return out
}

func trashDeleteMessage(id string, removed bool) string {
	if removed {
		return fmt.Sprintf("Purged trashed task %s\n", id)
	}
	return fmt.Sprintf("Trashed task %s was already purged\n", id)
}

// cmdTrashDelete permanently purges id's newest trashed generation right
// away, bypassing the retention window — for a compliance request or a
// leaked credential that needs the content gone now, not after
// RetentionDays.
func cmdTrashDelete(s *task.Manager, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: trash delete <id>")
	}
	removed, err := s.DeleteTrashedGeneration(args[0])
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(map[string]any{"status": "ok", "id": args[0], "removed": removed})
	}
	fmt.Print(trashDeleteMessage(args[0], removed))
	return 0
}

// cmdTrashEmpty permanently purges every trashed generation, regardless of
// age.
func cmdTrashEmpty(s *task.Manager, jsonOut bool) int {
	rep, err := s.PruneAllTrash()
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(newTrashPruneReportJSON(rep))
	}
	fmt.Printf("Purged %d/%d trashed generations\n", rep.Removed, rep.Scanned)
	return 0
}

// taskHistoryEntry is one commit from the tasks-dir git snapshot repo (see
// internal/tasksnapshot).
type taskHistoryEntry struct {
	SHA     string `json:"sha"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// cmdTasksHistory lists commits from the tasks-dir git snapshot repo — a
// read-only convenience wrapper around `git log` against
// config.TaskSnapshotGitDir(); plain git against that path suffices for
// actual recovery (see docs/tasks-snapshots.md).
func cmdTasksHistory(cfg *config.Config, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("tasks-history", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "max number of commits to show")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "usage: tasks-history [--limit N]")
	}
	if *limit <= 0 {
		*limit = 20
	}

	ctx := context.Background()
	gitDir := config.TaskSnapshotGitDir()
	// Reuse the snapshotter's env builder so an inherited GIT_WORK_TREE can't
	// leak in and break git commands; the work-tree value itself is unused by
	// the read-only commands below but must be set consistently.
	env := tasksnapshot.BuildEnv(gitDir, cfg.TasksDir)

	verify := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	verify.Env = env
	if err := verify.Run(); err != nil {
		return fatal(jsonOut, "tasks snapshot history unavailable — snapshotting is disabled or has not run yet (%v)", err)
	}

	// Detect an empty repo by HEAD resolvability, not a locale-dependent
	// stderr string: `rev-parse --verify --quiet HEAD` exits non-zero with no
	// output when no commits exist yet, which is a valid empty history.
	head := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", "HEAD")
	head.Env = env
	hasCommits := head.Run() == nil

	var entries []taskHistoryEntry
	if hasCommits {
		const sep = "\x1f"
		logCmd := exec.CommandContext(ctx, "git", "log", "--date=iso-strict", "--pretty=format:%h"+sep+"%ad"+sep+"%s", fmt.Sprintf("-n%d", *limit))
		logCmd.Env = env
		var stdout, stderr bytes.Buffer
		logCmd.Stdout = &stdout
		logCmd.Stderr = &stderr
		if err := logCmd.Run(); err != nil {
			return fatal(jsonOut, "tasks snapshot history unavailable: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		for line := range strings.SplitSeq(strings.TrimRight(stdout.String(), "\n"), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, sep, 3)
			if len(parts) != 3 {
				continue
			}
			entries = append(entries, taskHistoryEntry{SHA: parts[0], Date: parts[1], Subject: parts[2]})
		}
	}

	if jsonOut {
		if entries == nil {
			entries = []taskHistoryEntry{}
		}
		return printJSON(entries)
	}
	if len(entries) == 0 {
		fmt.Println("no snapshot commits yet")
		return 0
	}
	for _, e := range entries {
		fmt.Printf("%s %s %s\n", e.SHA, e.Date, e.Subject)
	}
	return 0
}

func cmdConfig(cfg *config.Config, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: config <dump|doctor>")
	}
	switch sub := args[0]; sub {
	case "dump":
		return cmdConfigDump(cfg, jsonOut)
	case "doctor":
		return cmdConfigDoctor(cfg, jsonOut)
	default:
		return fatal(jsonOut, "unknown config command: %s", sub)
	}
}

// redactedConfig returns a shallow copy of cfg with credential fields
// blanked out. Keep in sync with cmd/gen-config-docs's redactedYAMLPaths —
// both exist because the doc generator redacts a default value that's
// always empty anyway, while this redacts a live, possibly populated value.
func redactedConfig(cfg *config.Config) config.Config {
	out := *cfg
	if out.Todoist.APIToken != "" {
		out.Todoist.APIToken = "[redacted]"
	}
	if out.Server.AuthToken != "" {
		out.Server.AuthToken = "[redacted]"
	}
	return out
}

func cmdConfigDump(cfg *config.Config, jsonOut bool) int {
	redacted := redactedConfig(cfg)
	if jsonOut {
		return printJSON(redacted)
	}
	data, err := yaml.Marshal(redacted)
	if err != nil {
		return fatal(jsonOut, "marshal config: %v", err)
	}
	_, _ = os.Stdout.Write(data)
	return 0
}

type configDoctorFinding struct {
	Severity string `json:"severity"` // "error", "warning", or "ok" (no findings)
	Message  string `json:"message"`
}

type configDoctorReport struct {
	Findings []configDoctorFinding `json:"findings"`
}

func cmdConfigDoctor(cfg *config.Config, jsonOut bool) int {
	var findings []configDoctorFinding
	add := func(severity, format string, a ...any) {
		findings = append(findings, configDoctorFinding{Severity: severity, Message: fmt.Sprintf(format, a...)})
	}

	addConfigPermFindings(add)

	dirs := cfg.Directories()
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		dir := dirs[name]
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		switch {
		case os.IsNotExist(err):
			// Most of these are created lazily on first use (e.g. artifacts,
			// experiences) — absence alone is not an error.
			add("warning", "%s dir does not exist yet: %s", name, dir)
		case err != nil:
			add("error", "%s dir: %v", name, err)
		case !info.IsDir():
			add("error", "%s dir is not a directory: %s", name, dir)
		}
	}

	if cfg.Agent.Provider != "" {
		if _, err := task.ValidateAgentProvider(cfg.Agent.Provider); err != nil {
			add("error", "agent.provider: %v", err)
		}
	}
	if cfg.Agent.HeadlessPermissionMode != "" {
		if _, err := config.NormalizeHeadlessPermissionMode(cfg.Agent.HeadlessPermissionMode); err != nil {
			add("error", "agent.headless_permission_mode: %v", err)
		}
	}
	if cfg.Agent.SandboxMode != "" {
		mode, err := config.NormalizeSandboxMode(cfg.Agent.SandboxMode)
		if err != nil {
			add("error", "agent.sandbox_mode: %v", err)
		} else if mode == "enforce" && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			add("error", "agent.sandbox_mode=enforce requires darwin or linux; current host is %s", runtime.GOOS)
		}
	}

	if cfg.Todoist.Enabled && cfg.Todoist.APIToken == "" {
		add("error", "todoist.enabled is true but no API token is set (todoist.api_token or SYBRA_TODOIST_TOKEN)")
	}
	if cfg.GitHub.PollerRole != "" && cfg.GitHub.PollerRole != "primary" && cfg.GitHub.PollerRole != "secondary" {
		add("error", "github.poller_role must be \"primary\", \"secondary\", or empty, got %q", cfg.GitHub.PollerRole)
	}
	if cfg.GitHub.App.Enabled && cfg.GitHub.App.PrivateKeyPath == "" {
		add("error", "github.app.enabled is true but github.app.private_key_path is empty")
	}

	if len(findings) == 0 {
		add("ok", "no issues found")
	}

	errCount := 0
	for _, f := range findings {
		if f.Severity == "error" {
			errCount++
		}
	}

	report := configDoctorReport{Findings: findings}
	if jsonOut {
		if code := printJSON(report); code != 0 {
			return code
		}
		if errCount > 0 {
			return 1
		}
		return 0
	}

	for _, f := range findings {
		fmt.Printf("[%s] %s\n", f.Severity, f.Message)
	}
	if errCount > 0 {
		return 1
	}
	return 0
}

func addConfigPermFindings(add func(severity, format string, a ...any)) {
	home := config.HomeDir()
	addPathPermFinding(add, "config home", home, 0o700)
	addPathPermFinding(add, "config file", filepath.Join(home, "config.yaml"), 0o600)
}

func addPathPermFinding(add func(severity, format string, a ...any), label, path string, target os.FileMode) {
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		add("warning", "%s does not exist yet: %s", label, path)
		return
	case err != nil:
		add("error", "%s: inspect permissions: %v", label, err)
		return
	case info.Mode()&os.ModeSymlink != 0:
		add("warning", "%s is a symlink; Sybra will not chmod symlink targets: %s", label, path)
		return
	}
	if perm := info.Mode().Perm(); perm&^target != 0 {
		add("warning", "%s permissions are %04o, want no broader than %04o: %s", label, perm, target, path)
	}
}
