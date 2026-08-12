package workercontrol

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /worker/v1/register", s.handleRegister)
	mux.HandleFunc("POST /worker/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /worker/v1/commands", s.handlePollCommands)
	mux.HandleFunc("POST /worker/v1/commands/ack", s.handleAckCommands)
	mux.HandleFunc("POST /worker/v1/events", s.handleEvents)
	mux.HandleFunc("GET /worker/v1/events/{runID}", s.handleReplayEvents)
	mux.HandleFunc("POST /worker/v1/events/{runID}/ack", s.handleAckEvents)
	mux.HandleFunc("POST /worker/v1/artifacts", s.handleArtifact)
	mux.HandleFunc("POST /worker/v1/drain", s.handleDrain)
	mux.HandleFunc("GET /worker/v1/diagnostics", s.handleDiagnostics)
	return mux
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var request RegisterRequest
	if !decode(w, r, &request) {
		return
	}
	session, err := s.Register(r.Context(), request)
	respond(w, session, err)
}

func (s *Service) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SessionID    string   `json:"sessionId"`
		Capabilities []string `json:"capabilities,omitempty"`
	}
	if !decode(w, r, &request) {
		return
	}
	session, err := s.Heartbeat(r.Context(), request.SessionID, request.Capabilities)
	respond(w, session, err)
}

func (s *Service) handlePollCommands(w http.ResponseWriter, r *http.Request) {
	after, err := uintQuery(r, "after")
	if err != nil {
		respond(w, nil, err)
		return
	}
	waitSeconds, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	commands, err := s.PollCommands(r.Context(), r.URL.Query().Get("session"), after, 100, time.Duration(waitSeconds)*time.Second)
	respond(w, commands, err)
}

func (s *Service) handleAckCommands(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SessionID string `json:"sessionId"`
		Through   uint64 `json:"through"`
	}
	if !decode(w, r, &request) {
		return
	}
	respond(w, map[string]bool{"acknowledged": true}, s.AckCommands(r.Context(), request.SessionID, request.Through))
}

func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	var batch EventBatch
	if !decode(w, r, &batch) {
		return
	}
	acks, err := s.AppendEvents(r.Context(), batch)
	respond(w, map[string]any{"acknowledgedThrough": acks}, err)
}

func (s *Service) handleReplayEvents(w http.ResponseWriter, r *http.Request) {
	after, err := uintQuery(r, "after")
	if err != nil {
		respond(w, nil, err)
		return
	}
	events, err := s.ReplayEvents(r.Context(), r.PathValue("runID"), after, 1000)
	respond(w, events, err)
}

func (s *Service) handleAckEvents(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SessionID string `json:"sessionId"`
		Through   uint64 `json:"through"`
	}
	if !decode(w, r, &request) {
		return
	}
	respond(w, map[string]bool{"acknowledged": true}, s.AckEvents(r.Context(), request.SessionID, r.PathValue("runID"), request.Through))
}

func (s *Service) handleArtifact(w http.ResponseWriter, r *http.Request) {
	var upload ArtifactUpload
	if !decode(w, r, &upload) {
		return
	}
	respond(w, map[string]bool{"imported": true}, s.UploadArtifact(r.Context(), upload))
}

func (s *Service) handleDrain(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if !decode(w, r, &request) {
		return
	}
	respond(w, map[string]bool{"draining": true}, s.Drain(r.Context(), request.SessionID))
}

func (s *Service) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	diagnostics, err := s.Diagnostics(r.Context())
	respond(w, diagnostics, err)
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := decoder.Decode(target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func respond(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		status, message := http.StatusInternalServerError, "internal server error"
		switch {
		case errors.Is(err, ErrStaleSession), errors.Is(err, ErrLeaseExpired):
			status = http.StatusConflict
			message = err.Error()
		case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrEventGap):
			status = http.StatusBadRequest
			message = err.Error()
		}
		writeJSONError(w, status, message)
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func uintQuery(r *http.Request, name string) (uint64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, invalidf("invalid %s cursor", name)
	}
	return parsed, nil
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
