package workercontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/executioncontract"
)

var ErrNoEligibleWorker = errors.New("worker control: no eligible worker")

// PlacementRequest is leader-owned policy applied to a validated start. A
// NodeOverride is a hard pin. Legacy AssignedNode metadata is an affinity so
// existing boards retain locality without turning a temporarily absent node
// into a permanent dispatch failure.
type PlacementRequest struct {
	Spec                    executioncontract.RunSpec         `json:"spec"`
	Command                 executioncontract.CommandEnvelope `json:"command"`
	NodeOverride            string                            `json:"nodeOverride,omitempty"`
	AssignedNode            string                            `json:"assignedNode,omitempty"`
	AllowAffinityFallback   bool                              `json:"allowAffinityFallback"`
	AllowLocalFallback      bool                              `json:"allowLocalFallback"`
	WorkType                string                            `json:"workType,omitempty"`
	RequireTrusted          bool                              `json:"requireTrusted,omitempty"`
	RequireEncrypted        bool                              `json:"requireEncrypted,omitempty"`
	OS                      string                            `json:"os,omitempty"`
	Architecture            string                            `json:"architecture,omitempty"`
	Labels                  map[string]string                 `json:"labels,omitempty"`
	Sandbox                 string                            `json:"sandbox,omitempty"`
	WarmCacheHints          []string                          `json:"warmCacheHints,omitempty"`
	RequireRepositoryAnchor bool                              `json:"requireRepositoryAnchor,omitempty"`
	// RequireVerifierAuth keeps independent review agents off workers that
	// cannot supply Sybra's restricted GitHub App credential. A local fallback
	// is safer than dispatching the run and discovering the missing capability
	// only after it has consumed a workflow retry.
	RequireVerifierAuth bool `json:"requireVerifierAuth,omitempty"`
	// WorkspaceBaseBundles are leader-only transport inputs keyed by the exact
	// repository HEAD advertised by a worker. Descriptors participate in stable
	// request identity; bytes do not. Worker control selects and persists one
	// variant atomically with the worker reservation.
	WorkspaceBaseBundles []WorkspaceBaseBundleInput `json:"workspaceBaseBundles,omitempty"`
}

type WorkspaceBaseBundleInput struct {
	RepositoryAnchor string                              `json:"repositoryAnchor"`
	Reference        *executioncontract.ContentReference `json:"reference,omitempty"`
	Content          []byte                              `json:"-"`
}

type PlacementCandidate struct {
	WorkerID string   `json:"workerId"`
	Eligible bool     `json:"eligible"`
	Score    int      `json:"score"`
	Reasons  []string `json:"reasons,omitempty"`
	Capacity int      `json:"capacity"`
	Active   int      `json:"active"`
}

type Placement struct {
	WorkerID      string               `json:"workerId,omitempty"`
	SessionID     string               `json:"sessionId,omitempty"`
	Command       Command              `json:"command,omitzero"`
	LocalFallback bool                 `json:"localFallback,omitempty"`
	Candidates    []PlacementCandidate `json:"candidates"`
}

type placementSession struct {
	workerID, sessionID string
	state               string
	capabilities        capabilitySet
	active              int
	candidate           PlacementCandidate
}

// ScheduleStart selects and reserves one worker in the same transaction that
// persists the fenced run and its start command. Session row locks serialize
// concurrent schedulers on postgres; sqlite uses its immediate transaction.
func (s *Service) ScheduleStart(ctx context.Context, request PlacementRequest) (Placement, error) {
	if err := validateStartDelivery(&request.Spec, request.Command); err != nil {
		return Placement{}, err
	}
	if err := validateWorkspaceBaseBundles(request); err != nil {
		return Placement{}, err
	}
	var result Placement
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		existing, ok, err := s.existingPlacementTx(ctx, tx, request)
		if err != nil {
			return err
		}
		if ok {
			result = existing
			return nil
		}
		sessions, err := s.placementSessionsTx(ctx, tx, request)
		if err != nil {
			return err
		}
		// Another scheduler may have inserted this effect while we waited for
		// the session locks. Recheck under those locks before reserving.
		existing, ok, err = s.existingPlacementTx(ctx, tx, request)
		if err != nil {
			return err
		}
		if ok {
			result = existing
			return nil
		}
		result.Candidates = make([]PlacementCandidate, 0, len(sessions))
		for i := range sessions {
			result.Candidates = append(result.Candidates, sessions[i].candidate)
		}
		eligible := make([]placementSession, 0, len(sessions))
		for i := range sessions {
			if sessions[i].candidate.Eligible {
				eligible = append(eligible, sessions[i])
			}
		}
		if len(eligible) == 0 {
			if request.AllowLocalFallback && strings.TrimSpace(request.NodeOverride) == "" {
				local, err := s.reserveLocalFallbackTx(ctx, tx, request, result.Candidates)
				result = local
				return err
			}
			return ErrNoEligibleWorker
		}
		sort.Slice(eligible, func(i, j int) bool {
			if eligible[i].candidate.Score != eligible[j].candidate.Score {
				return eligible[i].candidate.Score > eligible[j].candidate.Score
			}
			if eligible[i].workerID != eligible[j].workerID {
				return eligible[i].workerID < eligible[j].workerID
			}
			return eligible[i].sessionID < eligible[j].sessionID
		})
		selected := eligible[0]
		command, err := s.reserveStartTx(ctx, tx, selected, request)
		if err != nil {
			return err
		}
		result.WorkerID, result.SessionID, result.Command = selected.workerID, selected.sessionID, command
		return nil
	})
	if err != nil {
		return result, err
	}
	if result.SessionID != "" {
		s.notify()
	}
	return result, nil
}

func validateWorkspaceBaseBundle(ref *executioncontract.ContentReference, content []byte) error {
	if ref == nil {
		if len(content) != 0 {
			return invalidf("workspace base bundle has no contract reference")
		}
		return nil
	}
	if int64(len(content)) != ref.SizeBytes || int64(len(content)) > executioncontract.MaxWorkspaceBaseBundleSize {
		return invalidf("workspace base bundle size differs from contract")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	if !strings.EqualFold(digest, ref.DigestSHA256) {
		return invalidf("workspace base bundle digest differs from contract")
	}
	return nil
}

func validateWorkspaceBaseBundles(request PlacementRequest) error {
	seen := make(map[string]struct{}, len(request.WorkspaceBaseBundles))
	for i := range request.WorkspaceBaseBundles {
		input := &request.WorkspaceBaseBundles[i]
		anchor := strings.TrimSpace(input.RepositoryAnchor)
		if !validRepositoryAnchor(anchor) {
			return invalidf("workspace base bundle repository anchor is invalid")
		}
		if _, ok := seen[anchor]; ok {
			return invalidf("workspace base bundle repository anchor is duplicated")
		}
		seen[anchor] = struct{}{}
		if input.Reference != nil && input.Reference.ID != request.Spec.RunID {
			return invalidf("workspace base bundle belongs to another run")
		}
		if err := validateWorkspaceBaseBundle(input.Reference, input.Content); err != nil {
			return err
		}
	}
	return nil
}

func validRepositoryAnchor(anchor string) bool {
	if len(anchor) != 40 && len(anchor) != 64 {
		return false
	}
	_, err := hex.DecodeString(anchor)
	return err == nil
}

func workspaceBaseBundleFor(session placementSession, request PlacementRequest) (WorkspaceBaseBundleInput, bool) {
	anchor := session.capabilities.one("repository_head:" + request.Spec.Workspace.RepositoryID)
	for i := range request.WorkspaceBaseBundles {
		if request.WorkspaceBaseBundles[i].RepositoryAnchor == anchor {
			return request.WorkspaceBaseBundles[i], true
		}
	}
	return WorkspaceBaseBundleInput{}, false
}

func (s *Service) placementSessionsTx(ctx context.Context, tx *sql.Tx, request PlacementRequest) ([]placementSession, error) {
	query := `SELECT worker_id, session_id, state, capabilities_json FROM worker_sessions WHERE state IN ('active', 'draining', 'disabled') AND lease_expires_at > ? ORDER BY worker_id, session_id`
	if s.db.Dialect() == db.Postgres {
		query += ` FOR UPDATE`
	}
	rows, err := tx.QueryContext(ctx, s.db.Rebind(query), db.TimeValue(s.now().UTC()))
	if err != nil {
		return nil, err
	}
	var out []placementSession
	for rows.Next() {
		var item placementSession
		var encoded string
		if err := rows.Scan(&item.workerID, &item.sessionID, &item.state, &encoded); err != nil {
			return nil, err
		}
		var values []string
		if err := json.Unmarshal([]byte(encoded), &values); err != nil {
			return nil, err
		}
		item.capabilities = parseCapabilities(values)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Count reservations in a fresh statement after the session locks are
	// acquired. On PostgreSQL, folding this count into the FOR UPDATE query
	// would retain that statement's earlier READ COMMITTED snapshot after a
	// lock wait and could miss the reservation committed by the lock holder.
	activeRows, err := tx.QueryContext(ctx, `SELECT session_id, COUNT(*) FROM remote_runs WHERE state != 'terminal' GROUP BY session_id`)
	if err != nil {
		return nil, err
	}
	activeBySession := make(map[string]int)
	for activeRows.Next() {
		var sessionID string
		var active int
		if err := activeRows.Scan(&sessionID, &active); err != nil {
			_ = activeRows.Close()
			return nil, err
		}
		activeBySession[sessionID] = active
	}
	if err := activeRows.Err(); err != nil {
		_ = activeRows.Close()
		return nil, err
	}
	if err := activeRows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].active = activeBySession[out[i].sessionID]
		out[i].candidate = scorePlacement(out[i], request)
	}
	return out, nil
}

func scorePlacement(session placementSession, request PlacementRequest) PlacementCandidate {
	c := PlacementCandidate{WorkerID: session.workerID, Eligible: true, Capacity: session.capabilities.capacity, Active: session.active}
	reject := func(reason string) { c.Eligible = false; c.Reasons = append(c.Reasons, reason) }
	if session.state != "active" {
		reject("worker is " + session.state)
	}
	hardPin, affinity := strings.TrimSpace(request.NodeOverride), strings.TrimSpace(request.AssignedNode)
	if hardPin != "" && session.workerID != hardPin {
		reject("hard node pin targets another worker")
	}
	if hardPin == "" && affinity != "" && session.workerID != affinity && !request.AllowAffinityFallback {
		reject("affinity fallback is disabled")
	}
	if session.capabilities.capacity <= session.active {
		reject("advertised capacity is exhausted")
	}
	trusted := request.RequireTrusted || strings.EqualFold(request.WorkType, "work")
	if trusted && !session.capabilities.flags["trusted_work"] {
		reject("trusted work capability is required")
	}
	if request.RequireEncrypted && !session.capabilities.flags["encrypted_work"] {
		reject("encrypted work capability is required")
	}
	if request.RequireVerifierAuth && !session.capabilities.flags["verifier_auth"] {
		reject("verifier authentication capability is required")
	}
	if request.RequireRepositoryAnchor {
		if !session.capabilities.flags["workspace_base_bundle"] {
			reject("workspace base bundle capability is required")
		} else if _, ok := workspaceBaseBundleFor(session, request); !ok {
			reject("workspace base bundle does not match repository anchor")
		}
	}
	provider := request.Spec.Provider.Provider
	if !session.capabilities.has("provider", provider) {
		reject("provider is unavailable")
	}
	if session.capabilities.one("provider_health:"+provider) != "healthy" {
		reject("provider is unhealthy")
	}
	if models := session.capabilities.values["model"]; len(models) > 0 && !slices.Contains(models, request.Spec.Provider.Model) {
		reject("model is unavailable")
	}
	for key, want := range map[string]string{"os": request.OS, "arch": request.Architecture} {
		if want != "" && session.capabilities.one(key) != want {
			reject(key + " capability does not match")
		}
	}
	labelKeys := make([]string, 0, len(request.Labels))
	for key := range request.Labels {
		labelKeys = append(labelKeys, key)
	}
	slices.Sort(labelKeys)
	for _, key := range labelKeys {
		want := request.Labels[key]
		if session.capabilities.one("label:"+key) != want {
			reject("label " + key + " does not match")
		}
	}
	if request.Sandbox == "enforce" && session.capabilities.one("sandbox") != "enforce" {
		reject("enforced sandbox is required")
	}
	if affinity != "" && session.workerID == affinity {
		c.Score += 1000
	}
	if session.capabilities.has("repository", request.Spec.Workspace.RepositoryID) {
		c.Score += 100
	}
	for _, hint := range request.WarmCacheHints {
		if session.capabilities.has("cache", hint) {
			c.Score += 20
		}
	}
	c.Score += max(session.capabilities.capacity-session.active, 0)
	return c
}

func (s *Service) existingPlacementTx(ctx context.Context, tx *sql.Tx, request PlacementRequest) (Placement, bool, error) {
	var decision, decisionRunID, workerID, sessionID, requestJSON string
	err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT decision, run_id, COALESCE(worker_id, ''), COALESCE(session_id, ''), request_json FROM run_placement_decisions WHERE effect_id = ?`), request.Spec.EffectID).
		Scan(&decision, &decisionRunID, &workerID, &sessionID, &requestJSON)
	if err == nil {
		encodedRequest, _ := json.Marshal(request)
		if decisionRunID != request.Spec.RunID || requestJSON != string(encodedRequest) {
			return Placement{}, false, invalidf("effect id already belongs to another fenced placement")
		}
		if decision == "local" {
			return Placement{LocalFallback: true}, true, nil
		}
		var sequence uint64
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT sequence FROM worker_commands WHERE run_id = ? AND command_type = ? ORDER BY sequence LIMIT 1`), decisionRunID, executioncontract.CommandStart).Scan(&sequence); err != nil {
			return Placement{}, false, err
		}
		return Placement{WorkerID: workerID, SessionID: sessionID, Command: Command{Sequence: sequence, Envelope: request.Command}}, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Placement{}, false, err
	}
	var runID, legacyWorkerID, legacySessionID, specJSON string
	err = tx.QueryRowContext(ctx, s.db.Rebind(`SELECT run_id, worker_id, session_id, run_spec_json FROM remote_runs WHERE effect_id = ?`), request.Spec.EffectID).
		Scan(&runID, &legacyWorkerID, &legacySessionID, &specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Placement{}, false, nil
	}
	if err != nil {
		return Placement{}, false, err
	}
	encodedSpec, _ := json.Marshal(request.Spec)
	if runID != request.Spec.RunID || specJSON != string(encodedSpec) {
		return Placement{}, false, invalidf("effect id already belongs to another fenced run")
	}
	var sequence uint64
	var payload string
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT sequence, payload_json FROM worker_commands WHERE run_id = ? AND command_type = ? ORDER BY sequence LIMIT 1`), runID, executioncontract.CommandStart).
		Scan(&sequence, &payload); err != nil {
		return Placement{}, false, err
	}
	encodedCommand, _ := json.Marshal(request.Command)
	if payload != string(encodedCommand) {
		return Placement{}, false, invalidf("fenced run start command differs")
	}
	return Placement{WorkerID: legacyWorkerID, SessionID: legacySessionID, Command: Command{Sequence: sequence, Envelope: request.Command}}, true, nil
}

func (s *Service) reserveLocalFallbackTx(ctx context.Context, tx *sql.Tx, request PlacementRequest, candidates []PlacementCandidate) (Placement, error) {
	specJSON, err := json.Marshal(request.Spec)
	if err != nil {
		return Placement{}, err
	}
	commandJSON, err := json.Marshal(request.Command)
	if err != nil {
		return Placement{}, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return Placement{}, err
	}
	result, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO run_placement_decisions
		(effect_id, run_id, decision, run_spec_json, command_json, request_json, created_at)
		VALUES (?, ?, 'local', ?, ?, ?, ?) ON CONFLICT(effect_id) DO NOTHING`), request.Spec.EffectID, request.Spec.RunID,
		string(specJSON), string(commandJSON), string(requestJSON), db.TimeValue(s.now().UTC()))
	if err != nil {
		return Placement{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		existing, ok, existingErr := s.existingPlacementTx(ctx, tx, request)
		if existingErr != nil {
			return Placement{}, existingErr
		}
		if !ok || !existing.LocalFallback {
			return Placement{}, invalidf("effect placement conflict")
		}
		existing.Candidates = candidates
		return existing, nil
	}
	return Placement{LocalFallback: true, Candidates: candidates}, nil
}

func (s *Service) reserveStartTx(ctx context.Context, tx *sql.Tx, selected placementSession, request PlacementRequest) (Command, error) {
	selectedSpec := request.Spec
	var selectedBundle []byte
	if len(request.WorkspaceBaseBundles) > 0 {
		input, ok := workspaceBaseBundleFor(selected, request)
		if !ok {
			return Command{}, invalidf("selected worker has no matching workspace base bundle")
		}
		selectedSpec.Workspace.BaseBundle = input.Reference
		selectedSpec.Workspace.RepositoryAnchor = input.RepositoryAnchor
		selectedBundle = input.Content
	}
	selectedCommand := request.Command
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &selectedSpec})
	if err != nil {
		return Command{}, err
	}
	selectedCommand.Payload = payload
	if err := validateStartDelivery(&selectedSpec, selectedCommand); err != nil {
		return Command{}, err
	}
	specJSON, err := json.Marshal(selectedSpec)
	if err != nil {
		return Command{}, err
	}
	commandJSON, err := json.Marshal(selectedCommand)
	if err != nil {
		return Command{}, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return Command{}, err
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO run_placement_decisions
		(effect_id, run_id, decision, worker_id, session_id, run_spec_json, command_json, request_json, created_at)
		VALUES (?, ?, 'remote', ?, ?, ?, ?, ?, ?)`), request.Spec.EffectID, request.Spec.RunID, selected.workerID, selected.sessionID,
		string(specJSON), string(commandJSON), string(requestJSON), db.TimeValue(s.now().UTC())); err != nil {
		return Command{}, err
	}
	_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO remote_runs
		(run_id, worker_id, session_id, effect_id, task_id, task_generation, workflow_id, workflow_generation, step_id, run_spec_json, state, updated_at, workspace_base_bundle)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)`), request.Spec.RunID, selected.workerID, selected.sessionID,
		selectedSpec.EffectID, selectedSpec.Fence.TaskID, selectedSpec.Fence.TaskGeneration, selectedSpec.Fence.WorkflowID,
		selectedSpec.Fence.WorkflowGeneration, selectedSpec.Fence.StepID, string(specJSON), db.TimeValue(s.now().UTC()), nullableBytes(selectedBundle))
	if err != nil {
		return Command{}, err
	}
	var lastAck, maximum uint64
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT last_command_ack FROM worker_sessions WHERE session_id = ?`), selected.sessionID).Scan(&lastAck); err != nil {
		return Command{}, err
	}
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(MAX(sequence), 0) FROM worker_commands WHERE session_id = ?`), selected.sessionID).Scan(&maximum); err != nil {
		return Command{}, err
	}
	sequence := max(lastAck, maximum) + 1
	_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_commands
		(session_id, sequence, command_id, run_id, idempotency_key, command_type, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), selected.sessionID, sequence, selectedCommand.CommandID, selectedSpec.RunID,
		selectedCommand.IdempotencyKey, selectedCommand.Type, string(commandJSON), db.TimeValue(s.now().UTC()))
	return Command{Sequence: sequence, Envelope: selectedCommand}, err
}

// RepositoryAnchors returns the exact repository HEADs advertised by live,
// active workers. The opaque repository identity stays on the leader; daemon
// paths and remotes never cross the protocol boundary.
func (s *Service) RepositoryAnchors(ctx context.Context, repositoryID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`SELECT capabilities_json FROM worker_sessions WHERE state = 'active' AND lease_expires_at > ? ORDER BY worker_id, session_id`), db.TimeValue(s.now().UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	unique := make(map[string]struct{})
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var values []string
		if err := json.Unmarshal([]byte(encoded), &values); err != nil {
			return nil, err
		}
		capabilities := parseCapabilities(values)
		if !capabilities.has("repository", repositoryID) {
			continue
		}
		if anchor := capabilities.one("repository_head:" + repositoryID); validRepositoryAnchor(anchor) {
			unique[anchor] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	anchors := make([]string, 0, len(unique))
	for anchor := range unique {
		anchors = append(anchors, anchor)
	}
	slices.Sort(anchors)
	return anchors, nil
}

func nullableBytes(content []byte) any {
	if len(content) == 0 {
		return nil
	}
	return content
}

type capabilitySet struct {
	values   map[string][]string
	flags    map[string]bool
	capacity int
}

func parseCapabilities(raw []string) capabilitySet {
	set := capabilitySet{values: map[string][]string{}, flags: map[string]bool{}}
	for _, item := range raw {
		key, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			continue
		}
		set.values[key] = append(set.values[key], value)
		if value == "true" {
			set.flags[key] = true
		}
		if key == "capacity" {
			set.capacity, _ = strconv.Atoi(value)
		}
	}
	return set
}

func (c capabilitySet) one(key string) string {
	if len(c.values[key]) == 0 {
		return ""
	}
	return c.values[key][0]
}

func (c capabilitySet) has(key, value string) bool { return slices.Contains(c.values[key], value) }

func (p Placement) String() string {
	if p.LocalFallback {
		return "local fallback selected"
	}
	if p.WorkerID != "" {
		return fmt.Sprintf("worker %s reserved", p.WorkerID)
	}
	return "no placement"
}
