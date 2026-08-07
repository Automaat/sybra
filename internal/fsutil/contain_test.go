package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		ok   bool
	}{
		{name: "plain id", key: "task-abc123", ok: true},
		{name: "dots inside a name", key: "report.v2.json", ok: true},
		{name: "unicode name", key: "café-résumé", ok: true},

		{name: "empty", key: ""},
		{name: "whitespace only", key: "   "},
		{name: "current dir", key: "."},
		{name: "parent dir", key: ".."},
		{name: "traversal prefix", key: "../etc"},
		{name: "traversal suffix", key: "a/.."},
		{name: "forward slash", key: "a/b"},
		{name: "backslash", key: `a\b`},
		{name: "absolute path", key: "/etc/passwd"},
		{name: "absolute traversal", key: "/../../etc/passwd"},
		{name: "nul byte", key: "a\x00b"},
		{name: "trailing separator", key: "dir/"},

		// A lookalike is not a separator, so it stays a legal single component
		// — the point is that filepath cannot mistake it for one.
		{name: "fullwidth solidus lookalike", key: "a／b", ok: true},
		{name: "division slash lookalike", key: "a∕b", ok: true},
		{name: "fullwidth full stops", key: "．．", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateKey(tt.key)
			if tt.ok && err != nil {
				t.Errorf("ValidateKey(%q) = %v, want nil", tt.key, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("ValidateKey(%q) = nil, want an error", tt.key)
			}
		})
	}
}

// A key that ValidateKey accepts must never produce a path outside the root,
// which is the property the per-package copies were each hand-rolling.
func TestValidateKeyAcceptedKeysStayInsideTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, key := range []string{"task-abc", "report.v2.json", "café", "a／b", "．．"} {
		if err := ValidateKey(key); err != nil {
			continue
		}
		joined := filepath.Join(root, key)
		if !Within(root, joined) {
			t.Errorf("ValidateKey accepted %q but %q escapes %q", key, joined, root)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if got, err := SafeJoin(root, "a", "b"); err != nil {
		t.Errorf("SafeJoin(root, a, b) = %v", err)
	} else if got != filepath.Join(root, "a", "b") {
		t.Errorf("SafeJoin = %q, want %q", got, filepath.Join(root, "a", "b"))
	}

	for _, parts := range [][]string{
		{".."}, {"..", ".."}, {"a", ".."}, {"a/b"}, {`a\b`}, {"/etc"}, {""}, {"."},
	} {
		if _, err := SafeJoin(root, parts...); err == nil {
			t.Errorf("SafeJoin(root, %q) = nil, want an error", parts)
		}
	}

	if _, err := SafeJoin("", "a"); err == nil {
		t.Error("SafeJoin with an empty root should fail")
	}

	// A root that does not exist yet cannot host a symlink, so the validated components make the lexical join sound.
	missing := filepath.Join(root, "not-created-yet")
	if got, err := SafeJoin(missing, "a"); err != nil {
		t.Errorf("SafeJoin under a missing root = %v", err)
	} else if got != filepath.Join(missing, "a") {
		t.Errorf("SafeJoin = %q, want %q", got, filepath.Join(missing, "a"))
	}
}

// The reason SafeJoin resolves through os.Root rather than comparing strings:
// a component can be a symlink whose target is outside the root, which every
// lexical check in this repo used to miss.
func TestSafeJoinRefusesASymlinkOutOfTheRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
			panic("unreachable")
		}
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	if _, err := SafeJoin(root, "escape"); err == nil {
		t.Error("SafeJoin followed a symlink pointing out of the root")
	}

	// Relative target: os.Root refuses an absolute symlink target outright,
	// even one pointing back inside, so only a relative link can be accepted.
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := os.Symlink("real", filepath.Join(root, "ok")); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := SafeJoin(root, "ok"); err != nil {
		t.Errorf("SafeJoin refused a relative symlink that stays inside the root: %v", err)
	}
}

func TestWithin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{name: "same path", root: "/a/b", path: "/a/b", want: true},
		{name: "child", root: "/a/b", path: "/a/b/c", want: true},
		{name: "deep child", root: "/a/b", path: "/a/b/c/d", want: true},
		{name: "unclean child", root: "/a/b", path: "/a/b/c/../d", want: true},
		{name: "parent", root: "/a/b", path: "/a"},
		{name: "sibling", root: "/a/b", path: "/a/bc"},
		{name: "escaping traversal", root: "/a/b", path: "/a/b/../../c"},
		{name: "unrelated", root: "/a/b", path: "/x/y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Within(tt.root, tt.path); got != tt.want {
				t.Errorf("Within(%q, %q) = %v, want %v", tt.root, tt.path, got, tt.want)
			}
		})
	}
}

// The sibling-prefix case is why Within uses filepath.Rel rather than
// strings.HasPrefix: "/a/bc" starts with "/a/b" but is not inside it.
func TestWithinRejectsASiblingSharingThePrefix(t *testing.T) {
	t.Parallel()
	if Within("/data/sybra", "/data/sybra-backup/secret") {
		t.Error("Within accepted a sibling directory sharing the root's prefix")
	}
}

func TestProjectKeyDir(t *testing.T) {
	t.Parallel()
	opaque := "work-" + strings.Repeat("a", 64)
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "owner and repo", id: "Automaat/sybra", want: "gh-4175746f6d616174-7379627261"},
		{name: "opaque work key passes through", id: opaque, want: opaque},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ProjectKeyDir(tt.id)
			if err != nil {
				t.Fatalf("ProjectKeyDir(%q) = %v", tt.id, err)
				panic("unreachable")
			}
			if got != tt.want {
				t.Errorf("ProjectKeyDir(%q) = %q, want %q", tt.id, got, tt.want)
			}
			if err := ValidateKey(got); err != nil {
				t.Errorf("ProjectKeyDir(%q) produced an unusable component: %v", tt.id, err)
			}
		})
	}

	for _, id := range []string{"", "   ", "..", "../etc", "owner", "owner/", "/repo", "a/b/c", `a\b`, "./a/b", "../a/b"} {
		if _, err := ProjectKeyDir(id); err == nil {
			t.Errorf("ProjectKeyDir(%q) = nil, want an error", id)
		}
	}
}

func TestIsOpaqueWorkProjectKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "64 hex digits", id: "work-" + strings.Repeat("a", 64), want: true},
		{name: "mixed hex", id: "work-" + strings.Repeat("0f", 32), want: true},
		{name: "too short", id: "work-" + strings.Repeat("a", 63)},
		{name: "too long", id: "work-" + strings.Repeat("a", 65)},
		{name: "uppercase hex", id: "work-" + strings.Repeat("A", 64)},
		{name: "non-hex", id: "work-" + strings.Repeat("z", 64)},
		{name: "no prefix", id: strings.Repeat("a", 64)},
		{name: "empty", id: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsOpaqueWorkProjectKey(tt.id); got != tt.want {
				t.Errorf("IsOpaqueWorkProjectKey(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
