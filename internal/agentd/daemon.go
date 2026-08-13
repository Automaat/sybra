package agentd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentworkspace"
	agentevents "github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/version"
	"github.com/Automaat/sybra/internal/workercontrol"
)

type Daemon struct {
	cfg          Config
	logger       *slog.Logger
	client       *leaderClient
	issueGrant   func(context.Context, string, string) (workercontrol.RunGrant, error)
	spool        *Spool
	manager      *agent.Manager
	approvals    *agent.ApprovalServer
	capabilities []string

	approvalMu sync.Mutex
	mu         sync.RWMutex
	sessionID  string
	runAgents  map[string]string
	agentRuns  map[string]string
	runCancels map[string]context.CancelFunc
	draining   bool
}

func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Daemon, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	for _, dir := range []string{cfg.WorkspaceRoot, cfg.StateRoot, filepath.Join(cfg.StateRoot, "logs"), filepath.Join(cfg.StateRoot, "homes")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("agentd: initialize %s: %w", dir, err)
		}
	}
	spool, err := OpenSpool(cfg.StateRoot, cfg.SpoolMaxBytes, cfg.Capacity)
	if err != nil {
		return nil, err
	}
	build := strings.TrimSpace(version.Version)
	if build == "" {
		build = "dev"
	}
	d := &Daemon{
		cfg: cfg, logger: logger, spool: spool,
		client:       newLeaderClient(cfg.LeaderURL, os.Getenv(cfg.TokenEnv)),
		capabilities: cfg.Capabilities(build), runAgents: make(map[string]string), agentRuns: make(map[string]string),
		runCancels: make(map[string]context.CancelFunc),
	}
	d.issueGrant = d.client.runGrant
	approvals, err := agent.NewDurableApprovalServer(ctx, d.emitManagerEvent, logger, 0,
		filepath.Join(cfg.StateRoot, "approval-port"), filepath.Join(cfg.StateRoot, "approval-token"))
	if err != nil {
		return nil, fmt.Errorf("agentd: approval server: %w", err)
	}
	keepApprovals := false
	defer func() {
		if !keepApprovals {
			shutdownApprovalServer(ctx, approvals)
		}
	}()
	d.approvals = approvals
	state := spool.snapshot()
	for toolUseID, decision := range state.Approvals {
		if err := approvals.StageApproval(toolUseID, decision.Approved, decision.Fingerprint); err != nil {
			return nil, fmt.Errorf("agentd: restore approval %s: %w", toolUseID, err)
		}
	}
	switch {
	case cfg.NodeID != "":
		d.cfg.NodeID = cfg.NodeID
	case state.NodeID != "":
		d.cfg.NodeID = state.NodeID
	default:
		d.cfg.NodeID, err = opaqueID("node")
		if err != nil {
			return nil, err
		}
		if err := spool.update(func(state *durableState) error { state.NodeID = d.cfg.NodeID; return nil }); err != nil {
			return nil, err
		}
	}
	manager, err := agent.NewManager(ctx, d.emitManagerEvent, logger, filepath.Join(cfg.StateRoot, "logs"), agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{
			MaxConcurrent: cfg.Capacity, DefaultProvider: cfg.Providers[0], SandboxMode: cfg.SandboxMode,
			HeadlessSteerable: true,
		},
		OnComplete: d.completeAgent,
		OnReattach: func(a *agent.Agent) error {
			if a.HasAmbiguousSteerDispatch() {
				return errors.New("agentd: provider state is ambiguous after interrupted steer delivery")
			}
			return d.replayMissingOutput(a.TaskID, a)
		},
		ApprovalAddr:      approvals.Addr(),
		SurviveRestartDir: filepath.Join(cfg.StateRoot, "registry"),
		SandboxHome: func(taskID string) (string, error) {
			home := filepath.Join(cfg.StateRoot, "homes", taskID)
			return home, os.MkdirAll(home, 0o700)
		},
	})
	if err != nil {
		return nil, err
	}
	d.manager = manager
	approvals.SetManager(manager)
	manager.SetExecutionApprovalResponder(approvals.RespondApproval)
	if err := d.recoverRuns(ctx, state); err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		shutdownApprovalServer(ctx, approvals)
	}()
	keepApprovals = true
	return d, nil
}

func (d *Daemon) recoverRuns(ctx context.Context, state durableState) error {
	// Install the durable mapping first. A provider that completed while
	// agentd was down is finalized synchronously during reattachment, and its
	// callback must still resolve the protocol run that owns it.
	for runID, agentID := range state.RunAgents {
		d.runAgents[runID], d.agentRuns[agentID] = agentID, runID
	}
	recoveredIDs := make(map[string]struct{})
	for _, recovered := range d.manager.ReattachAllContext(ctx) {
		recoveredIDs[recovered.ID] = struct{}{}
	}
	// A mapping left now has neither a live provider nor a completion callback
	// that reconciled it. Report its fate instead of waiting forever.
	for runID, agentID := range d.spool.snapshot().RunAgents {
		if _, ok := recoveredIDs[agentID]; ok {
			continue
		}
		d.mu.Lock()
		delete(d.runAgents, runID)
		delete(d.agentRuns, agentID)
		d.mu.Unlock()
		if d.spool.hasTerminal(runID) {
			approvalIDs := d.spool.approvalIDs(runID)
			if err := d.spool.completeExistingRun(runID); err != nil {
				return err
			}
			d.approvals.DiscardStagedApprovals(approvalIDs)
			continue
		}
		if err := d.emitCompletion(runID, map[string]any{
			"state": executioncontract.TerminalFailed, "error": "provider process unavailable after daemon restart",
		}); err != nil {
			return fmt.Errorf("agentd: report unrecoverable run %s: %w", runID, err)
		}
	}
	return nil
}

func shutdownApprovalServer(parent context.Context, server *agent.ApprovalServer) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.register(ctx); err != nil {
		return err
	}
	d.pruneExpiredWorkspaces()
	heartbeat := time.NewTicker(time.Duration(max(d.cfg.LeaseSeconds/3, 1)) * time.Second)
	defer heartbeat.Stop()
	for {
		if err := d.flushEvents(ctx); err != nil {
			d.logger.Warn("agentd.events.retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			d.pruneExpiredWorkspaces()
			session, err := d.client.heartbeat(ctx, d.currentSession(), d.capabilities)
			if err != nil {
				d.logger.Warn("agentd.heartbeat", "err", err)
			} else {
				d.mu.Lock()
				d.draining = session.State == "draining"
				d.mu.Unlock()
			}
		default:
		}
		state := d.spool.snapshot()
		commands, err := d.client.poll(ctx, d.currentSession(), state.LastCommandAck, d.cfg.PollSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.logger.Warn("agentd.poll", "err", err)
			time.Sleep(time.Second)
			continue
		}
		for i := range commands {
			command := &commands[i]
			if err := d.handleCommand(ctx, *command); err != nil {
				d.logger.Error("agentd.command", "sequence", command.Sequence, "type", command.Envelope.Type, "err", err)
				break
			}
			// Persist the applied cursor before acknowledging it remotely. If the
			// process dies between the side effect and the HTTP acknowledgement,
			// registration resumes from this cursor instead of replaying control.
			if err := d.spool.update(func(state *durableState) error {
				state.LastCommandAck = command.Sequence
				return nil
			}); err != nil {
				d.logger.Error("agentd.command_cursor.persist", "sequence", command.Sequence, "err", err)
				break
			}
			if err := d.client.ackCommands(ctx, d.currentSession(), command.Sequence); err != nil {
				break
			}
		}
	}
}

func (d *Daemon) pruneExpiredWorkspaces() {
	before := time.Now().Add(-time.Duration(d.cfg.WorkspaceRetentionHours) * time.Hour)
	expired, err := d.spool.expireArtifacts(before)
	if err != nil {
		d.logger.Warn("agentd.workspace.prune", "err", err)
		return
	}
	for _, runID := range expired {
		_ = os.RemoveAll(filepath.Join(d.cfg.WorkspaceRoot, runID))
	}
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
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || protected[entry.Name()] {
			continue
		}
		info, statErr := entry.Info()
		if statErr == nil && info.ModTime().Before(before) {
			_ = os.RemoveAll(filepath.Join(d.cfg.WorkspaceRoot, entry.Name()))
		}
	}
}

func (d *Daemon) register(ctx context.Context) error {
	state := d.spool.snapshot()
	request := workercontrol.RegisterRequest{
		WorkerID: d.cfg.NodeID, Capabilities: d.capabilities, LeaseSeconds: d.cfg.LeaseSeconds,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: buildVersion(),
		},
		ResumeSessionID: state.SessionID, LastCommandAck: state.LastCommandAck,
	}
	session, err := d.client.register(ctx, request)
	if err != nil && state.SessionID != "" && len(d.manager.ListLiveAgents()) == 0 {
		request.ResumeSessionID, request.LastCommandAck = "", 0
		session, err = d.client.register(ctx, request)
	}
	if err != nil {
		return fmt.Errorf("agentd register: %w", err)
	}
	d.mu.Lock()
	d.sessionID, d.draining = session.SessionID, session.State == "draining"
	d.mu.Unlock()
	return d.spool.update(func(state *durableState) error {
		state.SessionID, state.LastCommandAck = session.SessionID, session.LastCommandAck
		return nil
	})
}

func (d *Daemon) handleCommand(ctx context.Context, command workercontrol.Command) error {
	envelope := command.Envelope
	if err := envelope.Validate(); err != nil {
		return err
	}
	return d.applyCommand(ctx, envelope)
}

func (d *Daemon) applyCommand(ctx context.Context, envelope executioncontract.CommandEnvelope) error {
	switch envelope.Type {
	case executioncontract.CommandStart:
		return d.start(ctx, envelope)
	case executioncontract.CommandStop:
		agentID, ok := d.agentForRun(envelope.RunID)
		if !ok {
			return nil
		}
		return d.manager.StopAgent(agentID)
	case executioncontract.CommandSteer:
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil || payload.Text == "" {
			return errors.New("agentd: steer command requires text")
		}
		agentID, ok := d.agentForRun(envelope.RunID)
		if !ok {
			return nil
		}
		return d.manager.SendMessageOnce(ctx, agentID, envelope.CommandID, payload.Text)
	case executioncontract.CommandApprovalResponse:
		var payload struct {
			ToolUseID string `json:"toolUseId"`
			Approved  bool   `json:"approved"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil || payload.ToolUseID == "" {
			return errors.New("agentd: approval response requires toolUseId")
		}
		d.approvalMu.Lock()
		defer d.approvalMu.Unlock()
		if _, ok := d.agentForRun(envelope.RunID); !ok {
			return nil
		}
		request := d.spool.snapshot().PendingApprovals[payload.ToolUseID]
		decision := durableApproval{RunID: envelope.RunID, Approved: payload.Approved, Fingerprint: request.Fingerprint}
		if err := d.spool.stageApproval(payload.ToolUseID, decision); err != nil {
			return err
		}
		return d.approvals.StageApproval(payload.ToolUseID, payload.Approved, request.Fingerprint)
	default:
		return fmt.Errorf("agentd: unsupported command %q", envelope.Type)
	}
}

func (d *Daemon) start(ctx context.Context, envelope executioncontract.CommandEnvelope) error {
	durable := d.spool.snapshot()
	d.mu.RLock()
	_, exists := d.runAgents[envelope.RunID]
	draining := d.draining
	d.mu.RUnlock()
	if exists {
		return nil
	}
	// A non-zero sequence proves this run was accepted or rejected durably.
	// Run IDs are single-use protocol identities, so replaying Start after its
	// mapping was removed would create a second provider execution.
	if durable.RunSequences[envelope.RunID] > 0 {
		return nil
	}
	if draining {
		return d.emitAdmissionFailure(envelope.RunID, map[string]any{
			"state": executioncontract.TerminalFailed, "error": "worker is draining",
		})
	}
	if d.manager.RunningCount() >= d.cfg.Capacity {
		return d.emitAdmissionFailure(envelope.RunID, map[string]any{
			"state": executioncontract.TerminalFailed, "error": agent.ErrMaxConcurrentReached.Error(),
		})
	}
	if errors.Is(d.spool.capacityError(), ErrSpoolExhausted) {
		return d.emitAdmissionFailure(envelope.RunID, map[string]any{
			"state": executioncontract.TerminalFailed, "error": ErrSpoolExhausted.Error(),
		})
	}
	var payload executioncontract.StartCommandPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil || payload.Spec == nil {
		return errors.New("agentd: start requires inline run spec")
	}
	spec := payload.Spec
	if err := spec.Validate(); err != nil {
		return err
	}
	source := d.cfg.Repositories[spec.Workspace.RepositoryID]
	layout, err := agentworkspace.Prepare(ctx, d.cfg.WorkspaceRoot, source, *spec)
	if err != nil {
		d.logger.Error("agentd.workspace.prepare", "run_id", spec.RunID, "err", err)
		return d.rejectStart(spec.RunID, errors.New("workspace preparation failed"))
	}
	keepWorkspace := false
	defer removeRunWorkspaceUnlessKept(layout.RunRoot, &keepWorkspace)
	runEnv, err := projectRunEnvironment(layout, *spec, d.cfg.SecretEnv)
	if err != nil {
		return d.rejectStart(spec.RunID, err)
	}
	grantEnv, err := d.projectRunGrant(ctx, layout, spec.RunID)
	if err != nil {
		return d.rejectStart(spec.RunID, err)
	}
	runEnv = append(runEnv, grantEnv)
	runCtx, cancel := context.WithDeadline(ctx, spec.Deadline)
	_, err = d.manager.RunContext(runCtx, agent.RunConfig{
		TaskID: spec.RunID, AdmissionTaskKey: spec.RunID, IntentID: spec.IdempotencyKey,
		TaskGeneration: spec.Fence.TaskGeneration, Name: spec.RunID, Role: agent.Role(spec.Role), Mode: "headless",
		Prompt: spec.Prompt.Text, OutputSchema: spec.Prompt.OutputSchema, AllowedTools: spec.Tools.AllowedTools,
		// The sandbox's one auxiliary write root covers this run's isolated
		// sidecar/artifact siblings; it never grants the shared workspace root.
		Dir: layout.Worktree, SidecarDir: layout.RunRoot, Provider: spec.Provider.Provider, Model: spec.Provider.Model, ReasoningEffort: spec.Provider.ReasoningEffort,
		RequirePermissions: spec.Tools.RequirePermissions, HeadlessPermissionMode: spec.Tools.PermissionMode,
		MaxTurns: spec.Resources.MaxTurns, BashTimeoutMs: spec.Resources.BashTimeoutMillis,
		HeadlessSteerable: spec.Options.Steerable, ForkSubagent: spec.Options.ForkSubagent,
		RetryWatchdog: spec.Options.RetryWatchdog, FallbackModel: spec.Options.FallbackModel,
		RequestedSkill: spec.Options.RequestedSkill, SkillExecutionMode: spec.Options.SkillExecutionMode,
		SeedWorkingMemory: spec.Options.SeedWorkingMemory, ResumeSessionID: spec.Options.ResumeSessionID,
		SandboxMode: d.cfg.SandboxMode, DisableProviderFailover: true, SkipDispatchJitter: true,
		ExtraEnv: runEnv, StripEnvKeys: d.secretEnvironmentKeys(), UnthrottledOutputEvents: true,
		BeforeStart: func(agentID string) error {
			d.mu.Lock()
			d.runAgents[spec.RunID], d.agentRuns[agentID] = agentID, spec.RunID
			d.runCancels[agentID] = cancel
			d.mu.Unlock()
			if err := d.spool.update(func(state *durableState) error {
				state.RunAgents[spec.RunID] = agentID
				state.RunSpecs[spec.RunID] = *spec
				return nil
			}); err != nil {
				return err
			}
			return d.emit(spec.RunID, executioncontract.EventStarted, map[string]any{"agentId": agentID})
		},
	})
	if err != nil {
		cancel()
		d.mu.Lock()
		agentID := d.runAgents[spec.RunID]
		delete(d.runAgents, spec.RunID)
		delete(d.agentRuns, agentID)
		delete(d.runCancels, agentID)
		d.mu.Unlock()
		emitErr := d.emitCompletion(spec.RunID, map[string]any{"state": executioncontract.TerminalFailed, "error": err.Error()})
		if emitErr != nil {
			return errors.Join(err, emitErr)
		}
		d.logger.Error("agentd.start.failed", "run_id", spec.RunID, "err", err)
		// The failure is now a durable terminal outcome. Treat the command as
		// handled so it is acknowledged rather than replayed into another attempt.
		return nil
	}
	keepWorkspace = true
	return nil
}

func removeRunWorkspaceUnlessKept(runRoot string, keep *bool) {
	if !*keep {
		_ = os.RemoveAll(runRoot)
	}
}

func (d *Daemon) projectRunGrant(ctx context.Context, layout agentworkspace.Layout, runID string) (string, error) {
	runGrant, err := d.issueGrant(ctx, d.currentSession(), runID)
	if err != nil {
		return "", errors.New("agentd: issue scoped run grant")
	}
	grantPath := filepath.Join(layout.RunRoot, "secrets", "run-grant")
	if err := os.MkdirAll(filepath.Dir(grantPath), 0o700); err != nil {
		return "", errors.New("agentd: create run grant directory")
	}
	if err := fsutil.AtomicWriteMode(grantPath, []byte(runGrant.Token), 0o400); err != nil {
		return "", errors.New("agentd: project scoped run grant")
	}
	return "SYBRA_RUN_GRANT_FILE=" + grantPath, nil
}

func projectRunEnvironment(layout agentworkspace.Layout, spec executioncontract.RunSpec, secretEnv map[string]string) ([]string, error) {
	runEnv := agentworkspace.Environment(layout)
	secretDir := filepath.Join(layout.RunRoot, "secrets")
	for _, binding := range spec.Environment {
		if binding.SecretRef == nil {
			runEnv = append(runEnv, binding.Name+"="+binding.Value)
			continue
		}
		envName := secretEnv[binding.SecretRef.Name]
		if envName == "" {
			return nil, fmt.Errorf("agentd: unresolved secret capability %q", binding.SecretRef.Name)
		}
		if err := os.MkdirAll(secretDir, 0o700); err != nil {
			return nil, errors.New("agentd: create protected run secret directory")
		}
		secretPath := filepath.Join(secretDir, binding.Name)
		if err := fsutil.AtomicWriteMode(secretPath, []byte(os.Getenv(envName)), 0o400); err != nil {
			return nil, errors.New("agentd: project protected run secret")
		}
		runEnv = append(runEnv, binding.Name+"_FILE="+secretPath)
	}
	return runEnv, nil
}

func (d *Daemon) secretEnvironmentKeys() []string {
	keys := make([]string, 0, len(d.cfg.SecretEnv)+1)
	keys = append(keys, d.cfg.TokenEnv)
	for _, envName := range d.cfg.SecretEnv {
		keys = append(keys, envName)
	}
	return keys
}

func (d *Daemon) rejectStart(runID string, cause error) error {
	emitErr := d.emitAdmissionFailure(runID, map[string]any{
		"state": executioncontract.TerminalFailed, "error": cause.Error(),
	})
	if emitErr != nil {
		return errors.Join(cause, emitErr)
	}
	d.logger.Error("agentd.start.rejected", "run_id", runID, "err", cause)
	return nil
}

func (d *Daemon) emitManagerEvent(name string, data any) {
	var kind executioncontract.EventType
	var agentID string
	payload := data
	switch {
	case strings.HasPrefix(name, agentevents.AgentOutputPrefix):
		kind = executioncontract.EventOutput
		agentID = strings.TrimPrefix(name, agentevents.AgentOutputPrefix)
	case strings.HasPrefix(name, agentevents.AgentApprovalPrefix):
		d.approvalMu.Lock()
		defer d.approvalMu.Unlock()
		kind = executioncontract.EventProgress
		agentID = strings.TrimPrefix(name, agentevents.AgentApprovalPrefix)
		payload = map[string]any{"kind": "approval_request", "request": data}
	default:
		return
	}
	d.mu.RLock()
	runID := d.agentRuns[agentID]
	d.mu.RUnlock()
	if runID != "" {
		var err error
		if request, ok := data.(agent.ApprovalRequest); kind == executioncontract.EventProgress && ok {
			err = d.emitApprovalRequest(runID, request.ToolUseID, request.Fingerprint, payload)
		} else {
			err = d.emit(runID, kind, payload)
		}
		if err != nil {
			d.logger.Error("agentd.event.persist", "run_id", runID, "type", kind, "err", err)
		}
	}
}

func (d *Daemon) completeAgent(a *agent.Agent) {
	d.approvalMu.Lock()
	defer d.approvalMu.Unlock()
	d.mu.Lock()
	runID := d.agentRuns[a.ID]
	cancel := d.runCancels[a.ID]
	delete(d.agentRuns, a.ID)
	delete(d.runAgents, runID)
	delete(d.runCancels, a.ID)
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if runID == "" {
		return
	}
	artifactState := executioncontract.ArtifactsFailed
	manifestID := ""
	if spec, ok := d.spool.snapshot().RunSpecs[runID]; ok && !errors.Is(a.GetExitErr(), ErrSpoolExhausted) {
		collectCtx, collectCancel := context.WithTimeout(context.Background(), 30*time.Second)
		layout := agentworkspace.Layout{
			RunRoot: filepath.Join(d.cfg.WorkspaceRoot, runID), Worktree: filepath.Join(d.cfg.WorkspaceRoot, runID, "worktree"),
			Sidecar: filepath.Join(d.cfg.WorkspaceRoot, runID, "sidecar"), Artifact: filepath.Join(d.cfg.WorkspaceRoot, runID, "artifact"),
			WorkingMemory: filepath.Join(d.cfg.WorkspaceRoot, runID, "worktree"),
		}
		manifest, content, collectErr := agentworkspace.Collect(collectCtx, layout, spec, buildVersion())
		collectCancel()
		if collectErr == nil {
			collectErr = d.spool.QueueArtifact(workercontrol.ArtifactUpload{Manifest: manifest, Content: content})
		}
		if collectErr != nil {
			d.logger.Error("agentd.artifact.collect", "run_id", runID, "err", collectErr)
			a.SetExitErr(errors.New("artifact collection failed"))
		} else {
			artifactState, manifestID = executioncontract.ArtifactsReady, manifest.ManifestID
		}
	}
	if !errors.Is(a.GetExitErr(), ErrSpoolExhausted) {
		if err := d.replayMissingOutput(runID, a); err != nil {
			d.logger.Error("agentd.output.recover", "run_id", runID, "err", err)
			return
		}
	}
	state := executioncontract.TerminalSucceeded
	errText := ""
	if err := a.GetExitErr(); errors.Is(err, ErrSpoolExhausted) {
		state, errText = executioncontract.TerminalFailed, err.Error()
	} else if err := a.GetExitErr(); err != nil {
		state = executioncontract.TerminalFailed
		errText = err.Error()
	} else if a.WasStopped() {
		state = executioncontract.TerminalCanceled
	} else if !a.CompletedSuccessfully() {
		state = executioncontract.TerminalFailed
	}
	terminalErr := d.emitCompletion(runID, map[string]any{
		"state": state, "error": errText, "artifactState": artifactState, "artifactManifestId": manifestID,
	})
	if terminalErr != nil {
		d.logger.Error("agentd.terminal.persist", "run_id", runID, "err", terminalErr)
		// Keep the durable ownership mapping. Startup reconciliation can retry a
		// compact terminal fate after acknowledged events free spool capacity.
		return
	}
}

func (d *Daemon) replayMissingOutput(runID string, a *agent.Agent) error {
	if runID == "" || a == nil {
		return nil
	}
	output := a.Output()
	delivered := d.spool.snapshot().OutputCounts[runID]
	if delivered > uint64(len(output)) {
		return fmt.Errorf("agentd: durable output cursor %d exceeds recovered output %d", delivered, len(output))
	}
	for i := delivered; i < uint64(len(output)); i++ {
		if err := d.emit(runID, executioncontract.EventOutput, output[i]); err != nil {
			return fmt.Errorf("agentd: replay recovered output %d: %w", i, err)
		}
	}
	return nil
}

func (d *Daemon) emit(runID string, kind executioncontract.EventType, payload any) error {
	return d.emitWith(runID, kind, payload, d.spool.appendEvent)
}

func (d *Daemon) emitApprovalRequest(runID, toolUseID, fingerprint string, payload any) error {
	return d.emitWith(runID, executioncontract.EventProgress, payload, func(event executioncontract.EventEnvelope) error {
		return d.spool.appendApprovalRequest(event, toolUseID, fingerprint)
	})
}

func (d *Daemon) emitCompletion(runID string, payload any) error {
	approvalIDs := d.spool.approvalIDs(runID)
	if err := d.emitWith(runID, executioncontract.EventTerminal, payload, d.spool.appendTerminalAndComplete); err != nil {
		return err
	}
	d.approvals.DiscardStagedApprovals(approvalIDs)
	return nil
}

func (d *Daemon) emitAdmissionFailure(runID string, payload any) error {
	return d.emitWith(runID, executioncontract.EventTerminal, payload, d.spool.appendAdmissionEvent)
}

func (d *Daemon) emitWith(runID string, kind executioncontract.EventType, payload any, appendEvent func(executioncontract.EventEnvelope) error) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	id, err := opaqueID("event")
	if err != nil {
		return err
	}
	event := executioncontract.EventEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: buildVersion(), RunID: runID,
		EventID: id, Type: kind,
		ObservedAt: time.Now().UTC(), Payload: body,
	}
	if err := appendEvent(event); err != nil {
		if errors.Is(err, ErrSpoolExhausted) {
			d.mu.RLock()
			agentID := d.runAgents[runID]
			d.mu.RUnlock()
			if agentID != "" {
				if active, getErr := d.manager.GetAgent(agentID); getErr == nil {
					active.SetExitErr(ErrSpoolExhausted)
				}
				_ = d.manager.StopAgent(agentID)
			}
		}
		return err
	}
	return nil
}

func (d *Daemon) flushEvents(ctx context.Context) error {
	state := d.spool.snapshot()
	for runID, events := range state.Events {
		if len(events) == 0 {
			continue
		}
		authorizations := make(map[string]workercontrol.RunActionRequest)
		for i := range events {
			event := &events[i]
			var progress struct {
				Kind string `json:"kind"`
			}
			if event.Type != executioncontract.EventProgress || json.Unmarshal(event.Payload, &progress) != nil || progress.Kind != "approval_request" {
				continue
			}
			spec, ok := state.RunSpecs[runID]
			if !ok {
				return errors.New("agentd: approval event has no run specification")
			}
			grantPath := filepath.Join(d.cfg.WorkspaceRoot, runID, "secrets", "run-grant")
			// Approval can arrive long after the start-time grant expired. Mint a
			// fresh bounded grant through the live worker session and rotate the
			// protected projection before presenting it.
			grant, grantErr := d.issueGrant(ctx, d.currentSession(), runID)
			if grantErr != nil {
				return errors.New("agentd: renew scoped run grant")
			}
			if writeErr := fsutil.AtomicWriteMode(grantPath, []byte(grant.Token), 0o400); writeErr != nil {
				return errors.New("agentd: rotate scoped run grant")
			}
			request := workercontrol.RunActionRequest{
				SessionID: d.currentSession(), Token: grant.Token, TaskID: spec.Fence.TaskID, EffectID: spec.EffectID,
				WorkflowGeneration: spec.Fence.WorkflowGeneration, Action: "approval.request", ReplayKey: event.IdempotencyKey,
			}
			authorizations[event.IdempotencyKey] = request
		}
		ack, err := d.client.events(ctx, workercontrol.EventBatch{SessionID: d.currentSession(), Events: events, Authorizations: authorizations})
		if err != nil {
			return err
		}
		if err := d.spool.ackEvents(runID, ack); err != nil {
			return err
		}
	}
	state = d.spool.snapshot()
	for manifestID := range state.Artifacts {
		upload := state.Artifacts[manifestID]
		upload.SessionID = d.currentSession()
		if err := d.client.artifact(ctx, upload); err != nil {
			return err
		}
		if err := d.spool.ackArtifact(manifestID); err != nil {
			return err
		}
		_ = os.RemoveAll(filepath.Join(d.cfg.WorkspaceRoot, upload.Manifest.RunID))
	}
	return nil
}

func (d *Daemon) currentSession() string { d.mu.RLock(); defer d.mu.RUnlock(); return d.sessionID }
func (d *Daemon) agentForRun(runID string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	id, ok := d.runAgents[runID]
	return id, ok
}

func buildVersion() string {
	if strings.TrimSpace(version.Version) == "" {
		return "dev"
	}
	return version.Version
}

func opaqueID(prefix string) (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(buf), nil
}
