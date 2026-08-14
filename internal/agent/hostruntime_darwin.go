//go:build darwin

package agent

// hostRuntimeReadRoots grants Homebrew's runtime closure on arm64 macs.
//
// Scoped to the three directories below /opt/homebrew rather than /opt, which
// stays excluded: the deploy checkout lives at /opt/sybra/src and keeping it
// unreadable is the whole reason /opt is not a system read root. Intel
// Homebrew installs under /usr/local and is already covered by /usr.
//
// Cellar holds the real binaries, opt holds the version-independent symlinks
// a dylib install name points at, and lib holds the shared libraries those
// resolve to. A loader needs all three to open one library.
func hostRuntimeReadRoots() []string {
	return []string{
		"/opt/homebrew/Cellar",
		"/opt/homebrew/opt",
		"/opt/homebrew/lib",
	}
}
