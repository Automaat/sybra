//go:build darwin

package notification

import (
	"fmt"
	"os/exec"
)

func sendDesktopNotification(title, message string) error {
	script := fmt.Sprintf(`display notification %q with title %q`, message, title)
	return exec.Command("osascript", "-e", script).Run()
}
