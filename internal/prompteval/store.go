package prompteval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Automaat/sybra/internal/fsutil"
)

// ErrNotFound is returned by Store.Read when no verdict exists for the given
// (variantID, digest) pair.
var ErrNotFound = errors.New("prompteval: verdict not found")

var validKey = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Store persists VariantVerdict JSON at <root>/<variantID>/<digest>.json.
// Mirrors internal/artifact/store.go's safety pattern: a strict key
// allowlist plus a path-containment check, and atomic temp-file+rename
// writes so a reader never observes a partial file.
type Store struct {
	root string
}

// New creates a Store rooted at dir. The directory is created on first write.
func New(dir string) *Store {
	return &Store{root: dir}
}

// validateKey rejects a variantID/digest that is empty, contains a path
// separator, "..", or any character outside [A-Za-z0-9._-].
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("prompteval: key must not be empty")
	}
	if !validKey.MatchString(key) {
		return fmt.Errorf("prompteval: invalid key %q", key)
	}
	if key == "." || key == ".." {
		return fmt.Errorf("prompteval: invalid key %q", key)
	}
	return nil
}

// normalizeDigest strips an optional "sha256:" prefix so callers that carry
// the abtest.Variant.Digest convention (e.g. "sha256:<hex>") and callers
// that pass raw prompteval.Digest output (bare hex) resolve to the same
// on-disk key.
func normalizeDigest(digest string) string {
	const prefix = "sha256:"
	if len(digest) > len(prefix) && strings.EqualFold(digest[:len(prefix)], prefix) {
		return digest[len(prefix):]
	}
	return digest
}

// verdictPath resolves and containment-checks the on-disk path for a
// (variantID, digest) pair.
func (s *Store) verdictPath(variantID, digest string) (string, error) {
	digest = normalizeDigest(digest)
	if err := validateKey(variantID); err != nil {
		return "", err
	}
	if err := validateKey(digest); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, variantID)
	path := filepath.Join(dir, digest+".json")
	root := filepath.Clean(s.root) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(path), root) {
		return "", fmt.Errorf("prompteval: key escapes store root")
	}
	return path, nil
}

// Write persists a verdict, creating the variant directory on first write.
func (s *Store) Write(v VariantVerdict) error {
	path, err := s.verdictPath(v.VariantID, v.Digest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prompteval: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("prompteval: marshal verdict: %w", err)
	}
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return fmt.Errorf("prompteval: write verdict: %w", err)
	}
	return nil
}

// Read loads a persisted verdict, returning ErrNotFound if none exists.
func (s *Store) Read(variantID, digest string) (VariantVerdict, error) {
	path, err := s.verdictPath(variantID, digest)
	if err != nil {
		return VariantVerdict{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return VariantVerdict{}, ErrNotFound
		}
		return VariantVerdict{}, fmt.Errorf("prompteval: read verdict: %w", err)
	}
	var v VariantVerdict
	if err := json.Unmarshal(data, &v); err != nil {
		return VariantVerdict{}, fmt.Errorf("prompteval: parse verdict: %w", err)
	}
	return v, nil
}
