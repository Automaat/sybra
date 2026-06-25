// Package artifact provides a per-task store for typed intermediate agent-run
// state (plan snapshots, trace events, generic blobs).
//
// The store is local-debug-only and raw: artifacts are never scrubbed at write
// time. Any future code that surfaces a work-typed artifact in a GitHub
// issue/PR/comment MUST first route through App.workScrubContextForTask +
// scrub.Scrub — the store deliberately does not scrub.
package artifact

import (
	"regexp"
	"time"
)

// Kind classifies an artifact's role in the workflow.
type Kind string

const (
	// KindPlan holds a raw markdown plan snapshot.
	KindPlan Kind = "plan"
	// KindTrace holds an append-only NDJSON stream of step-completion events.
	KindTrace Kind = "trace"
	// KindGeneric is a catch-all for helper blobs that don't fit the above.
	KindGeneric Kind = "generic"
)

func (k Kind) defaultExt() string {
	switch k {
	case KindTrace:
		return ".jsonl"
	default:
		return ".md"
	}
}

// defaultName returns the conventional artifact file name for a kind.
func (k Kind) defaultName() string {
	return string(k) + k.defaultExt()
}

// Meta is the per-artifact metadata stored alongside the raw bytes.
// It is the source of truth for List; index.json is a derived cache.
type Meta struct {
	Name         string `json:"name"`
	Kind         Kind   `json:"kind"`
	ProducerRole string `json:"producerRole,omitempty"`
	// TaskID is the containing task's ID (redundant with the dir name, kept for AC).
	TaskID    string    `json:"taskId"`
	StepID    string    `json:"stepId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// SourcePath is the agent-output file that was imported (forensic only).
	SourcePath string `json:"sourcePath,omitempty"`
	Size       int64  `json:"size"`
	// Stream is true for append-only artifacts (trace.jsonl).
	Stream bool `json:"stream,omitempty"`
}

// Artifact is the write request passed to Put.
type Artifact struct {
	Kind         Kind
	Name         string // if empty, derived from Kind
	ProducerRole string
	StepID       string
	SourcePath   string
	Content      []byte // binary-safe end-to-end
}

// validName allows alphanumeric, dot, underscore, hyphen — no path separators.
var validName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validTaskID allows alphanumeric, underscore, hyphen — matches the task ID
// character set so a hostile ID cannot escape the store root.
var validTaskID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
