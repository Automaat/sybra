package llmexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
)

// CommandFactory builds the process a one-shot provider call runs in.
//
// The OS-level sandbox lives in internal/agent, which imports this package,
// so the dependency is inverted rather than the wrap duplicated: whoever owns
// an agent Manager registers its factory here at startup and every call made
// afterwards is contained the same way an agent spawn is.
type CommandFactory func(ctx context.Context, dir, name string, args []string) (*exec.Cmd, func(), error)

var factory atomic.Pointer[CommandFactory]

// SetCommandFactory installs the process constructor for every later call.
// Registering twice replaces the previous one; passing nil restores the
// unsandboxed default.
func SetCommandFactory(f CommandFactory) {
	if f == nil {
		factory.Store(nil)
		return
	}
	factory.Store(&f)
}

// providerCommand returns the command to run and a release function the caller
// must call when the process has exited.
//
// The working directory is the caller's when it named one, and otherwise a
// fresh empty directory that is removed afterwards. It is never inherited: on
// the deploy host the serving process runs from a source checkout, so an
// inherited cwd put a tool-enabled CLI inside Sybra's own repository (#3383).
func providerCommand(ctx context.Context, opts Options, name string, args []string) (cmd *exec.Cmd, release func(), err error) {
	dir := opts.Dir
	release = func() {}
	if dir == "" {
		scratch, mkErr := os.MkdirTemp("", "sybra-llmexec-")
		if mkErr != nil {
			return nil, nil, fmt.Errorf("%w: %w", errWorkdir, mkErr)
		}
		dir = scratch
		release = func() { _ = os.RemoveAll(scratch) }
	}

	if f := factory.Load(); f != nil {
		built, done, buildErr := (*f)(ctx, dir, name, args)
		if buildErr != nil {
			release()
			return nil, nil, fmt.Errorf("%w: build provider command: %w", errWorkdir, buildErr)
		}
		// Composed rather than tied to ctx: a caller passes the app's root
		// context, which is done only at shutdown, so one scratch home per
		// call would accumulate for the life of the server.
		dropWorkdir := release
		release = func() {
			if done != nil {
				done()
			}
			dropWorkdir()
		}
		return built, release, nil
	}

	// Reached only by a caller running outside the app, a CLI or a test, with
	// no Manager to borrow containment from. Such a call still gets its own
	// working directory, and tools stay off unless it asked for them.
	// #nosec G702 -- name is a provider id resolved by invocation, not caller input.
	cmd = exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd, release, nil
}
