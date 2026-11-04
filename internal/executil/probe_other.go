//go:build !unix

package executil

import (
	"os"
	"os/exec"
)

// configureProbeGroup keeps exec.CommandContext's direct-child cancellation on
// hosts without Unix process groups; Sybra's supported hosts are darwin and Linux.
func configureProbeGroup(_ *exec.Cmd) {}

func killProbeGroup(_ *exec.Cmd) error { return os.ErrProcessDone }
