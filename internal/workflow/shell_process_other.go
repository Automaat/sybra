//go:build !unix

package workflow

import "os/exec"

func configureShellCancellation(_ *exec.Cmd) {}
