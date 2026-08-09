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
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	TaskID    string    `json:"taskId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

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
	if s == nil {
		return "", errors.New("agent grants: store is not configured")
	}
	if strings.TrimSpace(taskID) == "" {
		return "", errors.New("agent grants: a grant needs a task")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agent grants: read randomness: %w", err)
	}
	token := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now().UTC())
	s.grants[digest(token)] = Grant{TaskID: taskID, ExpiresAt: time.Now().UTC().Add(s.ttl)}
	if err := s.persistLocked(); err != nil {
		delete(s.grants, digest(token))
		return "", err
	}
	return token, nil
}

// Verify resolves a presented credential.
//
// The comparison is constant-time so a caller cannot learn a stored digest by
// timing, and a lapsed grant is refused rather than pruned here — pruning takes
// the write path, and a read must not become one.
func (s *Store) Verify(token string) (Grant, bool) {
	if s == nil || strings.TrimSpace(token) == "" {
		return Grant{}, false
	}
	want := digest(token)
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	for stored, grant := range s.grants {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(want)) != 1 {
			continue
		}
		if now.After(grant.ExpiresAt) {
			return Grant{}, false
		}
		return grant, true
	}
	return Grant{}, false
}

// Revoke drops every grant issued for a task, which is what the end of its run
// should do rather than waiting for the expiry.
func (s *Store) Revoke(taskID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for stored, grant := range s.grants {
		if grant.TaskID == taskID {
			delete(s.grants, stored)
		}
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

func (s *Store) pruneLocked(now time.Time) {
	for stored, grant := range s.grants {
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
