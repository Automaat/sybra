//go:build !darwin && !linux

package gitexec

import "os/exec"

// Sybra's supported production hosts are darwin and Linux. Keep the package
// buildable elsewhere while retaining exec.CommandContext's direct-child
// cancellation fallback.
func configureProcessGroupCancel(_ *exec.Cmd) {}
