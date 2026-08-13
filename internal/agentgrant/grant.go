// Package agentgrant issues the credential a task-scoped agent uses to reach
// its board.
//
// An agent used to be handed the board's own bearer token. That is the whole
// control plane's credential: revoking it means rotating the board and
// restarting every other client, so in practice nobody revokes it; it never
// expires and was stored in the clear; and it authenticates as an operator, so
// it reaches every method rather than the task operations an agent performs.
//
// A grant is the shape the verifier control channel already used: random,
// stored only as a digest, expiring, and revoked when the run that needed it
// ends.
package agentgrant

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
)

// DefaultTTL bounds a grant that is never revoked, which is what a process
// killed mid-run leaves behind.
const DefaultTTL = 24 * time.Hour

// Grant is what a presented credential resolves to.
type Grant struct {
	TaskID             string    `json:"taskId"`
	RunID              string    `json:"runId,omitempty"`
	EffectID           string    `json:"effectId,omitempty"`
	WorkflowGeneration int64     `json:"workflowGeneration,omitempty"`
	AllowedActions     []string  `json:"allowedActions,omitempty"`
	ExpiresAt          time.Time `json:"expiresAt"`
	UsedReplayKeys     []string  `json:"usedReplayKeys,omitempty"`
}

type Use struct {
	TaskID             string
	RunID              string
	EffectID           string
	WorkflowGeneration int64
	Action             string
	ReplayKey          string
}

type AuditEvent struct {
	Kind               string
	TaskID             string
	RunID              string
	EffectID           string
	WorkflowGeneration int64
	Action             string
	Allowed            bool
}

type AuditSink func(AuditEvent)

var (
	ErrUnauthorized = errors.New("agent grants: credential is invalid, expired, or revoked")
	ErrOutOfScope   = errors.New("agent grants: request is outside grant scope")
	ErrReplay       = errors.New("agent grants: replayed request")
)

// Store issues and verifies per-run credentials.
//
// Only digests are held. An operator reading the store, or a backup of it,
// learns which runs hold a credential and when it lapses, never a credential
// they could present.
type Store struct {
	path string
	ttl  time.Duration

	mu     sync.Mutex
	grants map[string]Grant
	audit  AuditSink
}

// New opens the store at path, creating its directory. A blank path keeps the
// grants in memory only, which a restart then discards — every live run
// re-mints on its next dispatch.
func New(path string, ttl time.Duration) (*Store, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	s := &Store{path: strings.TrimSpace(path), ttl: ttl, grants: map[string]Grant{}}
	if s.path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("agent grants: mkdir: %w", err)
	}
	loaded, err := load(s.path)
	if err != nil {
		return nil, err
	}
	s.grants = loaded
	return s, nil
}

// Mint returns a fresh credential for one task and stores its digest.
func (s *Store) Mint(taskID string) (string, error) {
	return s.MintScoped(Grant{TaskID: taskID})
}

func (s *Store) SetAuditSink(sink AuditSink) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

// MintScoped issues one short-lived capability. Actions are exact decoded API
// operation names (for example "task.get"), never URL prefixes.
func (s *Store) MintScoped(scope Grant) (string, error) {
	if s == nil {
		return "", errors.New("agent grants: store is not configured")
	}
	if strings.TrimSpace(scope.TaskID) == "" {
		return "", errors.New("agent grants: a grant needs a task")
	}
	if scope.RunID != "" && (strings.TrimSpace(scope.EffectID) == "" || len(scope.AllowedActions) == 0) {
		return "", errors.New("agent grants: scoped grants need run, effect, and actions")
	}
	for i := range scope.AllowedActions {
		scope.AllowedActions[i] = strings.TrimSpace(scope.AllowedActions[i])
		if scope.AllowedActions[i] == "" {
			return "", errors.New("agent grants: actions cannot be empty")
		}
	}
	slices.Sort(scope.AllowedActions)
	scope.AllowedActions = slices.Compact(scope.AllowedActions)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agent grants: read randomness: %w", err)
	}
	token := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now().UTC())
	now := time.Now().UTC()
	scope.ExpiresAt = now.Add(s.ttl)
	scope.UsedReplayKeys = nil
	s.grants[digest(token)] = scope
	if err := s.persistLocked(); err != nil {
		delete(s.grants, digest(token))
		return "", err
	}
	s.auditLocked(AuditEvent{Kind: "grant.issued", TaskID: scope.TaskID, RunID: scope.RunID, EffectID: scope.EffectID, WorkflowGeneration: scope.WorkflowGeneration, Allowed: true})
	return token, nil
}

// Verify resolves a presented credential.
//
// The lookup is keyed by the digest of the presented token, so a caller that does not already hold a token cannot reach a stored grant without a SHA-256 preimage, and no scan over stored digests is needed. A lapsed grant is refused rather than pruned here — pruning takes the write path, and a read must not become one.
func (s *Store) Verify(token string) (Grant, bool) {
	if s == nil || strings.TrimSpace(token) == "" {
		return Grant{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[digest(token)]
	if !ok {
		return Grant{}, false
	}
	if time.Now().UTC().After(grant.ExpiresAt) {
		return Grant{}, false
	}
	return grant, true
}

// Authorize validates scope only after the caller has decoded the request into
// an exact Use. Successful replay keys are consumed durably; a lost response
// cannot be submitted again under the same grant.
func (s *Store) Authorize(token string, use Use) error {
	if s == nil || strings.TrimSpace(token) == "" {
		return ErrUnauthorized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[digest(token)]
	event := AuditEvent{Kind: "grant.used", TaskID: use.TaskID, RunID: use.RunID, EffectID: use.EffectID, WorkflowGeneration: use.WorkflowGeneration, Action: use.Action}
	if !ok || !time.Now().UTC().Before(grant.ExpiresAt) {
		s.auditLocked(event)
		return ErrUnauthorized
	}
	if grant.TaskID != use.TaskID || grant.RunID != use.RunID || grant.EffectID != use.EffectID ||
		grant.WorkflowGeneration != use.WorkflowGeneration || !slices.Contains(grant.AllowedActions, use.Action) {
		s.auditLocked(event)
		return ErrOutOfScope
	}
	if strings.TrimSpace(use.ReplayKey) == "" {
		s.auditLocked(event)
		return ErrOutOfScope
	}
	if slices.Contains(grant.UsedReplayKeys, use.ReplayKey) {
		s.auditLocked(event)
		return ErrReplay
	}
	grant.UsedReplayKeys = append(grant.UsedReplayKeys, use.ReplayKey)
	s.grants[digest(token)] = grant
	if err := s.persistLocked(); err != nil {
		return err
	}
	event.Allowed = true
	s.auditLocked(event)
	return nil
}

// ReleaseReplay compensates an authorization whose surrounding durable
// operation failed. It only removes the named replay key from the exact grant.
func (s *Store) ReleaseReplay(token, replayKey string) error {
	if s == nil || strings.TrimSpace(token) == "" || strings.TrimSpace(replayKey) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := digest(token)
	grant, ok := s.grants[stored]
	if !ok {
		return nil
	}
	grant.UsedReplayKeys = slices.DeleteFunc(grant.UsedReplayKeys, func(key string) bool { return key == replayKey })
	s.grants[stored] = grant
	return s.persistLocked()
}

// Revoke drops every grant issued for a task, which is what the end of its run
// should do rather than waiting for the expiry.
func (s *Store) Revoke(taskID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for stored := range s.grants {
		grant := s.grants[stored]
		if grant.TaskID == taskID {
			s.auditLocked(AuditEvent{Kind: "grant.revoked", TaskID: grant.TaskID, RunID: grant.RunID, EffectID: grant.EffectID, WorkflowGeneration: grant.WorkflowGeneration, Allowed: true})
			delete(s.grants, stored)
		}
	}
	return s.persistLocked()
}

func (s *Store) RevokeRun(runID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for stored := range s.grants {
		grant := s.grants[stored]
		if grant.RunID == runID {
			s.auditLocked(AuditEvent{Kind: "grant.revoked", TaskID: grant.TaskID, RunID: grant.RunID, EffectID: grant.EffectID, WorkflowGeneration: grant.WorkflowGeneration, Allowed: true})
			delete(s.grants, stored)
		}
	}
	return s.persistLocked()
}

// RevokeToken drops one grant by the raw credential held by its issuer. It is
// used to roll back a mint when the surrounding durable operation fails; the
// token itself is never persisted by the store.
func (s *Store) RevokeToken(token string) error {
	if s == nil || strings.TrimSpace(token) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := digest(token)
	if grant, ok := s.grants[stored]; ok {
		s.auditLocked(AuditEvent{Kind: "grant.revoked", TaskID: grant.TaskID, RunID: grant.RunID, EffectID: grant.EffectID, WorkflowGeneration: grant.WorkflowGeneration, Allowed: true})
		delete(s.grants, stored)
	}
	return s.persistLocked()
}

// Prune drops lapsed grants.
func (s *Store) Prune() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now().UTC())
	return s.persistLocked()
}

func (s *Store) auditLocked(event AuditEvent) {
	if s.audit != nil {
		s.audit(event)
	}
}

func (s *Store) pruneLocked(now time.Time) {
	for stored := range s.grants {
		grant := s.grants[stored]
		if now.After(grant.ExpiresAt) {
			delete(s.grants, stored)
		}
	}
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.grants, "", "  ")
	if err != nil {
		return fmt.Errorf("agent grants: encode: %w", err)
	}
	if err := fsutil.AtomicWriteMode(s.path, data, 0o600); err != nil {
		return fmt.Errorf("agent grants: write: %w", err)
	}
	return nil
}

// load reads the stored digests.
//
// A store this build cannot decode starts empty rather than failing: every live
// run then re-mints on its next dispatch, which costs a dispatch instead of the
// whole board.
func load(path string) (map[string]Grant, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Grant{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent grants: read: %w", err)
	}
	grants := map[string]Grant{}
	if len(data) == 0 {
		return grants, nil
	}
	if decodeErr := json.Unmarshal(data, &grants); decodeErr != nil {
		grants = map[string]Grant{}
	}
	return grants, nil
}

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
