// Package gitenv isolates git-dependent tests from the developer's own git
// configuration.
//
// Without this, ambient config silently changes what the tests exercise. The
// motivating case: a `url."ssh://git@github.com/".insteadOf` rewrite (a common
// setup) turns the HTTPS remote a test just configured into an ssh:// URL when
// git reads it back, so PreflightPushCredentials' isGitHubHTTPSRemote gate says
// "not a GitHub HTTPS remote" and returns nil before probing anything. The test
// then asserts against a code path that never ran — red on that machine, green
// in CI, and green for the wrong reason on any machine where the rewrite exists
// but the assertion happens to be satisfied anyway.
package gitenv

import (
	"fmt"
	"os"
	"path/filepath"
)

// Isolate points git at a throwaway global config and disables system config
// for the remainder of the process, returning a cleanup func.
//
// Call it from a package's TestMain, before m.Run(). Repo-local config that
// tests set themselves still applies — only the developer's global/system
// layers are cut out.
func Isolate() (cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "sybra-gitenv-*")
	if err != nil {
		return nil, fmt.Errorf("create git config dir: %w", err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	// Identity is set so repos that don't configure their own can still
	// commit; nothing here rewrites URLs or touches credentials.
	const contents = "[user]\n\tname = Sybra Test\n\temail = test@test.com\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(contents), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("write git config: %w", err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   cfg,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
	} {
		if err := os.Setenv(k, v); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("set %s: %w", k, err)
		}
	}
	return func() { _ = os.RemoveAll(dir) }, nil
}
