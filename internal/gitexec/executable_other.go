//go:build !darwin && !linux

package gitexec

import "os"

func executableByCurrentUser(_ string, info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
