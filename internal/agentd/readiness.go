package agentd

import (
	"os"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/pressure"
)

// Readiness is advertised independently of process liveness. A worker with a
// blocked spool must keep heartbeating and replaying accepted runs, but should
// receive no new starts. Reasons are static codes, never host paths or secrets.
func (d *Daemon) readiness() string {
	if d.spool.capacityError() != nil {
		return "spool_pressure"
	}
	for _, root := range []string{d.cfg.StateRoot, d.cfg.WorkspaceRoot} {
		bytes, ok := pressure.CurrentDiskFreeBytes(root)
		if !ok {
			return "storage_unavailable"
		}
		if bytes < float64(d.cfg.MinDiskFreeBytes) {
			return "storage_pressure"
		}
		// statfs cannot see per-user quotas or read-only mounts. Exercise the
		// same write-and-rename operation the spool needs, without touching it.
		if err := probeStorage(root); err != nil {
			return "storage_unwritable"
		}
	}
	if _, err := agent.ProbeSandboxPosture(d.cfg.SandboxMode); err != nil {
		return "sandbox_unavailable"
	}
	return "ready"
}

func probeStorage(root string) error {
	file, err := os.CreateTemp(root, ".agentd-readiness-*")
	if err != nil {
		return err
	}
	target := file.Name()
	defer func() { _ = os.Remove(target) }()
	if err := file.Close(); err != nil {
		return err
	}
	return fsutil.AtomicWrite(target, []byte("ready\n"))
}
