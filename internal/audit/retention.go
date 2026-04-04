package audit

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Cleanup(dir string, retentionDays int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.DateOnly)

	for _, e := range entries {
		day, ok := strings.CutSuffix(e.Name(), ".ndjson")
		if e.IsDir() || !ok {
			continue
		}
		if day < cutoff {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}
