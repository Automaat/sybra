package agentd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Automaat/sybra/internal/agentworkspace"
	"github.com/Automaat/sybra/internal/executioncontract"
)

func (d *Daemon) prepareRunWorkspace(ctx context.Context, source string, spec executioncontract.RunSpec, baseBundle []byte) (agentworkspace.Layout, func(), error) {
	prepare := d.prepareWorkspace
	if prepare == nil {
		prepare = agentworkspace.PrepareWithBaseBundle
	}
	// Ordinary starts share the read lock and may prepare concurrently. A
	// pressure recovery upgrades to exclusive ownership only after all other
	// starts have durably published their RunAgents ownership in BeforeStart.
	d.workspaceMu.RLock()
	layout, err := prepare(ctx, d.cfg.WorkspaceRoot, source, spec, baseBundle)
	if err == nil || !isWorkspacePressureError(err) {
		return layout, d.workspaceMu.RUnlock, err
	}
	d.workspaceMu.RUnlock()

	d.workspaceMu.Lock()
	// Another pressure recovery may have reclaimed space while this start was
	// waiting for exclusive ownership, so retry before deleting diagnostics.
	layout, err = prepare(ctx, d.cfg.WorkspaceRoot, source, spec, baseBundle)
	if err == nil || !isWorkspacePressureError(err) {
		return layout, d.workspaceMu.Unlock, err
	}

	reclaimed, reclaimErr := d.reclaimUnprotectedWorkspacesLocked()
	if reclaimErr != nil {
		d.logger.Warn("agentd.workspace.pressure-reclaim", "removed", reclaimed, "err", reclaimErr)
	} else {
		d.logger.Warn("agentd.workspace.pressure-reclaim", "removed", reclaimed)
	}
	if reclaimed == 0 {
		return agentworkspace.Layout{}, d.workspaceMu.Unlock, err
	}
	layout, err = prepare(ctx, d.cfg.WorkspaceRoot, source, spec, baseBundle)
	return layout, d.workspaceMu.Unlock, err
}

func isWorkspacePressureError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || strings.Contains(message, "no space left on device") || strings.Contains(message, "disk quota exceeded")
}

// reclaimUnprotectedWorkspacesLocked drops only completed diagnostic
// checkouts which are not needed by a live provider or a durable artifact
// retry. Under storage pressure, preserving execution capacity takes priority
// over the normal diagnostic retention window.
func (d *Daemon) reclaimUnprotectedWorkspacesLocked() (int, error) {
	state := d.spool.snapshot()
	protected := make(map[string]bool, len(state.RunAgents)+len(state.Artifacts))
	for runID := range state.RunAgents {
		protected[runID] = true
	}
	for manifestID := range state.Artifacts {
		protected[state.Artifacts[manifestID].Manifest.RunID] = true
	}

	entries, err := os.ReadDir(d.cfg.WorkspaceRoot)
	if err != nil {
		return 0, err
	}
	removed := 0
	var removeErr error
	for _, entry := range entries {
		if !entry.IsDir() || protected[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.cfg.WorkspaceRoot, entry.Name())); err != nil {
			removeErr = errors.Join(removeErr, err)
			continue
		}
		removed++
	}
	return removed, removeErr
}

func (d *Daemon) ackArtifactAndRemoveWorkspace(manifestID, runID string) error {
	if err := d.spool.ackArtifact(manifestID); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(d.cfg.WorkspaceRoot, runID)); err != nil {
		d.logger.Warn("agentd.workspace.remove", "run_id", runID, "err", err)
	}
	return nil
}
