package logging

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/synapse/internal/config"
)

func TestNew(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "logs")

	cfg := config.LoggingConfig{
		Level:     "info",
		Dir:       dir,
		MaxSizeMB: 1,
		MaxFiles:  2,
	}

	logger, _, cleanup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if logger == nil {
		t.Fatal("logger is nil")
	}

	// Verify log dir was created
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("log dir not created: %v", err)
	}

	// Verify log file exists after writing
	logger.Info("test.message", "key", "value")

	logFile := filepath.Join(dir, "synapse.log")
	info, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("log file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("log file is empty after writing")
	}
}

func TestNewDefaultLimits(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "logs")

	cfg := config.LoggingConfig{
		Level:     "debug",
		Dir:       dir,
		MaxSizeMB: 0,
		MaxFiles:  0,
	}

	logger, _, cleanup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	logger.Debug("debug.test")

	info, err := os.Stat(filepath.Join(dir, "synapse.log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Error("debug message not written")
	}
}

func TestNewInvalidDir(t *testing.T) {
	t.Parallel()
	cfg := config.LoggingConfig{
		Level:     "info",
		Dir:       "/dev/null/impossible",
		MaxSizeMB: 1,
		MaxFiles:  2,
	}

	_, _, _, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for invalid dir")
	}
}

func TestNewRotatingWriterInvalidPath(t *testing.T) {
	t.Parallel()
	_, err := NewRotatingWriter("/nonexistent/dir/test.log", 100, 3)
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestAgentOutputFileInvalidDir(t *testing.T) {
	t.Parallel()
	_, err := NewAgentOutputFile("/dev/null/impossible", "test-id")
	if err == nil {
		t.Fatal("expected error for invalid dir")
	}
}
