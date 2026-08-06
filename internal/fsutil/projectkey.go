package fsutil

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// ProjectKeyDir maps a project id ("owner/repo") to one filesystem-safe
// directory name. The owner and repo are hex-encoded rather than sanitized so
// that two distinct projects can never collide on the same directory — a
// character-stripping sanitizer would map "a/b-c" and "a/b_c" together.
//
// Work-project keys arrive already opaque (see IsOpaqueWorkProjectKey) and are
// passed through: they are a hash, so they carry no path structure and must
// not be re-encoded, or the record written under one name is read under
// another.
func ProjectKeyDir(projectID string) (string, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return "", fmt.Errorf("project id is empty")
	}
	if IsOpaqueWorkProjectKey(id) {
		return id, nil
	}
	if filepath.Clean(id) != id || strings.Contains(id, `\`) {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	owner, repo, ok := strings.Cut(id, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	if owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	return "gh-" + hex.EncodeToString([]byte(owner)) + "-" + hex.EncodeToString([]byte(repo)), nil
}

// IsOpaqueWorkProjectKey reports whether id is a scrubbed work-project key:
// "work-" followed by 64 lowercase hex digits. Work project ids are hashed
// before they reach disk so a work repo's owner/name never lands in a path.
func IsOpaqueWorkProjectKey(id string) bool {
	const prefix = "work-"
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+64 {
		return false
	}
	for _, r := range id[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
