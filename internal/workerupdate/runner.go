package workerupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/version"
	"github.com/Automaat/sybra/internal/workercontrol"
)

type journal struct {
	WorkerID     string    `json:"worker_id"`
	LeaderHomeID string    `json:"leader_home_id"`
	ID           string    `json:"id"`
	Revision     string    `json:"revision"`
	Previous     string    `json:"previous"`
	Phase        string    `json:"phase"`
	Session      string    `json:"session"`
	SwitchedAt   time.Time `json:"switched_at"`
}

type runner struct {
	cfg          Config
	leader       *leaderClient
	gh           func(context.Context, ...string) ([]byte, error)
	command      func(context.Context, string, ...string) error
	trust        func(string) error
	localCheck   func(context.Context, string) error
	serviceCheck func(context.Context) error
	now          func() time.Time
}

// RunOnce advances a durable operation once. Busy workers and interrupted
// network calls are retried by the timer; no timeout kills accepted work.
func RunOnce(ctx context.Context, cfg Config, retryQuarantined bool) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if runtime.GOOS != "linux" {
		return "", errors.New("worker updater: Linux standalone host required")
	}
	if os.Geteuid() != 0 {
		return "", errors.New("worker updater: root deployment service required")
	}
	for _, path := range []string{cfg.ReleaseRoot, cfg.StateDir, cfg.AgentConfig, filepath.Dir(cfg.CurrentLink)} {
		if err := trustedPath(path); err != nil {
			return "", err
		}
	}
	r := &runner{cfg: cfg, leader: newLeaderClient(cfg), gh: runGitHub, command: runCommand, trust: trustedPath, now: time.Now}
	r.localCheck = r.checkLocalWorker
	r.serviceCheck = r.checkService
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return "", err
	}
	if err := trustedPath(ghPath); err != nil {
		return "", err
	}
	unlock, err := fsutil.LockFileWithin(r.journalPath(), time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = unlock() }()
	if err := r.leader.identify(ctx); err != nil {
		return "", err
	}
	j, err := r.load()
	if err != nil {
		return "", err
	}
	return r.step(ctx, &j, retryQuarantined)
}

func runCommand(ctx context.Context, name string, args ...string) error {
	// Child output can contain configuration/credentials; never persist it in
	// the updater journal or public deployment diagnostics.
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return errors.New("worker updater: host command failed")
	}
	return nil
}

func validNonce(s string) bool { _, err := hex.DecodeString(s); return len(s) == 32 && err == nil }

func (r *runner) step(ctx context.Context, j *journal, retry bool) (string, error) {
	if j.Phase == "" || j.Phase == "complete" || j.Phase == "retired" || j.Phase == "quarantined" {
		var release workercontrol.WorkerRelease
		if err := r.leader.call(ctx, http.MethodGet, "/worker/v1/release", nil, &release, true); err != nil {
			return "waiting for leader release", err
		}
		if !version.ValidRevision(release.Revision) {
			return "", errors.New("worker updater: invalid leader release")
		}
		if release.Protocol != executioncontract.CurrentVersion() {
			return "", errors.New("worker updater: protocol changed; operator bootstrap required")
		}
		if j.Phase == "quarantined" && j.Revision == release.Revision && !retry {
			return "candidate quarantined; explicit retry required", nil
		}
		current, err := r.leader.current(ctx)
		if err != nil {
			return "waiting for worker", err
		}
		previous, err := r.pointer()
		if err != nil {
			return "", err
		}
		if previous != current.BuildVersion {
			return "", errors.New("worker updater: running build differs from retained pointer")
		}
		if err := r.trust(filepath.Join(r.cfg.ReleaseRoot, previous, "sybra-agentd")); err != nil {
			return "", err
		}
		if previous == release.Revision {
			return "worker current", nil
		}
		if current.State != "active" || current.Readiness != "ready" || current.UpdateHeld {
			return "waiting for available worker", nil
		}
		if err := r.serviceCheck(ctx); err != nil {
			return "", err
		}
		if err := r.stage(ctx, release.Revision, retry); err != nil {
			return "waiting for verified artifact", err
		}
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return "", err
		}
		*j = journal{WorkerID: r.cfg.WorkerID, LeaderHomeID: r.cfg.LeaderHomeID, ID: hex.EncodeToString(nonce), Revision: release.Revision, Previous: previous, Phase: "draining", Session: current.SessionID}
		if err := r.save(j); err != nil {
			return "", err
		}
	}
	switch j.Phase {
	case "draining":
		var hold workercontrol.UpdateHold
		if err := r.leader.update(ctx, "begin", *j, &hold); err != nil {
			var response *httpError
			if errors.As(err, &response) && response.status == http.StatusServiceUnavailable {
				current, readErr := r.leader.current(ctx)
				if readErr == nil && !current.UpdateHeld {
					j.Phase = "retired" // no hold or host mutation; target/state changed
					if err := r.save(j); err != nil {
						return "", err
					}
					return "unheld update intent retired", nil
				}
			}
			return "waiting for update hold", err
		}
		if hold.PreviousRevision != j.Previous {
			return "", errors.New("worker updater: previous release changed; retain hold for operator recovery")
		}
		if err := r.leader.update(ctx, "check", *j, nil); err != nil {
			return "draining accepted work and handback", err
		}
		current, err := r.leader.current(ctx)
		if err != nil {
			return "", err
		}
		j.Session, j.Phase, j.SwitchedAt = current.SessionID, "switching", r.now().UTC()
		if err := r.save(j); err != nil {
			return "", err
		}
		return r.switchRelease(ctx, j, false)
	case "switching":
		return r.switchRelease(ctx, j, false)
	case "verifying":
		return r.verifyRunning(ctx, j, false)
	case "rollback":
		return r.switchRelease(ctx, j, true)
	case "rollback-verifying":
		return r.verifyRunning(ctx, j, true)
	case "releasing", "rollback-releasing":
		return r.releaseHold(ctx, j)
	default:
		return "", errors.New("worker updater: unknown journal phase; operator recovery required")
	}
}

func (r *runner) switchRelease(ctx context.Context, j *journal, rollback bool) (string, error) {
	target, next := j.Revision, "verifying"
	if rollback {
		target, next = j.Previous, "rollback-verifying"
	}
	current, err := r.leader.current(ctx)
	if err != nil && !rollback {
		if r.now().Sub(j.SwitchedAt) >= 2*time.Minute {
			j.Phase = "rollback"
			if err := r.save(j); err != nil {
				return "", err
			}
			return r.switchRelease(ctx, j, true)
		}
		return "waiting for held worker", err
	}
	if err == nil && !current.UpdateHeld {
		return "", errors.New("worker updater: update hold disappeared; refusing restart")
	}
	// A crash after restart but before journal persistence must not restart a
	// replacement session a second time.
	alreadyRunning := err == nil && current.BuildVersion == target && current.SessionID != j.Session
	if !alreadyRunning {
		if err := r.leader.update(ctx, "held", *j, nil); err != nil {
			return "waiting for proof of update hold; refusing restart", err
		}
		if !rollback {
			if err := r.leader.update(ctx, "check", *j, nil); err != nil {
				return "waiting for quiescence", err
			}
		}
		session := ""
		if err == nil {
			session = current.SessionID
		}
		if err := r.localCheck(ctx, session); err != nil {
			return "waiting for local worker quiescence", err
		}
		if err := r.trust(filepath.Join(r.cfg.ReleaseRoot, target, "sybra-agentd")); err != nil {
			return "", err
		}
		if err := r.switchTo(target); err != nil {
			return "", err
		}
		if err := r.command(ctx, "/usr/bin/systemctl", "restart", "sybra-agentd.service"); err != nil {
			if !rollback {
				j.Phase = "rollback"
				if saveErr := r.save(j); saveErr != nil {
					return "", saveErr
				}
			}
			return "restart failed; update hold retained", err
		}
	}
	j.Phase, j.SwitchedAt = next, r.now().UTC()
	if err := r.save(j); err != nil {
		return "", err
	}
	return "waiting for replacement worker health", nil
}

func (r *runner) verifyRunning(ctx context.Context, j *journal, rollback bool) (string, error) {
	target := j.Revision
	if rollback {
		target = j.Previous
	}
	current, err := r.leader.current(ctx)
	if err == nil && current.BuildVersion == target && current.SessionID != j.Session && current.Readiness == "ready" {
		j.Phase = "releasing"
		if rollback {
			j.Phase = "rollback-releasing"
		}
		if err := r.save(j); err != nil {
			return "", err
		}
		return r.releaseHold(ctx, j)
	}
	if !rollback && r.now().Sub(j.SwitchedAt) >= 2*time.Minute {
		j.Phase = "rollback"
		if err := r.save(j); err != nil {
			return "", err
		}
		return r.switchRelease(ctx, j, true)
	}
	return "waiting for healthy replacement; update hold retained", err
}

// Once release is attempted its reply may be lost after commit. Persist this
// phase first: no later health timeout may roll back a worker accepting new work.
func (r *runner) releaseHold(ctx context.Context, j *journal) (string, error) {
	rollback := j.Phase == "rollback-releasing"
	target, final := j.Revision, "complete"
	if rollback {
		target, final = j.Previous, "quarantined"
	}
	current, err := r.leader.current(ctx)
	if err != nil {
		return "waiting for release confirmation; no restart permitted", err
	}
	if current.BuildVersion != target || current.Readiness != "ready" {
		return "waiting for expected worker after release attempt", nil
	}
	if current.UpdateHeld {
		if err := r.leader.update(ctx, "finish", *j, nil); err != nil {
			return "waiting for durable handback", err
		}
	}
	j.Phase = final
	if err := r.save(j); err != nil {
		return "", err
	}
	return final, nil
}
