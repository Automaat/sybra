package workerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/agentd"
)

func (r *runner) localConfig() (agentd.Config, error) {
	cfg, err := agentd.LoadConfig(r.cfg.AgentConfig)
	if err != nil {
		return cfg, errors.New("worker updater: local daemon configuration is invalid")
	}
	if cfg.NodeID != r.cfg.WorkerID || strings.TrimRight(cfg.LeaderURL, "/") != strings.TrimRight(r.cfg.LeaderURL, "/") || cfg.TokenEnv != r.cfg.TokenEnv {
		return cfg, errors.New("worker updater: local daemon and updater identities differ (explicit node_id required)")
	}
	return cfg, nil
}

func (r *runner) checkService(ctx context.Context) error {
	if _, err := r.localConfig(); err != nil {
		return err
	}
	data, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--property=ExecStart,MainPID", "sybra-agentd.service").Output()
	if err != nil {
		return errors.New("worker updater: cannot inspect standalone service")
	}
	pid, err := r.serviceIdentity(string(data))
	if err != nil {
		return err
	}
	if pid == 0 {
		return nil
	} // failed candidate; rollback still requires the exact leader hold
	args, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return err
	}
	argv := strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00")
	if !slices.Equal(argv, []string{filepath.Join(r.cfg.CurrentLink, "sybra-agentd"), "-config", r.cfg.AgentConfig}) {
		return errors.New("worker updater: live service command does not match updater configuration")
	}
	return nil
}

func (r *runner) serviceIdentity(data string) (int, error) {
	pid := -1
	matched := false
	for line := range strings.SplitSeq(data, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if key == "MainPID" {
			parsed, err := strconv.Atoi(value)
			if err == nil && parsed >= 0 {
				pid = parsed
			}
		}
		if key != "ExecStart" {
			continue
		}
		_, argv, ok := strings.Cut(value, "argv[]=")
		if !ok {
			continue
		}
		argv, _, _ = strings.Cut(argv, ";")
		fields := strings.Fields(argv)
		// Accept the documented unit's env PATH wrapper or a direct invocation,
		// never another script/config which happens to mention our worker ID.
		if len(fields) == 5 && fields[0] == "/usr/bin/env" && strings.HasPrefix(fields[1], "PATH=") {
			fields = fields[2:]
		}
		matched = slices.Equal(fields, []string{filepath.Join(r.cfg.CurrentLink, "sybra-agentd"), "-config", r.cfg.AgentConfig})
	}
	if !matched || pid < 0 {
		return 0, errors.New("worker updater: service must execute the configured standalone pointer and agent config")
	}
	return pid, nil
}

// Read the local durable spool after the leader proves no accepted runs remain.
// A terminal can reach the leader before the next backlog heartbeat; the spool
// already contains any artifact queued for that terminal. Never log its content.
func (r *runner) checkLocalWorker(ctx context.Context, session string) error {
	if err := r.serviceCheck(ctx); err != nil {
		return err
	}
	cfg, err := r.localConfig()
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(cfg.StateRoot, "spool.json"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, min(cfg.SpoolMaxBytes, 256<<20)+2))
	if err != nil {
		return err
	}
	if int64(len(data)) > min(cfg.SpoolMaxBytes, 256<<20)+1 {
		return errors.New("worker updater: local spool exceeds inspection bound")
	}
	return checkSpool(data, r.cfg.WorkerID, session)
}

func checkSpool(data []byte, worker, session string) error {
	var state struct {
		NodeID    string                       `json:"nodeId"`
		SessionID string                       `json:"sessionId"`
		RunAgents map[string]json.RawMessage   `json:"runAgents"`
		Events    map[string][]json.RawMessage `json:"events"`
		Artifacts map[string]json.RawMessage   `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return errors.New("worker updater: invalid local spool")
	}
	if (state.NodeID != "" && state.NodeID != worker) || state.SessionID == "" || (session != "" && state.SessionID != session) {
		return errors.New("worker updater: local spool identity differs from held worker")
	}
	if len(state.RunAgents) != 0 || len(state.Artifacts) != 0 {
		return errors.New("worker updater: local work or handback remains")
	}
	for _, events := range state.Events {
		if len(events) != 0 {
			return errors.New("worker updater: local events remain")
		}
	}
	return nil
}
