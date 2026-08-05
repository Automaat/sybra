package sybra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/textutil"
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
	attachmentProbe   *runtimeTextProbe
}

type runtimeTextProbe struct {
	args       []string
	substrings []string
}

var knownRuntimeSpecs = []runtimeSpec{
	{
		id:          "claude",
		name:        "Claude Code",
		binary:      "claude",
		versionArgs: []string{runtimeVersionArg},
		attachmentProbe: &runtimeTextProbe{
			args:       []string{"--help"},
			substrings: []string{"--file <specs...>"},
		},
	},
	{
		id:          "codex",
		name:        "Codex",
		binary:      "codex",
		versionArgs: []string{runtimeVersionArg},
		attachmentProbe: &runtimeTextProbe{
			args:       []string{"exec", "--help"},
			substrings: []string{"--image <file>"},
		},
	},
	{id: "opencode", name: "OpenCode", binary: "opencode", versionArgs: []string{runtimeVersionArg}},
	{id: "hermes", name: "Hermes", binary: "hermes", informationalOnly: true, versionArgs: []string{runtimeVersionArg}},
}

const (
	runtimeAttachmentUnsupported = "unsupported"
	runtimeAttachmentSupported   = "supported"
)

// RuntimeInfo is the read-only detected state for one known CLI runtime.
type RuntimeInfo struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Installed         bool   `json:"installed"`
	Path              string `json:"path,omitempty"`
	Version           string `json:"version,omitempty"`
	Error             string `json:"error,omitempty"`
	InformationalOnly bool   `json:"informationalOnly,omitempty"`
	AttachmentSupport string `json:"attachmentSupport,omitempty"`
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
		info.AttachmentSupport = probeRuntimeTextCapability(path, spec.attachmentProbe, runtimeProbeTimeout)
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
		msg = textutil.TruncateBytesTotal(strings.ToValidUTF8(msg, "\uFFFD"), runtimeProbeErrorMax, "...")
	}
	return msg
}

func probeRuntimeTextCapability(path string, probe *runtimeTextProbe, timeout time.Duration) string {
	if probe == nil || len(probe.args) == 0 || len(probe.substrings) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, probe.args...).CombinedOutput()
	if err != nil {
		return ""
	}
	lower := strings.ToLower(string(out))
	for _, needle := range probe.substrings {
		if strings.Contains(lower, strings.ToLower(needle)) {
			return runtimeAttachmentSupported
		}
	}
	return runtimeAttachmentUnsupported
}
