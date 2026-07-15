package sybra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	runtimeProbeTimeout  = 1500 * time.Millisecond
	runtimeProbeErrorMax = 160
	runtimeVersionArg    = "--version"
)

type runtimeSpec struct {
	id                string
	name              string
	binary            string
	informationalOnly bool
	versionArgs       []string
}

var knownRuntimeSpecs = []runtimeSpec{
	{id: "claude", name: "Claude Code", binary: "claude", versionArgs: []string{runtimeVersionArg}},
	{id: "codex", name: "Codex", binary: "codex", versionArgs: []string{runtimeVersionArg}},
	{id: "opencode", name: "OpenCode", binary: "opencode", versionArgs: []string{runtimeVersionArg}},
	{id: "hermes", name: "Hermes", binary: "hermes", informationalOnly: true, versionArgs: []string{runtimeVersionArg}},
}

// RuntimeInfo is the read-only detected state for one known CLI runtime.
type RuntimeInfo struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Installed         bool   `json:"installed"`
	Path              string `json:"path,omitempty"`
	Version           string `json:"version,omitempty"`
	Error             string `json:"error,omitempty"`
	InformationalOnly bool   `json:"informationalOnly,omitempty"`
}

func (s *InfoService) primeRuntimeSnapshot() {
	s.runtimeOnce.Do(func() {
		detect := s.detectRuntimes
		if detect == nil {
			detect = detectAvailableRuntimes
		}
		s.availableRuntimes = detect()
	})
}

func detectAvailableRuntimes() []RuntimeInfo {
	out := make([]RuntimeInfo, 0, len(knownRuntimeSpecs))
	for _, spec := range knownRuntimeSpecs {
		info := RuntimeInfo{
			ID:                spec.id,
			Name:              spec.name,
			InformationalOnly: spec.informationalOnly,
		}
		path, err := exec.LookPath(spec.binary)
		if err != nil {
			out = append(out, info)
			continue
		}
		info.Installed = true
		info.Path = path
		info.Version, info.Error = probeRuntimeVersion(path, spec.versionArgs, runtimeProbeTimeout)
		out = append(out, info)
	}
	return out
}

func probeRuntimeVersion(path string, args []string, timeout time.Duration) (version, probeErr string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return "", formatRuntimeProbeError(ctx, err, out, timeout)
	}
	return strings.TrimSpace(string(out)), ""
}

func formatRuntimeProbeError(ctx context.Context, err error, output []byte, timeout time.Duration) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("version probe timed out after %s", timeout)
	}
	msg := strings.TrimSpace(string(bytes.TrimSpace(output)))
	if msg == "" {
		msg = err.Error()
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > runtimeProbeErrorMax {
		msg = msg[:runtimeProbeErrorMax-1] + "…"
	}
	return msg
}
