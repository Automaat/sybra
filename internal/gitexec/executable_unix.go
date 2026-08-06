//go:build darwin || linux

package gitexec

import (
	"os"

	"golang.org/x/sys/unix"
)

func executableByCurrentUser(path string, info os.FileInfo) bool {
	return info.Mode().IsRegular() && unix.Access(path, unix.X_OK) == nil
}
