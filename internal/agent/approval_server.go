package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/toolledger"
)

// VerifierControlServiceMarker identifies the verifier control channel in its
// health response. It is distinct from httpserve.ServiceMarker so a client that
// merely inferred this port declines it rather than committing the board's
// token to a peer that serves two methods for one task.
const VerifierControlServiceMarker = "sybra-verifier-control"

// ApprovalServer runs an HTTP server that handles PreToolUse hook requests
// from Claude CLI. When a tool needs approval, the hook POSTs to this server,
// which emits a Wails event to the frontend and blocks until the user responds.
type ApprovalServer struct {
	mu                sync.Mutex
	pending           map[string]chan ApprovalResponse // keyed by tool_use_id
	staged            map[string]ApprovalResponse      // durable remote decisions awaiting a hook retry
	emit              EmitFunc
	logger            *slog.Logger
	server            *http.Server
	listener          net.Listener
	agents            *Manager
	verifierTokens    map[string]verifierTokenRecord // sha256(token) -> scoped, expiring grant
	verifierTokenPath string
	verifierControl   http.Handler
}

const verifierTokenTTL = 24 * time.Hour

const verifierGrantFile = ".sybra-control-grant"

type verifierTokenRecord struct {
	TaskID      string    `json:"taskId"`
	SandboxHome string    `json:"sandboxHome,omitempty"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// VerifierControlTokenPath is the lease-private credential file consumed by
// sybra-cli. Keeping the bearer out of the process environment prevents
// same-user process inspection from recovering another verifier's grant.
func VerifierControlTokenPath(sandboxHome string) string {
	return filepath.Join(sandboxHome, verifierGrantFile)
}

// NewApprovalServer creates and starts the approval HTTP server. port pins
// the localhost port (so a detached agent's baked approval-hook URL still
// resolves after a restart); 0 binds a random port. Durable app wiring persists
// that selection and fails closed when its pinned endpoint cannot be rebound.
func NewApprovalServer(ctx context.Context, emit EmitFunc, logger *slog.Logger, port int) (*ApprovalServer, error) {
	return newApprovalServer(ctx, emit, logger, port, "", "")
}

// NewDurableApprovalServer persists an automatically selected loopback port
// before serving, so detached agents retain both their endpoint and verifier
// credential across restarts.
func NewDurableApprovalServer(ctx context.Context, emit EmitFunc, logger *slog.Logger, port int, portPath, tokenPath string) (*ApprovalServer, error) {
	return newApprovalServer(ctx, emit, logger, port, portPath, tokenPath)
}

func newApprovalServer(ctx context.Context, emit EmitFunc, logger *slog.Logger, port int, portPath, tokenPath string) (*ApprovalServer, error) {
	if port == 0 && portPath != "" {
		if data, err := os.ReadFile(portPath); err == nil {
			persisted, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || persisted < 1 || persisted > 65535 {
				return nil, fmt.Errorf("invalid persisted approval port %q", strings.TrimSpace(string(data)))
			}
			port = persisted
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read persisted approval port: %w", err)
		}
	}
	addr := "127.0.0.1:0"
	if port > 0 {
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	if portPath != "" {
		tcpAddr, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			_ = listener.Close()
			return nil, fmt.Errorf("approval listener has unexpected address type %T", listener.Addr())
		}
		actualPort := tcpAddr.Port
		if err := os.MkdirAll(filepath.Dir(portPath), 0o700); err != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("create approval port state: %w", err)
		}
		if err := fsutil.AtomicWriteMode(portPath, []byte(strconv.Itoa(actualPort)+"\n"), 0o600); err != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("persist approval port: %w", err)
		}
	}
	tokens, err := loadVerifierTokens(tokenPath)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("load verifier control tokens: %w", err)
	}

	s := &ApprovalServer{
		pending:           make(map[string]chan ApprovalResponse),
		staged:            make(map[string]ApprovalResponse),
		emit:              emit,
		logger:            logger,
		listener:          listener,
		verifierTokens:    tokens,
		verifierTokenPath: tokenPath,
	}

	mux := http.NewServeMux()
	// The verifier's own CLI probes this before it will send its task-scoped
	// token, and an empty body is indistinguishable from a process that is not
	// Sybra at all — so every verifier CRUD call refused.
	//
	// Its own marker, deliberately not the board's: this channel fronts two
	// TaskService methods for one task. A client that inferred this port and
	// took it for a board would send the board's token and then 404 on
	// everything it asked for, so it has to be able to tell them apart.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","service":%q}`, VerifierControlServiceMarker)
	})
	mux.HandleFunc("/hooks/pre-tool-use", s.handlePreToolUse)
	mux.HandleFunc("POST /api/TaskService/{method}", s.handleVerifierControl)

	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("approval-server.serve", "err", err)
		}
	}()

	logger.Info("approval-server.start", "addr", listener.Addr().String())
	return s, nil
}

// VerifierToken creates a random task-scoped bearer credential. Only its hash
// is persisted, so unrestricted read posture cannot reveal a master secret or
// forge another task's credential.
func (s *ApprovalServer) VerifierToken(taskID, sandboxHome string) string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	token := hex.EncodeToString(raw)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	sandboxHome = strings.TrimSpace(sandboxHome)
	if sandboxHome != "" {
		sandboxHome = filepath.Clean(sandboxHome)
	}
	s.mu.Lock()
	s.pruneVerifierTokensLocked(time.Now().UTC())
	s.verifierTokens[digest] = verifierTokenRecord{TaskID: taskID, SandboxHome: sandboxHome, ExpiresAt: time.Now().UTC().Add(verifierTokenTTL)}
	err := s.persistVerifierTokensLocked()
	if err != nil {
		delete(s.verifierTokens, digest)
	}
	s.mu.Unlock()
	if err != nil {
		s.logger.Error("approval-server.verifier-token", "task_id", taskID, "err", err)
		return ""
	}
	return token
}

func loadVerifierTokens(path string) (map[string]verifierTokenRecord, error) {
	tokens := make(map[string]verifierTokenRecord)
	if path == "" {
		return tokens, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return tokens, nil
	}
	if err != nil {
		return nil, err
	}
	migrated := false
	if err := json.Unmarshal(data, &tokens); err != nil {
		// The first hash-only store encoded digest -> task ID but carried no
		// trusted sandbox identity. It cannot be safely revoked on completion,
		// so invalidate it during upgrade rather than preserve an unowned grant.
		var legacy map[string]string
		if legacyErr := json.Unmarshal(data, &legacy); legacyErr != nil {
			return nil, err
		}
		tokens = make(map[string]verifierTokenRecord)
		migrated = true
	}
	if tokens == nil {
		tokens = make(map[string]verifierTokenRecord)
	}
	now := time.Now().UTC()
	pruned := false
	for digest, record := range tokens {
		if record.TaskID == "" || record.SandboxHome == "" || !now.Before(record.ExpiresAt) {
			delete(tokens, digest)
			pruned = true
		}
	}
	if migrated || pruned {
		normalized, marshalErr := json.Marshal(tokens)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if writeErr := fsutil.AtomicWriteMode(path, normalized, 0o600); writeErr != nil {
			return nil, writeErr
		}
	}
	return tokens, nil
}

func (s *ApprovalServer) persistVerifierTokensLocked() error {
	if s.verifierTokenPath == "" {
		return nil
	}
	data, err := json.Marshal(s.verifierTokens)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteMode(s.verifierTokenPath, data, 0o600)
}

func (s *ApprovalServer) pruneVerifierTokensLocked(now time.Time) {
	for digest, record := range s.verifierTokens {
		if record.TaskID == "" || record.SandboxHome == "" || !now.Before(record.ExpiresAt) {
			delete(s.verifierTokens, digest)
		}
	}
}

// RevokeVerifierToken invalidates one completed verifier's grant.
func (s *ApprovalServer) RevokeVerifierToken(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	s.mu.Lock()
	delete(s.verifierTokens, digest)
	if err := s.persistVerifierTokensLocked(); err != nil {
		s.logger.Error("approval-server.verifier-token-revoke", "err", err)
	}
	s.mu.Unlock()
}

// RevokeVerifierGrantForSandbox invalidates grants using the trusted scratch
// path persisted at issuance time. The verifier can mutate its credential
// file, so completion must never rely on reading that file back.
func (s *ApprovalServer) RevokeVerifierGrantForSandbox(sandboxHome string) error {
	sandboxHome = filepath.Clean(strings.TrimSpace(sandboxHome))
	if sandboxHome == "." || sandboxHome == "" {
		return errors.New("verifier grant revocation requires a sandbox home")
	}
	s.mu.Lock()
	for digest, record := range s.verifierTokens {
		if record.SandboxHome == sandboxHome {
			delete(s.verifierTokens, digest)
		}
	}
	if err := s.persistVerifierTokensLocked(); err != nil {
		s.logger.Error("approval-server.verifier-grant-revoke", "sandbox_home", sandboxHome, "err", err)
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	return nil
}

// SetVerifierControl installs the already-allowlisted TaskService dispatcher.
func (s *ApprovalServer) SetVerifierControl(handler http.Handler) {
	s.mu.Lock()
	s.verifierControl = handler
	s.mu.Unlock()
}

func (s *ApprovalServer) handleVerifierControl(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	if method != "GetTask" && method != "UpdateTask" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var args []json.RawMessage
	if err := json.Unmarshal(body, &args); err != nil || len(args) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var taskID string
	if err := json.Unmarshal(args[0], &taskID); err != nil || strings.TrimSpace(taskID) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(bearer)))
	s.mu.Lock()
	record, tokenOK := s.verifierTokens[digest]
	s.mu.Unlock()
	if !ok || !tokenOK || record.TaskID != taskID || record.SandboxHome == "" || !time.Now().UTC().Before(record.ExpiresAt) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	handler := s.verifierControl
	s.mu.Unlock()
	if handler == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	handler.ServeHTTP(w, r)
}

// Addr returns the listener address (e.g., "127.0.0.1:54321").
func (s *ApprovalServer) Addr() string {
	return s.listener.Addr().String()
}

// SetManager sets the agent manager reference for resolving agent IDs from tool_use_ids.
func (s *ApprovalServer) SetManager(m *Manager) {
	s.agents = m
}

// Shutdown gracefully stops the HTTP server.
func (s *ApprovalServer) Shutdown(ctx context.Context) error {
	err := s.server.Shutdown(ctx)
	if closeErr := s.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return errors.Join(err, closeErr)
	}
	return err
}

// hookInput matches the JSON the Claude CLI sends to PreToolUse hooks.
type hookInput struct {
	SessionID      string         `json:"session_id"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	ToolUseID      string         `json:"tool_use_id"`
	CWD            string         `json:"cwd"`
	PermissionMode string         `json:"permission_mode"`
}

// hookResponse is what we return to the Claude CLI hook.
type hookResponse struct {
	HookSpecificOutput hookOutput `json:"hookSpecificOutput"`
}

type hookOutput struct {
	HookEventName            string         `json:"hookEventName"`
	PermissionDecision       string         `json:"permissionDecision"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
}

func (s *ApprovalServer) handlePreToolUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input hookInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		s.logger.Error("approval-server.decode", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.logger.Info("approval-server.request", "tool", input.ToolName, "tool_use_id", input.ToolUseID)

	// Auto-approve safe read-only tools regardless of session.
	if isSafeTool(input.ToolName) {
		// Resolve best-effort only: safe tools are allowed even when the
		// session is unknown, but recording them without an agent would strip
		// task/role/provider from the most common approvals in the ledger.
		s.recordDecision(s.findAgentBySession(input.SessionID), input, "allow", "safe-tool")
		s.respondAllow(w, input.ToolInput)
		return
	}

	// Find the agent by session_id; deny if unknown. The stdout parser sets
	// SessionID from the init event on a separate goroutine, with no ordering
	// guarantee against the CLI's first tool call reaching this hook. Retry
	// briefly on a miss before denying so that race doesn't fail the first
	// tool call of an otherwise healthy run.
	agentID := s.findAgentBySessionWithRetry(r.Context(), input.SessionID)
	if agentID == "" {
		s.logger.Warn("approval-server.no-agent", "session_id", input.SessionID)
		s.recordApprovalFailure(nil, input, "approval-unknown-session", "Unknown session")
		s.respondDeny(w, "Unknown session")
		return
	}

	// Create pending approval channel.
	ch := make(chan ApprovalResponse, 1)
	s.mu.Lock()
	s.pending[input.ToolUseID] = ch
	if staged, ok := s.staged[input.ToolUseID]; ok {
		ch <- staged
		delete(s.staged, input.ToolUseID)
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, input.ToolUseID)
		s.mu.Unlock()
	}()

	// Emit approval request to frontend.
	req := ApprovalRequest{
		ToolUseID: input.ToolUseID,
		ToolName:  input.ToolName,
		Input:     input.ToolInput,
	}
	s.emit(events.AgentApproval(agentID), req)

	// Update agent state to paused, flagged as awaiting tool approval.
	if s.agents != nil {
		if a, err := s.agents.GetAgent(agentID); err == nil {
			a.SetAwaitingApproval(true)
			a.SetState(StatePaused)
			s.emit(events.AgentState(agentID), a)
		}
	}

	// Restore agent state on all exit paths (approve, deny, canceled, timeout).
	defer func() {
		if s.agents != nil {
			if a, err := s.agents.GetAgent(agentID); err == nil {
				a.SetAwaitingApproval(false)
				a.SetState(StateRunning)
				s.emit(events.AgentState(agentID), a)
			}
		}
	}()

	// Block until user responds or context is cancelled.
	select {
	case resp := <-ch:
		if resp.Approved {
			s.recordDecision(agentID, input, "allow", "human")
			s.respondAllow(w, input.ToolInput)
		} else {
			s.recordDecision(agentID, input, "deny", "human")
			s.recordApprovalFailureByID(agentID, input, "approval-denied", "User denied this action")
			s.respondDeny(w, "User denied this action")
		}
	case <-r.Context().Done():
		s.recordApprovalFailureByID(agentID, input, "approval-canceled", "Request canceled")
		s.respondDeny(w, "Request canceled")
	case <-time.After(5 * time.Minute):
		s.recordApprovalFailureByID(agentID, input, "approval-timeout", "Approval timed out")
		s.respondDeny(w, "Approval timed out")
	}
}

func (s *ApprovalServer) recordApprovalFailureByID(agentID string, input hookInput, source, reason string) {
	var a *Agent
	if s.agents != nil && agentID != "" {
		if got, err := s.agents.GetAgent(agentID); err == nil {
			a = got
		}
	}
	s.recordApprovalFailure(a, input, source, reason)
}

func (s *ApprovalServer) recordApprovalFailure(a *Agent, input hookInput, source, reason string) {
	agentID := ""
	if a != nil {
		agentID = a.ID
	}
	logApprovalToolFailure(s.logger, agentID, input.ToolName, input.ToolUseID, source, reason)
	if s.agents == nil {
		return
	}
	s.agents.recordToolCallFailure(a, ToolCallFailureRecord{
		Timestamp:        time.Now().UTC(),
		SessionID:        input.SessionID,
		ToolUseID:        input.ToolUseID,
		ToolName:         input.ToolName,
		ToolInputSummary: summarizeToolInput(input.ToolInput),
		Source:           source,
		Reason:           reason,
	})
}

// recordDecision writes an adjudicated tool call to the ledger. Only
// refusals were ever recorded before, which describes what a human rejected
// and nothing about the far larger set they waved through — the wrong half to
// keep when the point is to derive a policy from observed behaviour.
func (s *ApprovalServer) recordDecision(agentID string, input hookInput, decision, by string) {
	if s == nil || s.agents == nil {
		return
	}
	rec := toolledger.Record{
		AgentID:   agentID,
		Tool:      input.ToolName,
		ToolUseID: input.ToolUseID,
		Input:     input.ToolInput,
		Decision:  decision,
		DecidedBy: by,
	}
	if a, err := s.agents.GetAgent(agentID); err == nil && a != nil {
		rec.TaskID = a.TaskID
		rec.Role = string(a.EffectiveRole())
		rec.Provider = a.Provider
	}
	s.agents.recordToolCall(rec)
}

func (s *ApprovalServer) respondAllow(w http.ResponseWriter, input map[string]any) {
	writeHookResponse(w, hookOutput{
		HookEventName:      "PreToolUse",
		PermissionDecision: "allow",
		UpdatedInput:       input,
	})
}

func (s *ApprovalServer) respondDeny(w http.ResponseWriter, reason string) {
	writeHookResponse(w, hookOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	})
}

func writeHookResponse(w http.ResponseWriter, out hookOutput) {
	data, err := json.Marshal(hookResponse{HookSpecificOutput: out})
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// RespondApproval handles a user's approval/denial decision from the frontend.
// Send is non-blocking: a double-click race where the handler has already
// consumed the channel but the deferred cleanup hasn't run yet would
// otherwise deadlock the UI thread on the buffered (size 1) channel.
func (s *ApprovalServer) RespondApproval(toolUseID string, approved bool) error {
	s.mu.Lock()
	ch, ok := s.pending[toolUseID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending approval for tool_use_id %s", toolUseID)
	}
	select {
	case ch <- ApprovalResponse{ToolUseID: toolUseID, Approved: approved}:
		return nil
	default:
		return fmt.Errorf("approval for tool_use_id %s already consumed or channel full", toolUseID)
	}
}

// StageApproval makes a remote approval decision retry-safe. If the hook is
// currently blocked it is delivered immediately; otherwise it is retained
// until the provider retries the same tool_use_id after daemon restart.
func (s *ApprovalServer) StageApproval(toolUseID string, approved bool) error {
	response := ApprovalResponse{ToolUseID: toolUseID, Approved: approved}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.staged[toolUseID]; ok {
		if prior.Approved != approved {
			return fmt.Errorf("conflicting approval decision for tool_use_id %s", toolUseID)
		}
		return nil
	}
	if ch, ok := s.pending[toolUseID]; ok {
		select {
		case ch <- response:
			return nil
		default:
			return nil
		}
	}
	s.staged[toolUseID] = response
	return nil
}

// findAgentBySession resolves the agent a PreToolUse hook request came from.
// Session IDs are provider-issued and unique per run, so this matches
// regardless of Mode: a headless run only reaches here when it was started
// with RequirePermissions (see buildClaudeHookSettings), and must resolve to
// its agent the same way an interactive session does.
//
// The registry is never pruned, so a rate-limited retry that resumes a prior
// session ID (RescheduleRateLimitedAgent) leaves the stopped original agent
// registered with the same SessionID as the new live one. First-match over a
// map is nondeterministic and could resolve to the dead original, hanging the
// live retry. Resolve deterministically instead: prefer a non-stopped agent,
// and among ties prefer the most-recently-started, so a live retry always
// wins over the stale entry it reused a session ID from.
func (s *ApprovalServer) findAgentBySession(sessionID string) string {
	if s.agents == nil {
		return ""
	}
	var best *Agent
	for _, a := range s.agents.ListAgents() {
		if a.GetSessionID() != sessionID {
			continue
		}
		if best == nil || betterSessionMatch(a, best) {
			best = a
		}
	}
	if best == nil {
		return ""
	}
	return best.ID
}

// sessionLookupRetries and sessionLookupBackoff bound the wait for the stdout
// parser to record SessionID before a first tool call is denied as unknown.
const (
	sessionLookupRetries = 20
	sessionLookupBackoff = 50 * time.Millisecond
)

// findAgentBySessionWithRetry resolves a session to an agent, retrying briefly
// to absorb the race where the CLI issues its first tool call before the
// stdout parser has recorded the session ID. Returns "" if still unresolved
// after the bounded wait or if the request is canceled.
func (s *ApprovalServer) findAgentBySessionWithRetry(ctx context.Context, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	for attempt := 0; ; attempt++ {
		if id := s.findAgentBySession(sessionID); id != "" {
			return id
		}
		if attempt >= sessionLookupRetries {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(sessionLookupBackoff):
		}
	}
}

// betterSessionMatch reports whether candidate should win over incumbent when
// both share a session ID: a live (non-stopped) agent beats a stopped one, and
// among agents of the same liveness the most-recently-started wins.
func betterSessionMatch(candidate, incumbent *Agent) bool {
	candStopped := candidate.GetState() == StateStopped
	incStopped := incumbent.GetState() == StateStopped
	if candStopped != incStopped {
		return !candStopped
	}
	candStarted := candidate.GetStartedAt()
	incStarted := incumbent.GetStartedAt()
	if !candStarted.Equal(incStarted) {
		return candStarted.After(incStarted)
	}
	return candidate.ID > incumbent.ID
}

// isSafeTool reports whether a tool is safe to auto-approve without a
// human prompt. MCP tools are never auto-approved here: their capabilities
// are arbitrary and server-defined, so a blanket "mcp__*" allow would let an
// injected agent reach dangerous or egress-capable tools unattended.
// WebFetch is excluded too — it's an egress-sensitive tool (attacker-supplied
// URL is a ready exfiltration channel for ingested content). WebSearch stays
// safe: it can only issue search queries, not fetch arbitrary attacker-chosen
// destinations.
func isSafeTool(name string) bool {
	safe := []string{"Read", "Glob", "Grep", "LSP", "WebSearch"}
	for _, s := range safe {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}
