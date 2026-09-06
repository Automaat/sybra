package version

import (
	"encoding/hex"
	"runtime/debug"
)

// ValidRevision accepts only immutable git object names, never refs or paths.
func ValidRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// CleanRevision is the running build's source identity, not the deploy
// checkout's HEAD (which may already have advanced while this process runs).
// Unknown and locally modified builds must not authorize worker upgrades.
func CleanRevision() string {
	info, _ := debug.ReadBuildInfo()
	return cleanRevision(Version, info)
}

func cleanRevision(injected string, info *debug.BuildInfo) string {
	revision := ""
	if info != nil {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				return ""
			}
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
		}
	}
	if ValidRevision(injected) {
		if revision != "" && revision != injected {
			return ""
		}
		return injected
	}
	if ValidRevision(revision) {
		return revision
	}
	return ""
}
