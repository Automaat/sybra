// Package agent stands in for the real internal/agent package just enough
// that sybra-build.sh's `go test ./internal/agent -run
// '^TestSandboxEnforce_LinkedWorktreeGitOps$'` smoke-test step (which fires
// whenever bwrap is on PATH, real or not) has something real to run against
// in deploy/tests.
package agent
