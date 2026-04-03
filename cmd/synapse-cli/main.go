package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
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

	store, err := task.NewStore(cfg.TasksDir)
	if err != nil {
		return fatal(jsonOut, "open store: %v", err)
	}

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
	default:
		return fatal(jsonOut, "unknown command: %s", cmd)
	}
}

func cmdList(s *task.Store, args []string, jsonOut bool) int {
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

func cmdGet(s *task.Store, args []string, jsonOut bool) int {
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
	if len(t.Tags) > 0 {
		fmt.Printf("Tags:   %s\n", strings.Join(t.Tags, ", "))
	}
	fmt.Printf("Created: %s\n", t.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Printf("Updated: %s\n", t.UpdatedAt.Format("2006-01-02 15:04"))
	if t.Body != "" {
		fmt.Printf("\n%s\n", t.Body)
	}
	return 0
}

func cmdCreate(s *task.Store, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task body markdown")
	mode := fs.String("mode", "headless", "agent mode: headless|interactive")
	tags := fs.String("tags", "", "comma-separated tags")
	proj := fs.String("project", "", "project id (owner/repo)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *title == "" {
		return fatal(jsonOut, "title is required")
	}

	t, err := s.Create(*title, *body, *mode)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	updates := map[string]any{}
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
	if len(updates) > 0 {
		t, err = s.Update(t.ID, updates)
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

func cmdUpdate(s *task.Store, args []string, jsonOut bool) int {
	if len(args) < 1 {
		return fatal(jsonOut, "usage: update <id> [flags]")
	}

	id := args[0]
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	title := fs.String("title", "", "new title")
	status := fs.String("status", "", "new status")
	body := fs.String("body", "", "new body")
	mode := fs.String("mode", "", "new agent mode")
	tags := fs.String("tags", "", "comma-separated tags (replaces existing)")
	proj := fs.String("project", "", "project id (owner/repo)")
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
	if *body != "" {
		updates["body"] = *body
	}
	if *mode != "" {
		updates["agent_mode"] = *mode
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

	if len(updates) == 0 {
		return fatal(jsonOut, "no updates specified")
	}

	t, err := s.Update(id, updates)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	if jsonOut {
		return printJSON(t)
	}
	fmt.Printf("Updated task %s\n", t.ID)
	return 0
}

func cmdDelete(s *task.Store, args []string, jsonOut bool) int {
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
		return fatal(jsonOut, "usage: project <list|get|create|delete> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdProjectList(ps, jsonOut)
	case "get":
		return cmdProjectGet(ps, rest, jsonOut)
	case "create":
		return cmdProjectCreate(ps, rest, jsonOut)
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
	_, _ = fmt.Fprintln(w, "ID\tNAME\tURL")
	for i := range projects {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", projects[i].ID, projects[i].Name, projects[i].URL)
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
	fmt.Printf("ID:    %s\nName:  %s\nOwner: %s\nRepo:  %s\nURL:   %s\nClone: %s\n",
		p.ID, p.Name, p.Owner, p.Repo, p.URL, p.ClonePath)
	return 0
}

func cmdProjectCreate(ps *project.Store, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("project create", flag.ContinueOnError)
	url := fs.String("url", "", "GitHub repository URL (required)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *url == "" {
		return fatal(jsonOut, "url is required")
	}
	p, err := ps.Create(*url)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(p)
	}
	fmt.Printf("Created project %s\n", p.ID)
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

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: synapse-cli [--json] <command> [flags]

Commands:
  list     [--status STATUS] [--tag TAG] [--project ID]
  get      <id>
  create   --title TITLE [--body BODY] [--mode MODE] [--tags t1,t2] [--project ID]
  update   <id> [--title T] [--status S] [--body B] [--mode M] [--tags T] [--project ID]
  delete   <id>

  project list
  project get <id>
  project create --url <github-url>
  project delete <id>

Global flags:
  --json   Output as JSON`)
}
