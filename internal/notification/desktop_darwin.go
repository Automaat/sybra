//go:build darwin

package notification

import (
	"context"
	"fmt"
	"os/exec"
)

func sendDesktopNotification(title, message string) error {
	script := fmt.Sprintf(`display notification %q with title %q`, message, title)
	return exec.CommandContext(context.Background(), "osascript", "-e", script).Run()
}
