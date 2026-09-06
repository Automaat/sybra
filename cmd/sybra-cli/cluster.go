package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/sybra"
)

func cmdCluster(cfg *config.Config, api *apiClient, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: sybra-cli cluster <nodes|reassign|gen-cert|reconcile-results> [flags]")
	}
	switch args[0] {
	case "nodes":
		return cmdClusterNodes(cfg, jsonOut)
	case "reassign":
		return cmdClusterReassign(cfg, api, args[1:], jsonOut)
	case "gen-cert":
		return cmdClusterGenCert(args[1:], jsonOut)
	case "reconcile-results":
		return cmdClusterReconcileResults(api, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown cluster command: %s", args[0])
	}
}

func cmdClusterReconcileResults(api *apiClient, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("reconcile-results", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "acknowledge only results with matching durable completion receipts (default: dry run)")
	after := fs.String("after", "", "opaque nextAfter cursor from the previous page")
	limit := fs.Int("limit", 100, "maximum results to examine (1-100)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 100 {
		return fatal(jsonOut, "usage: cluster reconcile-results [--apply] [--after CURSOR] [--limit 1-100]")
	}
	if api == nil {
		return fatal(jsonOut, "result recovery needs a Sybra server and none is reachable")
	}
	report, err := callAPI[sybra.RemoteResultRecoveryReport](api, "App", "ReconcileRemoteResults", *apply, *after, *limit)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(report)
	}
	mode := "dry run"
	if report.Apply {
		mode = "apply"
	}
	fmt.Printf("%s: scanned=%d eligible=%d acknowledged=%d preserved=%d events=%d\n", mode,
		report.Scanned, report.Eligible, report.Acknowledged, report.Preserved, report.Events)
	for reason, count := range report.Reasons {
		fmt.Printf("  %s: %d\n", reason, count)
	}
	if report.NextAfter != "" {
		fmt.Printf("Continue with --after %s\n", report.NextAfter)
	}
	return 0
}

func cmdClusterGenCert(args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("gen-cert", flag.ContinueOnError)
	dir := fs.String("out", "", "directory for follower.crt / follower.key (default: <sybra home>/tls)")
	var hosts multiFlag
	fs.Var(&hosts, "host", "hostname or IP the leader will dial (repeatable)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	outDir := *dir
	if outDir == "" {
		outDir = filepath.Join(config.HomeDir(), "tls")
	}

	got, err := GenerateFollowerCert(outDir, hosts, time.Now())
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if jsonOut {
		return printJSON(got)
	}
	fmt.Printf("wrote %s\n      %s\n\n", got.CertFile, got.KeyFile)
	fmt.Printf("On the FOLLOWER, serve the control plane with this keypair:\n\n")
	fmt.Printf("  cluster:\n    tls:\n      cert_file: %s\n      key_file: %s\n\n", got.CertFile, got.KeyFile)
	fmt.Printf("On the LEADER, pin this exact certificate:\n\n")
	fmt.Printf("  cluster:\n    followers:\n      - name: <node-name>\n        endpoints: [\"https://%s:8080\"]\n        tls_pin: %s\n\n", got.Hosts[0], got.Pin)
	fmt.Printf("Expires %s. The leader validates this fingerprint, not a CA chain,\n", got.NotAfter.Format("2006-01-02"))
	fmt.Printf("so regenerating the certificate means updating tls_pin on the leader.\n")
	return 0
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("empty --host")
	}
	*m = append(*m, v)
	return nil
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

func splitLeadingID(args []string) (id string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func cmdClusterReassign(cfg *config.Config, api *apiClient, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("reassign", flag.ContinueOnError)
	node := fs.String("node", "", `target node name, or "local" to bring the task back to the leader`)

	taskID, rest := splitLeadingID(args)
	if err := fs.Parse(rest); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if taskID == "" && fs.NArg() == 1 {
		taskID = fs.Arg(0)
	} else if taskID != "" && fs.NArg() > 0 {
		return fatal(jsonOut, "usage: sybra-cli cluster reassign <task-id> --node <name>")
	}
	if taskID == "" {
		return fatal(jsonOut, "usage: sybra-cli cluster reassign <task-id> --node <name>")
	}
	if *node == "" {
		return fatal(jsonOut, "--node is required")
	}
	if !cfg.IsLeader() {
		return fatal(jsonOut, "this node is not a cluster leader")
	}

	// Reassignment mutates the task and pushes it to a follower, so the server
	// owns the whole operation. gen-cert and nodes reach this file with no
	// board at all, so this is the one subcommand that has to ask for one.
	if api == nil {
		return fatal(jsonOut, "cluster reassign needs a Sybra server and none is reachable; start one, or set %s", serverTargetEnv)
	}
	if _, err := callAPI[struct{}](api, "ClusterService", "ReassignTask", taskID, *node); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	return reportReassign(jsonOut, taskID, *node)
}

func reportReassign(jsonOut bool, taskID, node string) int {
	if jsonOut {
		return printJSON(map[string]string{"id": taskID, "node": node})
	}
	fmt.Printf("reassigned %s to %s\n", taskID, node)
	return 0
}
