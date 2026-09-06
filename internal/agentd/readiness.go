package agentd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/agent"
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
	source := file.Name()
	target := filepath.Clean(source + ".ready")
	defer func() { _ = os.Remove(source); _ = os.Remove(target) }()
	_, writeErr := fmt.Fprintln(file, "ready")
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(source, target)
}
