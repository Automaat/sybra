package executioncontract

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type CommandType string

const (
	CommandStart            CommandType = "start"
	CommandStop             CommandType = "stop"
	CommandSteer            CommandType = "steer"
	CommandApprovalResponse CommandType = "approval_response"
)

type CommandEnvelope struct {
	Version        Version     `json:"version"`
	BuildVersion   string      `json:"buildVersion"`
	CommandID      string      `json:"commandId"`
	RunID          string      `json:"runId"`
	IdempotencyKey string      `json:"idempotencyKey"`
	Type           CommandType `json:"type"`
	SentAt         time.Time   `json:"sentAt"`
	// Payload can contain a RunSpec prompt or steering text. Treat it as
	// sensitive task content and never log it verbatim.
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (e CommandEnvelope) Validate() error {
	if err := e.Version.Validate(); err != nil {
		return err
	}
	if e.BuildVersion == "" || e.CommandID == "" || e.RunID == "" || e.IdempotencyKey == "" || e.SentAt.IsZero() {
		return errors.New("execution contract: command identity, run, idempotency key, and sent time are required")
	}
	switch e.Type {
	case CommandStart, CommandStop, CommandSteer, CommandApprovalResponse:
		return nil
	default:
		return fmt.Errorf("execution contract: unsupported command type %q", e.Type)
	}
}

type EventType string

const (
	EventStarted  EventType = "started"
	EventOutput   EventType = "output"
	EventProgress EventType = "progress"
	EventTerminal EventType = "terminal"
)

type EventEnvelope struct {
	Version        Version   `json:"version"`
	BuildVersion   string    `json:"buildVersion"`
	RunID          string    `json:"runId"`
	Sequence       uint64    `json:"sequence"`
	EventID        string    `json:"eventId"`
	IdempotencyKey string    `json:"idempotencyKey"`
	Type           EventType `json:"type"`
	ObservedAt     time.Time `json:"observedAt"`
	// Payload commonly contains provider output and is sensitive task content.
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (e EventEnvelope) Validate() error {
	if err := e.Version.Validate(); err != nil {
		return err
	}
	if e.BuildVersion == "" || e.RunID == "" || e.Sequence == 0 || e.EventID == "" || e.IdempotencyKey == "" || e.ObservedAt.IsZero() {
		return errors.New("execution contract: event run, positive sequence, identities, and observation time are required")
	}
	switch e.Type {
	case EventStarted, EventOutput, EventProgress, EventTerminal:
		return nil
	default:
		return fmt.Errorf("execution contract: unsupported event type %q", e.Type)
	}
}

// ValidateEventOrder verifies the complete ordered slice supplied by a
// transport replay. It accepts duplicates only when both sequence and
// idempotency key match, allowing at-least-once delivery without ambiguity.
func ValidateEventOrder(events []EventEnvelope) error {
	var last uint64
	type identity struct {
		id, key, payload string
		typeID           EventType
	}
	identities := map[uint64]identity{}
	terminal := false
	for i := range events {
		event := &events[i]
		if err := event.Validate(); err != nil {
			return err
		}
		if got, duplicate := identities[event.Sequence]; duplicate {
			if got != (identity{id: event.EventID, key: event.IdempotencyKey, payload: string(event.Payload), typeID: event.Type}) {
				return fmt.Errorf("execution contract: sequence %d reused with different event identity", event.Sequence)
			}
			continue
		}
		if terminal {
			return errors.New("execution contract: event follows terminal event")
		}
		if event.Sequence != last+1 {
			return fmt.Errorf("execution contract: event sequence gap: got %d after %d", event.Sequence, last)
		}
		identities[event.Sequence] = identity{id: event.EventID, key: event.IdempotencyKey, payload: string(event.Payload), typeID: event.Type}
		last = event.Sequence
		terminal = event.Type == EventTerminal
	}
	return nil
}

type TerminalState string

const (
	TerminalSucceeded TerminalState = "succeeded"
	TerminalFailed    TerminalState = "failed"
	TerminalCanceled  TerminalState = "canceled"
)

type ArtifactState string

const (
	ArtifactsPending ArtifactState = "pending"
	ArtifactsReady   ArtifactState = "ready"
	ArtifactsFailed  ArtifactState = "failed"
)

type TerminalResult struct {
	Version        Version       `json:"version"`
	BuildVersion   string        `json:"buildVersion"`
	RunID          string        `json:"runId"`
	IdempotencyKey string        `json:"idempotencyKey"`
	State          TerminalState `json:"state"`
	LastSequence   uint64        `json:"lastSequence"`
	ExitCode       *int          `json:"exitCode,omitempty"`
	// Error may contain provider/task content and must be scrubbed before it is
	// copied into a public artifact.
	Error              string        `json:"error,omitempty"`
	ArtifactState      ArtifactState `json:"artifactState"`
	ArtifactManifestID string        `json:"artifactManifestId,omitempty"`
	CompletedAt        time.Time     `json:"completedAt"`
}

func (r TerminalResult) Validate() error {
	if err := r.Version.Validate(); err != nil {
		return err
	}
	if r.BuildVersion == "" || r.RunID == "" || r.IdempotencyKey == "" || r.LastSequence == 0 || r.CompletedAt.IsZero() {
		return errors.New("execution contract: incomplete terminal result")
	}
	if r.State != TerminalSucceeded && r.State != TerminalFailed && r.State != TerminalCanceled {
		return fmt.Errorf("execution contract: invalid terminal state %q", r.State)
	}
	if r.ArtifactState != ArtifactsPending && r.ArtifactState != ArtifactsReady && r.ArtifactState != ArtifactsFailed {
		return fmt.Errorf("execution contract: invalid artifact state %q", r.ArtifactState)
	}
	if r.ArtifactState == ArtifactsReady && r.ArtifactManifestID == "" {
		return errors.New("execution contract: ready artifacts require a manifest id")
	}
	return nil
}

type ArtifactEntry struct {
	// Name and Path may reveal task/repository structure and inherit the entry's
	// sensitivity for logging and persistence policy.
	Name         string      `json:"name"`
	Kind         string      `json:"kind"`
	Root         LogicalRoot `json:"root"`
	Path         string      `json:"path"`
	DigestSHA256 string      `json:"digestSha256"`
	SizeBytes    int64       `json:"sizeBytes"`
	MediaType    string      `json:"mediaType"`
	Sensitivity  Sensitivity `json:"sensitivity"`
}

type ArtifactManifest struct {
	Version        Version         `json:"version"`
	BuildVersion   string          `json:"buildVersion"`
	RunID          string          `json:"runId"`
	ManifestID     string          `json:"manifestId"`
	IdempotencyKey string          `json:"idempotencyKey"`
	State          ArtifactState   `json:"state"`
	GeneratedAt    time.Time       `json:"generatedAt"`
	Artifacts      []ArtifactEntry `json:"artifacts,omitempty"`
}

func (m ArtifactManifest) Validate() error {
	if err := m.Version.Validate(); err != nil {
		return err
	}
	if m.BuildVersion == "" || m.RunID == "" || m.ManifestID == "" || m.IdempotencyKey == "" || m.GeneratedAt.IsZero() {
		return errors.New("execution contract: incomplete artifact manifest")
	}
	if m.State != ArtifactsPending && m.State != ArtifactsReady && m.State != ArtifactsFailed {
		return fmt.Errorf("execution contract: invalid artifact manifest state %q", m.State)
	}
	for _, artifact := range m.Artifacts {
		if artifact.Name == "" || artifact.Kind == "" || artifact.DigestSHA256 == "" || artifact.SizeBytes < 0 ||
			!validRoot(artifact.Root) || !logicalPath(artifact.Path) || !validSensitivity(artifact.Sensitivity) {
			return fmt.Errorf("execution contract: invalid artifact %q", artifact.Name)
		}
	}
	return nil
}

func decodeValidated[T any](data []byte, validate func(T) error) (T, error) {
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	return value, validate(value)
}

func DecodeCommand(data []byte) (CommandEnvelope, error) {
	return decodeValidated(data, CommandEnvelope.Validate)
}

func DecodeEvent(data []byte) (EventEnvelope, error) {
	return decodeValidated(data, EventEnvelope.Validate)
}

func DecodeTerminalResult(data []byte) (TerminalResult, error) {
	return decodeValidated(data, TerminalResult.Validate)
}

func DecodeArtifactManifest(data []byte) (ArtifactManifest, error) {
	return decodeValidated(data, ArtifactManifest.Validate)
}
