package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/workercontrol"
)

var ErrSpoolExhausted = errors.New("agentd: durable spool exhausted")

const terminalEventBudgetBytes int64 = 4096

type durableState struct {
	NodeID         string                                       `json:"nodeId"`
	SessionID      string                                       `json:"sessionId,omitempty"`
	LastCommandAck uint64                                       `json:"lastCommandAck,omitempty"`
	Events         map[string][]executioncontract.EventEnvelope `json:"events,omitempty"`
	RunAgents      map[string]string                            `json:"runAgents,omitempty"`
	RunSequences   map[string]uint64                            `json:"runSequences,omitempty"`
	OutputCounts   map[string]uint64                            `json:"outputCounts,omitempty"`
	Approvals      map[string]durableApproval                   `json:"approvals,omitempty"`
	Artifacts      map[string]workercontrol.ArtifactUpload      `json:"artifacts,omitempty"`
}

type durableApproval struct {
	RunID    string `json:"runId"`
	Approved bool   `json:"approved"`
}

type Spool struct {
	mu              sync.Mutex
	path            string
	maxBytes        int64
	terminalReserve int64
	state           durableState
}

func OpenSpool(root string, maxBytes int64, capacity ...int) (*Spool, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	runCapacity := 1
	if len(capacity) > 0 && capacity[0] > 0 {
		runCapacity = capacity[0]
	}
	s := &Spool{path: filepath.Join(root, "spool.json"), maxBytes: maxBytes, terminalReserve: terminalEventBudgetBytes * int64(runCapacity)}
	data, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return nil, fmt.Errorf("agentd: decode spool: %w", err)
		}
	}
	if s.state.Events == nil {
		s.state.Events = make(map[string][]executioncontract.EventEnvelope)
	}
	if s.state.RunAgents == nil {
		s.state.RunAgents = make(map[string]string)
	}
	if s.state.RunSequences == nil {
		s.state.RunSequences = make(map[string]uint64)
	}
	if s.state.OutputCounts == nil {
		s.state.OutputCounts = make(map[string]uint64)
	}
	if s.state.Approvals == nil {
		s.state.Approvals = make(map[string]durableApproval)
	}
	if s.state.Artifacts == nil {
		s.state.Artifacts = make(map[string]workercontrol.ArtifactUpload)
	}
	return s, nil
}

func (s *Spool) snapshot() durableState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

func (s *Spool) update(fn func(*durableState) error) error {
	return s.updateLimit(s.maxBytes, fn)
}

func (s *Spool) updateLimit(limit int64, fn func(*durableState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	if err := fn(&next); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%w: %d > %d bytes", ErrSpoolExhausted, len(data), limit)
	}
	if err := fsutil.AtomicWriteMode(s.path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	s.state = next
	return nil
}

func cloneState(in durableState) durableState {
	out := in
	out.Events = make(map[string][]executioncontract.EventEnvelope, len(in.Events))
	for run, events := range in.Events {
		out.Events[run] = append([]executioncontract.EventEnvelope(nil), events...)
	}
	out.RunAgents = make(map[string]string, len(in.RunAgents))
	maps.Copy(out.RunAgents, in.RunAgents)
	out.RunSequences = make(map[string]uint64, len(in.RunSequences))
	maps.Copy(out.RunSequences, in.RunSequences)
	out.OutputCounts = make(map[string]uint64, len(in.OutputCounts))
	maps.Copy(out.OutputCounts, in.OutputCounts)
	out.Approvals = make(map[string]durableApproval, len(in.Approvals))
	maps.Copy(out.Approvals, in.Approvals)
	out.Artifacts = make(map[string]workercontrol.ArtifactUpload, len(in.Artifacts))
	for id := range in.Artifacts {
		upload := in.Artifacts[id]
		upload.Content = append([]byte(nil), upload.Content...)
		out.Artifacts[id] = upload
	}
	return out
}

func (s *Spool) capacityError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if int64(len(data)) >= s.nonTerminalLimit() {
		return ErrSpoolExhausted
	}
	return nil
}

// QueueArtifact durably schedules an artifact upload. Artifact assembly is a
// separate boundary; this method only owns bounded delivery persistence.
func (s *Spool) QueueArtifact(upload workercontrol.ArtifactUpload) error {
	return s.updateLimit(s.nonTerminalLimit(), func(state *durableState) error {
		state.Artifacts[upload.Manifest.ManifestID] = upload
		return nil
	})
}

func (s *Spool) ackArtifact(manifestID string) error {
	return s.update(func(state *durableState) error {
		delete(state.Artifacts, manifestID)
		return nil
	})
}

func (s *Spool) stageApproval(toolUseID string, decision durableApproval) error {
	return s.updateLimit(s.nonTerminalLimit(), func(state *durableState) error {
		if prior, exists := state.Approvals[toolUseID]; exists && prior != decision {
			return fmt.Errorf("agentd: conflicting approval replay for %s", toolUseID)
		}
		state.Approvals[toolUseID] = decision
		return nil
	})
}

func (s *Spool) appendEvent(event executioncontract.EventEnvelope) error {
	limit := s.maxBytes
	if event.Type != executioncontract.EventTerminal {
		limit = s.nonTerminalLimit()
	}
	return s.updateLimit(limit, func(state *durableState) error {
		event.Sequence = state.RunSequences[event.RunID] + 1
		event.IdempotencyKey = fmt.Sprintf("%s:%d", event.RunID, event.Sequence)
		state.Events[event.RunID] = append(state.Events[event.RunID], event)
		state.RunSequences[event.RunID] = event.Sequence
		if event.Type == executioncontract.EventOutput {
			state.OutputCounts[event.RunID]++
		}
		return nil
	})
}

// appendAdmissionEvent records a terminal rejection without spending capacity
// reserved for the fates of already-admitted provider runs.
func (s *Spool) appendAdmissionEvent(event executioncontract.EventEnvelope) error {
	return s.updateLimit(s.nonTerminalLimit(), func(state *durableState) error {
		event.Sequence = state.RunSequences[event.RunID] + 1
		event.IdempotencyKey = fmt.Sprintf("%s:%d", event.RunID, event.Sequence)
		state.Events[event.RunID] = append(state.Events[event.RunID], event)
		state.RunSequences[event.RunID] = event.Sequence
		return nil
	})
}

func (s *Spool) nonTerminalLimit() int64 {
	if s.maxBytes <= s.terminalReserve {
		return s.maxBytes
	}
	return s.maxBytes - s.terminalReserve
}

func (s *Spool) ackEvents(runID string, through uint64) error {
	return s.update(func(state *durableState) error {
		events := state.Events[runID]
		cut := 0
		for cut < len(events) && events[cut].Sequence <= through {
			cut++
		}
		state.Events[runID] = append([]executioncontract.EventEnvelope(nil), events[cut:]...)
		if len(state.Events[runID]) == 0 {
			delete(state.Events, runID)
		}
		return nil
	})
}
