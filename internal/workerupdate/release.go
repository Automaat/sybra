package workerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/Automaat/sybra/internal/github"
)

func runGitHub(ctx context.Context, args ...string) ([]byte, error) {
	data, err := github.RunWithEnv(ctx, os.Environ(), args...)
	if err != nil {
		return nil, errors.New("worker updater: GitHub artifact/provenance operation failed")
	}
	return data, nil
}

func (r *runner) verify(ctx context.Context, dir, revision string) error {
	for _, name := range []string{"sybra-agentd", "sybra-worker-update"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 256<<20 {
			return errors.New("worker updater: invalid release binary")
		}
		if err := r.trust(path); err != nil {
			return err
		}
		_, err = r.gh(ctx, "attestation", "verify", path, "--repo", r.cfg.Repository,
			"--signer-workflow", r.cfg.Repository+"/.github/workflows/ci.yml",
			"--source-ref", "refs/heads/main", "--source-digest", revision,
			"--signer-digest", revision, "--deny-self-hosted-runners", "--hostname", "github.com")
		if err != nil {
			return err
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) stage(ctx context.Context, revision string, retry bool) error {
	destination := filepath.Join(r.cfg.ReleaseRoot, revision)
	if _, err := os.Lstat(destination); err == nil {
		if err := r.trust(destination); err != nil {
			return err
		}
		if err := r.verify(ctx, destination, revision); err != nil {
			return err
		}
		return r.preflight(ctx, destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := r.gh(ctx, "run", "list", "--repo", r.cfg.Repository, "--workflow", "ci.yml", "--commit", revision,
		"--event", "push", "--status", "success", "--json", "databaseId,headSha,headBranch,event,conclusion", "--limit", "10")
	if err != nil {
		return err
	}
	var runs []struct {
		ID         int64  `json:"databaseId"`
		SHA        string `json:"headSha"`
		Branch     string `json:"headBranch"`
		Event      string `json:"event"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(data, &runs); err != nil {
		return err
	}
	var runID int64
	for _, run := range runs {
		if run.ID > 0 && run.SHA == revision && run.Branch == "main" && run.Event == "push" && run.Conclusion == "success" {
			runID = run.ID
			break
		}
	}
	if runID == 0 {
		return errors.New("worker updater: successful main CI artifact is not available yet")
	}
	// One bounded stage per candidate: a persistent preflight error must not
	// download another pair of binaries on every timer tick until disk is full.
	stage := filepath.Join(r.cfg.ReleaseRoot, ".stage-"+revision)
	if err := os.Mkdir(stage, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := r.trust(stage); err != nil {
		return err
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (entry.Name() != "sybra-agentd" && entry.Name() != "sybra-worker-update") {
			return errors.New("worker updater: unexpected staging contents; operator inspection required")
		}
	}
	if len(entries) != 2 || retry {
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(stage, entry.Name())); err != nil {
				return err
			}
		}
		if _, err := r.gh(ctx, "run", "download", strconv.FormatInt(runID, 10), "--repo", r.cfg.Repository,
			"--name", "sybra-worker-linux-"+runtime.GOARCH, "--dir", stage); err != nil {
			return err
		}
		entries, err = os.ReadDir(stage)
		if err != nil {
			return err
		}
	}
	if len(entries) != 2 {
		return errors.New("worker updater: unexpected artifact contents")
	}
	if err := r.verify(ctx, stage, revision); err != nil {
		return err
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := r.preflight(ctx, stage); err != nil {
		return err
	}
	for _, name := range []string{"sybra-agentd", "sybra-worker-update", "."} {
		if err := syncPath(filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return syncPath(r.cfg.ReleaseRoot)
}

func (r *runner) preflight(ctx context.Context, dir string) error {
	if err := r.command(ctx, "/usr/sbin/runuser", "-u", r.cfg.ServiceUser, "--", filepath.Join(dir, "sybra-agentd"), "-check-config", "-config", r.cfg.AgentConfig); err != nil {
		return fmt.Errorf("worker updater: candidate configuration check: %w", err)
	}
	return nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
