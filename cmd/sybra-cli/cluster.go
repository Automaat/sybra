package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/task"
)

const clusterCmdTimeout = 60 * time.Second

func cmdCluster(cfg *config.Config, tasks *task.Manager, projects *project.Store, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: sybra-cli cluster <nodes|reassign> [flags]")
	}
	switch args[0] {
	case "nodes":
		return cmdClusterNodes(cfg, jsonOut)
	case "reassign":
		return cmdClusterReassign(cfg, tasks, projects, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown cluster command: %s", args[0])
	}
}

func cmdClusterNodes(cfg *config.Config, jsonOut bool) int {
	if !cfg.IsLeader() {
		return fatal(jsonOut, "this node is not a cluster leader")
	}
	type nodeOut struct {
		Name      string `json:"name"`
		Endpoint  string `json:"endpoint"`
		Trusted   bool   `json:"trusted"`
		Encrypted bool   `json:"encrypted"`
		Homes     int    `json:"homes"`
	}
	out := make([]nodeOut, 0, len(cfg.Cluster.Followers))
	for i := range cfg.Cluster.Followers {
		f := cfg.Cluster.Followers[i]
		out = append(out, nodeOut{
			Name:      f.Name,
			Endpoint:  f.PrimaryEndpoint(),
			Trusted:   f.Trusted,
			Encrypted: f.Encrypted(),
			Homes:     len(f.Homes),
		})
	}
	if jsonOut {
		return printJSON(out)
	}
	if len(out) == 0 {
		fmt.Println("no followers configured")
		return 0
	}
	for _, n := range out {
		fmt.Printf("%-16s %-32s trusted=%-5v encrypted=%-5v homes=%d\n", n.Name, n.Endpoint, n.Trusted, n.Encrypted, n.Homes)
	}
	return 0
}

func cmdClusterReassign(cfg *config.Config, tasks *task.Manager, projects *project.Store, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("reassign", flag.ContinueOnError)
	node := fs.String("node", "", `target node name, or "local" to bring the task back to the leader`)
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if fs.NArg() != 1 {
		return fatal(jsonOut, "usage: sybra-cli cluster reassign <task-id> --node <name>")
	}
	taskID := fs.Arg(0)
	if *node == "" {
		return fatal(jsonOut, "--node is required")
	}
	if !cfg.IsLeader() {
		return fatal(jsonOut, "this node is not a cluster leader")
	}

	logger := slog.Default()
	roster, err := clusterlead.NewRoster(cfg, logger)
	if err != nil {
		return fatal(jsonOut, "build roster: %v", err)
	}
	assigner := clusterlead.NewAssigner(cfg, tasks, roster, isWorkProject(projects), nil, logger)

	ctx, cancel := context.WithTimeout(context.Background(), clusterCmdTimeout)
	defer cancel()
	if err := assigner.Reassign(ctx, taskID, *node); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(map[string]string{"id": taskID, "node": *node})
	}
	fmt.Printf("reassigned %s to %s\n", taskID, *node)
	return 0
}

func isWorkProject(projects *project.Store) func(string) bool {
	return func(projectID string) bool {
		if projectID == "" {
			return false
		}
		if projects == nil {
			return true
		}
		rawType, err := projects.RawType(projectID)
		if err != nil {
			return true
		}
		return rawType != project.ProjectTypePet
	}
}
