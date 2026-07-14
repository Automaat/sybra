package health

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	dockerReclaimableThresholdBytes int64         = 20 * 1024 * 1024 * 1024
	dockerManualCommand                           = "docker system prune"
	dockerProbeTimeout              time.Duration = 5 * time.Second
)

type dockerRunner func(context.Context) ([]byte, error)

type dockerDFRow struct {
	Size        string
	Reclaimable string
}

var dockerSizeRe = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*([kmgtpe]?i?b)$`)

func defaultDockerRunner(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "system", "df", "--format", "json").Output()
}

func sampleDockerDisk(ctx context.Context, runner dockerRunner, now time.Time) DockerDiskUsage {
	sample := DockerDiskUsage{
		ManualCommand: dockerManualCommand,
		SampledAt:     now,
	}
	if runner == nil {
		runner = defaultDockerRunner
	}
	raw, err := runner(ctx)
	if err != nil {
		return sample
	}
	parsed, err := parseDockerSystemDF(raw)
	if err != nil {
		return sample
	}
	parsed.ManualCommand = dockerManualCommand
	parsed.SampledAt = now
	return parsed
}

func parseDockerSystemDF(raw []byte) (DockerDiskUsage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return DockerDiskUsage{}, fmt.Errorf("empty docker system df output")
	}

	var rows []dockerDFRow
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return DockerDiskUsage{}, fmt.Errorf("decode docker system df array: %w", err)
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(trimmed))
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var row dockerDFRow
			if err := json.Unmarshal(line, &row); err != nil {
				return DockerDiskUsage{}, fmt.Errorf("decode docker system df row: %w", err)
			}
			rows = append(rows, row)
		}
		if err := sc.Err(); err != nil {
			return DockerDiskUsage{}, fmt.Errorf("scan docker system df: %w", err)
		}
	}
	if len(rows) == 0 {
		return DockerDiskUsage{}, fmt.Errorf("no docker system df rows")
	}

	sample := DockerDiskUsage{Available: true}
	for _, row := range rows {
		reclaimable, err := parseDockerSize(row.Reclaimable)
		if err != nil {
			return DockerDiskUsage{}, fmt.Errorf("parse reclaimable size %q: %w", row.Reclaimable, err)
		}
		sample.ReclaimableBytes += reclaimable

		if row.Size == "" {
			continue
		}
		size, err := parseDockerSize(row.Size)
		if err != nil {
			return DockerDiskUsage{}, fmt.Errorf("parse size %q: %w", row.Size, err)
		}
		sample.TotalBytes += size
	}
	return sample, nil
}

func parseDockerSize(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("empty size")
	}
	if i := strings.IndexByte(trimmed, '('); i >= 0 {
		trimmed = strings.TrimSpace(trimmed[:i])
	}
	match := dockerSizeRe.FindStringSubmatch(trimmed)
	if match == nil {
		return 0, fmt.Errorf("invalid size %q", raw)
	}

	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("parse size number: %w", err)
	}

	unit := strings.ToUpper(match[2])
	multiplier, ok := dockerSizeMultiplier(unit)
	if !ok {
		return 0, fmt.Errorf("unknown unit %q", match[2])
	}

	return int64(math.Round(value * multiplier)), nil
}

func dockerSizeMultiplier(unit string) (float64, bool) {
	switch unit {
	case "B":
		return 1, true
	case "KB":
		return 1_000, true
	case "MB":
		return 1_000_000, true
	case "GB":
		return 1_000_000_000, true
	case "TB":
		return 1_000_000_000_000, true
	case "PB":
		return 1_000_000_000_000_000, true
	case "EB":
		return 1_000_000_000_000_000_000, true
	case "KIB":
		return 1 << 10, true
	case "MIB":
		return 1 << 20, true
	case "GIB":
		return 1 << 30, true
	case "TIB":
		return 1 << 40, true
	case "PIB":
		return 1 << 50, true
	case "EIB":
		return 1 << 60, true
	default:
		return 0, false
	}
}

func checkDockerReclaimable(sample DockerDiskUsage, now time.Time) []Finding {
	if !sample.Available || sample.ReclaimableBytes < dockerReclaimableThresholdBytes {
		return nil
	}

	return []Finding{{
		Category:    CatDockerReclaimable,
		Severity:    SeverityWarning,
		Title:       "docker reclaimable disk usage is high",
		Description: "Docker reports high reclaimable disk usage. Review and run docker system prune manually if appropriate.",
		Evidence: map[string]any{
			"reclaimable_bytes": sample.ReclaimableBytes,
			"threshold_bytes":   dockerReclaimableThresholdBytes,
			"manual_command":    dockerManualCommand,
		},
		DetectedAt: now,
	}}
}
