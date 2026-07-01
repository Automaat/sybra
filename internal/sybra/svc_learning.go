package sybra

import (
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/learning"
)

// LearningService exposes the Learning Digest journal (internal/learning) as
// Wails-bound methods. ListDigests/GetLatestDigest are read-only and safe to
// expose over HTTP; StoreDigest is deliberately kept off the HTTP allowlist
// (see services.go) since digests are raw/unscrubbed until a caller invokes
// Digest.Scrub.
type LearningService struct {
	store *learning.Store
	emit  func(string, any)
}

// ListDigests returns every persisted digest, newest-first. Returns an empty
// slice (never nil) when the store failed to initialize, so the frontend can
// always range over the result.
func (s *LearningService) ListDigests() ([]learning.Digest, error) {
	if s.store == nil {
		return []learning.Digest{}, nil
	}
	digests, err := s.store.List()
	if err != nil {
		return nil, err
	}
	if digests == nil {
		digests = []learning.Digest{}
	}
	return digests, nil
}

// GetLatestDigest returns the most recently generated digest and whether one
// exists.
func (s *LearningService) GetLatestDigest() (learning.Digest, bool, error) {
	if s.store == nil {
		return learning.Digest{}, false, nil
	}
	return s.store.Latest()
}

// StoreDigest persists d and emits events.LearningSummary only when it was
// actually a new digest (not a deduplicated repeat of an existing key).
func (s *LearningService) StoreDigest(d learning.Digest) (bool, error) {
	if s.store == nil {
		return false, nil
	}
	stored, err := s.store.Put(d)
	if err != nil {
		return stored, err
	}
	if stored && s.emit != nil {
		s.emit(events.LearningSummary, d)
	}
	return stored, nil
}
