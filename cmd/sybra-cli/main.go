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
	"github.com/Automaat/sybra/internal/skills"
	"github.com/Automaat/sybra/internal/skillsync"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
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

	// Detect the hook subcommand before config.Load can abort: codex lifecycle
	// hooks must fail open (see cmdHook) — a malformed config must never make
	// `sybra-cli hook` exit non-zero and stall an agent run.
	isHook := len(filtered) >= 1 && filtered[0] == "hook"

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
	switch cmd {
	case "list":
		return cmdList(store, rest, jsonOut)
	case "get":
		return cmdGet(store, rest, jsonOut)
	case "create":
		return cmdCreate(store, rest, jsonOut)
	case "handoff":
		return cmdHandoff(store, projStore, rest, jsonOut)
	case "umbrella":
		return cmdUmbrella(cfg, store, projStore, rest, jsonOut)
	case "update":
		return cmdUpdate(store, rest, jsonOut)
	case "link-pr":
		return cmdLinkPR(store, rest, jsonOut)
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
	case "evaluation":
		return cmdEvaluation(cfg, store, rest, jsonOut)
	case "harness-evolution":
		return cmdHarnessEvolution(cfg, store, rest, jsonOut)
	case "stats":
		return cmdStats(cfg, rest, jsonOut)
	case "install-skills":
		return cmdInstallSkills(cfg, jsonOut)
	case "artifact":
		return cmdArtifact(rest, jsonOut)
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

func cmdCreate(s *task.Manager, args []string, jsonOut bool) int {
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
	if *planContract != "" {
		updates["plan_contract"] = *planContract
	}
	if *planCritique != "" {
		updates["plan_critique"] = *planCritique
	}
	if *planResearch != "" {
		updates["plan_research"] = *planResearch
	}
	if *planDecisions != "" {
		updates["plan_decisions"] = *planDecisions
	}
	if *planBrief != "" {
		updates["plan_brief"] = *planBrief
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
	stage := fs.String("stage", "implement", "workflow entry stage: implement|review|testing|ready-pr")
	rawStatus := fs.String("status", "", "raw task status to create without starting a workflow")
	sourceProvider := fs.String("source-provider", "", "provider that produced the handed-off work: claude|codex|copilot")
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
		return fatal(jsonOut, "--stage %s requires --source-provider <claude|codex|copilot> so cross-provider review/testing is deterministic", *stage)
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
		return handoffStageConfig{}, "", false, fmt.Errorf("invalid --stage %q (valid: implement, review, testing, ready-pr; aliases: in-progress, ready-review, agentic-review, test, open-pr, create-pr)", stage)
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

// handoffStageConfigFor maps a handoff stage to the tags that route the task
// into the right Sybra lane on creation, or false for an unknown stage.
//   - implement: simple-task-handoff → in-progress → implement → review → testing → PR
//   - review:    simple-task-handoff-review → ready-review → review → testing → PR
//   - testing:   simple-task-handoff-testing → testing → adversarial test → PR
//   - ready-pr:  simple-task-handoff-ready-pr → ready-pr → open/update PR
func handoffStageConfigFor(stage string) (handoffStageConfig, bool) {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "", "implement", "in-progress":
		return handoffStageConfig{name: "implement", tags: []string{"handoff"}}, true
	case "review", "ready-review", "agentic-review":
		return handoffStageConfig{name: "review", tags: []string{"handoff", "handoff-review"}}, true
	case "testing", "test":
		return handoffStageConfig{name: "testing", tags: []string{"handoff", "handoff-testing"}}, true
	case "ready-pr", "open-pr", "create-pr":
		return handoffStageConfig{name: "ready-pr", tags: []string{"handoff", "handoff-ready-pr"}}, true
	default:
		return handoffStageConfig{}, false
	}
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
	switch stage {
	case "review", "testing":
		return true
	default:
		return false
	}
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
	branch, err := project.CurrentBranch(dir)
	if err != nil {
		return fmt.Errorf("resolve worktree branch: %w", err)
	}
	if branch == "" {
		return fmt.Errorf("worktree %q is in detached HEAD — check out a feature branch before handoff", dir)
	}
	// Fail closed: if the default branch can't be determined we can't prove the
	// worktree isn't on it, so refuse rather than risk pushing agent commits to
	// the default branch with no PR.
	def, dErr := project.DefaultBranch(proj.ClonePath)
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
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
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
		RepoDir:      repoDir,
		SkillsFS:     skills.FS,
		PrimaryDst:   cfg.SkillsDir,
		SybraHomeDir: config.HomeDir(),
		UserHomeDir:  home,
	})

	dsts := []string{
		cfg.SkillsDir,
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".codex", "skills"),
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

func cmdUpdate(s *task.Manager, args []string, jsonOut bool) int {
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
		issue:             fs.String("issue", "", "GitHub issue URL"),
		sourceProvider:    fs.String("source-provider", "", "handoff source provider: claude|codex|copilot|none"),
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
		updates["issue"] = *f.issue
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

// cmdLinkPR links a GitHub PR number to a task and advances non-terminal tasks
// to in-review so the PR monitor loop can take over (auto-merge / done on
// merge). Use when a PR was opened outside of Sybra (manually or by an external
// tool) and the task's pr_number is still 0.
func cmdLinkPR(s *task.Manager, args []string, jsonOut bool) int {
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

	u := task.Update{PRNumber: task.Ptr(prNum)}
	if !task.IsTerminalStatus(t.Status) && t.Status != task.StatusInReview {
		u.Status = task.Ptr(task.StatusInReview)
		u.StatusReason = task.Ptr("")
	}

	t, err = s.Update(id, u)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(t)
	}
	if u.Status != nil {
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

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: sybra-cli [--json] <command> [flags]

Commands:
  list     [--status STATUS] [--tag TAG] [--project ID]
           STATUS: new|todo|planning|plan-review|in-progress|in-review|testing|ready-pr|human-required|done|cancelled
  get      [--compact] <id>
  create   --title TITLE [--body BODY] [--plan PLAN] [--plan-contract JSON] [--mode MODE] [--type TYPE] [--tags t1,t2] [--project ID] [--branch B] [--pr N] [--issue URL] [--allow-dup]
           TYPE: normal|debug|research
  handoff  --title TITLE [--body BODY] [--plan PLAN | --plan-file PATH] [--project ID] [--worktree-dir DIR] [--stage STAGE | --status STATUS] [--source-provider claude|codex|copilot] [--pr N] [--mode MODE] [--tags t1,t2]
           Hand a task to Sybra at a workflow entry point, reusing the given git worktree
           (default: cwd). Project is derived from the worktree's origin remote
           when --project is omitted. STAGE (default implement):
             implement      have a plan -> Sybra implements, reviews, tests, opens the PR
             review         implemented locally -> Sybra enters agentic review
             testing        reviewed locally -> Sybra tests, then opens the PR
             ready-pr       tested locally -> Sybra opens or updates the PR; pass --pr N
                            only to link an existing same-branch PR
           --source-provider records which local agent produced handed-off work
           so review/testing/PR steps can run on a different provider.
           Required for --stage review|testing; optional for implement/ready-pr.
           --status STATUS creates the task directly in that status without workflow dispatch
  umbrella <issue-url> [--model M]
           Expand a GitHub umbrella issue into a gated task DAG: one umbrella tracker
           plus one blocked child per sub-issue, with dependency edges extracted by an
           LLM planner. Re-running only materializes sub-issues without an existing task.
  update   <id> [--title T] [--status S] [--status-reason R] [--body B] [--plan PLAN] [--plan-file PATH] [--plan-contract JSON|--plan-contract-file PATH] [--plan-research TEXT|--plan-research-file PATH] [--plan-decisions TEXT|--plan-decisions-file PATH] [--plan-brief TEXT|--plan-brief-file PATH] [--mode M] [--type TYPE] [--tags T] [--project ID] [--branch B] [--pr N] [--issue URL] [--source-provider P|none] [--max-turns N] [--reasoning-effort E]
  link-pr  <id> <pr-number>
           Link a PR number to a task and advance it to in-review. Use when a PR
           was opened outside of Sybra; the PR monitor will then auto-merge or
           advance the task to done once the PR lands.
  delete   <id>

  project list
  project get <id>
  project create --url <github-url> [--type pet|work]
  project update <id> --type pet|work
  project delete <id>

  audit    [--since DURATION|DATE] [--until DATE] [--type TYPE] [--task ID] [--summary]
  board    (status counts + in-progress/plan-review/human-required task lists)
  monitor  scan [--json]    one-shot read-only detector pass (no remediation)
  evaluation scan [--json]  fleet scorecard (autonomy, throughput, efficiency)
  harness-evolution run [--lookback 168h] [--min-cluster-size 2] [--file] [--json]
           Cluster selfmonitor failures into governed harness-change proposals.
  stats lifecycle [--since 30d] [--slowest N] [--json]
           Per-phase lead-time breakdown (planning/implementing/testing/review/
           waiting) for tasks that landed in the window — where time is spent.
  health   [--severity warning|critical] [--category CATEGORY]

  triage classify <id>         Classify a single task via claude -p and apply the verdict.
  triage classify --all        Classify every task with status=new.

  install-skills               Install/refresh Sybra's bundled skills into
                               ~/.claude/skills, ~/.codex/skills, ~/.agents/skills
                               (and the app skills dir) for Claude Code + Codex.

  artifact list <task-id>      List artifacts for a task.
  artifact get  <task-id> <name>  Print raw artifact bytes to stdout.
  artifact reindex <task-id>   Rebuild index.json from *.meta.json files.

Global flags:
  --json   Output as JSON`)
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
