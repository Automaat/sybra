package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscapesRoot reports a path that resolves outside the root it was joined
// under.
var ErrEscapesRoot = errors.New("path escapes root")

// ValidateKey rejects a string used as a single path component: the shape that
// a store derives a filename from. Callers pass externally-supplied keys —
// task ids pushed by a cluster peer, attachment names from an upload — so
// this rejects rather than sanitizes. Silently rewriting a hostile key hides
// the attempt and can still collide with a legitimate file.
func ValidateKey(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("key must not be empty")
	}
	if v == "." || v == ".." {
		return fmt.Errorf("key %q is a path traversal", v)
	}
	if strings.ContainsRune(v, 0) {
		return fmt.Errorf("key %q contains a NUL byte", v)
	}
	// Both separators, on every OS: a key crossing machines must not become
	// traversable only on the platform that treats "\" as a separator.
	if strings.ContainsAny(v, `/\`) || strings.ContainsRune(v, filepath.Separator) {
		return fmt.Errorf("key %q contains a path separator", v)
	}
	if filepath.IsAbs(v) || filepath.Base(v) != v {
		return fmt.Errorf("key %q is not a single path component", v)
	}
	return nil
}

// SafeJoin joins parts under root and returns the result only when it stays
// inside root. Every part must be a single component (see ValidateKey), so
// traversal is impossible by construction rather than caught after the fact
// by a string comparison on the joined path.
//
// When root exists, containment is re-checked through os.Root, which resolves
// each component against the real filesystem and so refuses a symlink that
// points outside — something neither a prefix test nor filepath.Rel can see.
func SafeJoin(root string, parts ...string) (string, error) {
	if root == "" {
		return "", errors.New("root must not be empty")
	}
	for _, p := range parts {
		if err := ValidateKey(p); err != nil {
			return "", err
		}
	}
	base := filepath.Clean(root)
	joined := filepath.Join(append([]string{base}, parts...)...)

	r, err := os.OpenRoot(base)
	if err != nil {
		// A not-yet-created root cannot host a symlink, and the components are
		// already validated, so the lexical join is sound. Any other error is
		// the caller's to see.
		if os.IsNotExist(err) {
			return joined, nil
		}
		return "", err
	}
	defer func() { _ = r.Close() }()

	rel, err := filepath.Rel(base, joined)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrEscapesRoot, joined)
	}
	// Stat resolves the whole chain against the real filesystem, so a
	// component that is a symlink out of the root fails here. A path that
	// simply does not exist yet is fine — callers are usually about to create
	// it. Note os.Root also refuses an absolute symlink target even when it
	// points back inside, which is stricter than containment requires but is
	// the safe direction.
	if _, err := r.Stat(rel); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("%w: %s: %w", ErrEscapesRoot, joined, err)
	}
	return joined, nil
}

// Within reports whether path resolves inside root. Prefer SafeJoin when you
// are building the path; this is for checking one you were handed.
//
// Purely lexical: it answers for the strings, not the filesystem, so it cannot
// see a symlink pointing out of root. Use it to reject obviously-escaping
// input, not as the only barrier around a hostile path.
func Within(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
