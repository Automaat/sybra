package health

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestParseDockerSystemDF(t *testing.T) {
	t.Parallel()

	t.Run("newline delimited objects", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`
{"Size":"10GB","Reclaimable":"5GB (50%)"}
{"Size":"20GiB","Reclaimable":"4GiB (20%)"}
`)

		got, err := parseDockerSystemDF(raw)
		if err != nil {
			t.Fatalf("parseDockerSystemDF: %v", err)
		}
		if !got.Available {
			t.Fatal("Available = false, want true")
		}
		wantReclaimable := int64(5_000_000_000 + 4*(1<<30))
		if got.ReclaimableBytes != wantReclaimable {
			t.Fatalf("ReclaimableBytes = %d, want %d", got.ReclaimableBytes, wantReclaimable)
		}
		wantTotal := int64(10_000_000_000 + 20*(1<<30))
		if got.TotalBytes != wantTotal {
			t.Fatalf("TotalBytes = %d, want %d", got.TotalBytes, wantTotal)
		}
	})

	t.Run("array fallback", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`[{"Size":"3GB","Reclaimable":"1GB (33%)"},{"Size":"2GiB","Reclaimable":"512MiB (25%)"}]`)

		got, err := parseDockerSystemDF(raw)
		if err != nil {
			t.Fatalf("parseDockerSystemDF: %v", err)
		}
		wantReclaimable := int64(1_000_000_000 + 512*(1<<20))
		if got.ReclaimableBytes != wantReclaimable {
			t.Fatalf("ReclaimableBytes = %d, want %d", got.ReclaimableBytes, wantReclaimable)
		}
	})
}

func TestParseDockerSystemDFReclaimablePercent(t *testing.T) {
	t.Parallel()

	got, err := parseDockerSystemDF([]byte(`{"Size":"12.3GB","Reclaimable":"12.3GB (45%)"}`))
	if err != nil {
		t.Fatalf("parseDockerSystemDF: %v", err)
	}
	if got.ReclaimableBytes != 12_300_000_000 {
		t.Fatalf("ReclaimableBytes = %d, want %d", got.ReclaimableBytes, int64(12_300_000_000))
	}
}

func TestParseDockerSizeDecimalAndBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want int64
	}{
		{raw: "12.3GB", want: 12_300_000_000},
		{raw: "1.5GiB", want: int64(1.5 * float64(1<<30))},
		{raw: "20GiB", want: 20 * (1 << 30)},
		{raw: "512MiB (25%)", want: 512 * (1 << 20)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := parseDockerSize(tt.raw)
			if err != nil {
				t.Fatalf("parseDockerSize: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseDockerSize(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCheckDockerReclaimable(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	t.Run("unavailable sample is ignored", func(t *testing.T) {
		t.Parallel()
		if got := checkDockerReclaimable(DockerDiskUsage{}, now); len(got) != 0 {
			t.Fatalf("got %d findings, want 0", len(got))
		}
	})

	t.Run("below threshold is ignored", func(t *testing.T) {
		t.Parallel()
		got := checkDockerReclaimable(DockerDiskUsage{
			Available:        true,
			ReclaimableBytes: dockerReclaimableThresholdBytes - 1,
		}, now)
		if len(got) != 0 {
			t.Fatalf("got %d findings, want 0", len(got))
		}
	})

	t.Run("threshold triggers warning", func(t *testing.T) {
		t.Parallel()
		got := checkDockerReclaimable(DockerDiskUsage{
			Available:        true,
			ReclaimableBytes: dockerReclaimableThresholdBytes,
		}, now)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		f := got[0]
		if f.Category != CatDockerReclaimable {
			t.Fatalf("Category = %q, want %q", f.Category, CatDockerReclaimable)
		}
		if f.Severity != SeverityWarning {
			t.Fatalf("Severity = %q, want %q", f.Severity, SeverityWarning)
		}
		if f.Evidence["reclaimable_bytes"] != dockerReclaimableThresholdBytes {
			t.Fatalf("reclaimable_bytes = %v, want %d", f.Evidence["reclaimable_bytes"], dockerReclaimableThresholdBytes)
		}
		if f.Evidence["threshold_bytes"] != dockerReclaimableThresholdBytes {
			t.Fatalf("threshold_bytes = %v, want %d", f.Evidence["threshold_bytes"], dockerReclaimableThresholdBytes)
		}
		if f.Evidence["manual_command"] != dockerManualCommand {
			t.Fatalf("manual_command = %v, want %q", f.Evidence["manual_command"], dockerManualCommand)
		}
		if !strings.Contains(f.Description, dockerManualCommand) {
			t.Fatalf("Description = %q, want mention of %q", f.Description, dockerManualCommand)
		}
	})
}

func TestSampleDockerDiskUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	tests := []struct {
		name   string
		runner dockerRunner
	}{
		{
			name: "missing binary",
			runner: func(context.Context) ([]byte, error) {
				return nil, exec.ErrNotFound
			},
		},
		{
			name: "permission denied",
			runner: func(context.Context) ([]byte, error) {
				return nil, errors.New("permission denied while trying to connect to the Docker daemon socket")
			},
		},
		{
			name: "timeout",
			runner: func(context.Context) ([]byte, error) {
				return nil, context.DeadlineExceeded
			},
		},
		{
			name: "malformed output",
			runner: func(context.Context) ([]byte, error) {
				return []byte(`{"Reclaimable":"bad"}`), nil
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sampleDockerDisk(context.Background(), tt.runner, now)
			if got.Available {
				t.Fatal("Available = true, want false")
			}
			if got.ReclaimableBytes != 0 {
				t.Fatalf("ReclaimableBytes = %d, want 0", got.ReclaimableBytes)
			}
			if got.ManualCommand != dockerManualCommand {
				t.Fatalf("ManualCommand = %q, want %q", got.ManualCommand, dockerManualCommand)
			}
			if !got.SampledAt.Equal(now) {
				t.Fatalf("SampledAt = %s, want %s", got.SampledAt, now)
			}
		})
	}
}
