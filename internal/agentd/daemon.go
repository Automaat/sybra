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
	agentevents "github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/version"
	"github.com/Automaat/sybra/internal/workercontrol"
)

type Daemon struct {
	cfg          Config
	logger       *slog.Logger
	client       *leaderClient
	spool        *Spool
	manager      *agent.Manager
	approvals    *agent.ApprovalServer
	capabilities []string

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
	spool, err := OpenSpool(cfg.StateRoot, cfg.SpoolMaxBytes)
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
		OnComplete:        d.completeAgent,
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
		if err := d.emit(runID, executioncontract.EventTerminal, map[string]any{
			"state": executioncontract.TerminalFailed, "error": "provider process unavailable after daemon restart",
		}); err != nil {
			return fmt.Errorf("agentd: report unrecoverable run %s: %w", runID, err)
		}
		if err := d.spool.update(func(state *durableState) error { delete(state.RunAgents, runID); return nil }); err != nil {
			return err
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
				for commandID, sequence := range state.HandledCommands {
					if sequence <= command.Sequence {
						delete(state.HandledCommands, commandID)
					}
				}
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
	if envelope.Type != executioncontract.CommandStart {
		alreadyHandled, err := d.beginControlCommand(command.Envelope.CommandID, command.Sequence)
		if err != nil || alreadyHandled {
			return err
		}
	}
	err := d.applyCommand(ctx, envelope)
	if err != nil && envelope.Type != executioncontract.CommandStart {
		if clearErr := d.clearControlCommand(command.Envelope.CommandID); clearErr != nil {
			return errors.Join(err, clearErr)
		}
	}
	return err
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
		return d.manager.SendMessage(agentID, payload.Text)
	case executioncontract.CommandApprovalResponse:
		var payload struct {
			ToolUseID string `json:"toolUseId"`
			Approved  bool   `json:"approved"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil || payload.ToolUseID == "" {
			return errors.New("agentd: approval response requires toolUseId")
		}
		agentID, ok := d.agentForRun(envelope.RunID)
		if !ok {
			return nil
		}
		return d.manager.RespondExecutionApproval(agentID, payload.ToolUseID, payload.Approved)
	default:
		return fmt.Errorf("agentd: unsupported command %q", envelope.Type)
	}
}

// beginControlCommand durably claims a one-shot control effect before it is
// applied. A replay after a crash is acknowledged without repeating steering,
// signaling, or an approval decision. Failed effects clear the claim so the
// leader can retry them.
func (d *Daemon) beginControlCommand(commandID string, sequence uint64) (bool, error) {
	already := false
	err := d.spool.update(func(state *durableState) error {
		_, already = state.HandledCommands[commandID]
		if !already {
			state.HandledCommands[commandID] = sequence
		}
		return nil
	})
	return already, err
}

func (d *Daemon) clearControlCommand(commandID string) error {
	return d.spool.update(func(state *durableState) error {
		delete(state.HandledCommands, commandID)
		return nil
	})
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
		return d.emit(envelope.RunID, executioncontract.EventTerminal, map[string]any{
			"state": executioncontract.TerminalFailed, "error": "worker is draining",
		})
	}
	if d.manager.RunningCount() >= d.cfg.Capacity {
		return d.emit(envelope.RunID, executioncontract.EventTerminal, map[string]any{
			"state": executioncontract.TerminalFailed, "error": agent.ErrMaxConcurrentReached.Error(),
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
	workspace := filepath.Join(d.cfg.WorkspaceRoot, spec.RunID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return d.rejectStart(spec.RunID, err)
	}
	runEnv := make([]string, 0, len(spec.Environment))
	for _, binding := range spec.Environment {
		value := binding.Value
		if binding.SecretRef != nil {
			envName := d.cfg.SecretEnv[binding.SecretRef.Name]
			if envName == "" {
				return d.rejectStart(spec.RunID, fmt.Errorf("agentd: unresolved secret capability %q", binding.SecretRef.Name))
			}
			value = os.Getenv(envName)
		}
		runEnv = append(runEnv, binding.Name+"="+value)
	}
	runCtx, cancel := context.WithDeadline(ctx, spec.Deadline)
	_, err := d.manager.RunContext(runCtx, agent.RunConfig{
		TaskID: spec.RunID, AdmissionTaskKey: spec.RunID, IntentID: spec.IdempotencyKey,
		TaskGeneration: spec.Fence.TaskGeneration, Name: spec.RunID, Role: agent.Role(spec.Role), Mode: "headless",
		Prompt: spec.Prompt.Text, OutputSchema: spec.Prompt.OutputSchema, AllowedTools: spec.Tools.AllowedTools,
		Dir: workspace, Provider: spec.Provider.Provider, Model: spec.Provider.Model, ReasoningEffort: spec.Provider.ReasoningEffort,
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
			if err := d.spool.update(func(state *durableState) error { state.RunAgents[spec.RunID] = agentID; return nil }); err != nil {
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
		emitErr := d.emit(spec.RunID, executioncontract.EventTerminal, map[string]any{"state": executioncontract.TerminalFailed, "error": err.Error()})
		cleanupErr := d.spool.update(func(state *durableState) error { delete(state.RunAgents, spec.RunID); return nil })
		if emitErr != nil || cleanupErr != nil {
			return errors.Join(err, emitErr, cleanupErr)
		}
		d.logger.Error("agentd.start.failed", "run_id", spec.RunID, "err", err)
		// The failure is now a durable terminal outcome. Treat the command as
		// handled so it is acknowledged rather than replayed into another attempt.
		return nil
	}
	return nil
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
	emitErr := d.emit(runID, executioncontract.EventTerminal, map[string]any{
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
		if err := d.emit(runID, kind, payload); err != nil {
			d.logger.Error("agentd.event.persist", "run_id", runID, "type", kind, "err", err)
		}
	}
}

func (d *Daemon) completeAgent(a *agent.Agent) {
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
	state := executioncontract.TerminalSucceeded
	errText := ""
	if a.WasStopped() {
		state = executioncontract.TerminalCanceled
	} else if err := a.GetExitErr(); err != nil || !a.CompletedSuccessfully() {
		state = executioncontract.TerminalFailed
		if err != nil {
			errText = err.Error()
		}
	}
	terminalErr := d.emit(runID, executioncontract.EventTerminal, map[string]any{"state": state, "error": errText})
	if terminalErr != nil {
		d.logger.Error("agentd.terminal.persist", "run_id", runID, "err", terminalErr)
		// Keep the durable ownership mapping. Startup reconciliation can retry a
		// compact terminal fate after acknowledged events free spool capacity.
		return
	}
	if err := d.spool.update(func(state *durableState) error { delete(state.RunAgents, runID); return nil }); err != nil {
		d.logger.Error("agentd.run_mapping.persist", "run_id", runID, "err", err)
	}
}

func (d *Daemon) emit(runID string, kind executioncontract.EventType, payload any) error {
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
	if err := d.spool.appendEvent(event); err != nil {
		if errors.Is(err, ErrSpoolExhausted) {
			d.mu.RLock()
			agentID := d.runAgents[runID]
			d.mu.RUnlock()
			if agentID != "" {
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
		ack, err := d.client.events(ctx, workercontrol.EventBatch{SessionID: d.currentSession(), Events: events})
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
