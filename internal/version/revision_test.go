package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestCleanRevision(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, injected, revision, modified, want string
	}{
		{"archive build", sha, "", "", sha},
		{"clean go run", "dev", sha, "false", sha},
		{"dirty build", sha, sha, "true", ""},
		{"mismatched injection", strings.Repeat("b", 40), sha, "false", ""},
		{"unknown", "dev", "", "", ""},
		{"ref is not proof", "main", "refs/heads/main", "false", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: tc.revision}, {Key: "vcs.modified", Value: tc.modified}}}
			if got := cleanRevision(tc.injected, info); got != tc.want {
				t.Fatalf("revision = %q, want %q", got, tc.want)
			}
		})
	}
	if got := cleanRevision(sha, nil); got != sha {
		t.Fatalf("archive without buildinfo = %q", got)
	}
}
