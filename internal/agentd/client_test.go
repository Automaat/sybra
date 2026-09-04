package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/workercontrol"
)

func TestLeaderClientStreamsAndVerifiesWorkspaceBaseBundle(t *testing.T) {
	content := []byte("streamed git bundle")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.URL.Query().Get("session") != "session" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-bundle")
		w.Header().Set("X-Sybra-Content-SHA256", digest)
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)
	client := newLeaderClient(server.URL, "token")
	ref := executioncontract.ContentReference{ID: "run", DigestSHA256: digest, SizeBytes: int64(len(content))}
	bundle, err := client.workspaceBaseBundle(t.Context(), "session", "run", ref)
	if err != nil || !bytes.Equal(bundle.Content, content) || bundle.DigestSHA256 != digest {
		t.Fatalf("bundle = %+v, %v", bundle, err)
	}
}

func TestLeaderClientDecodesPerRunEventAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/worker/v1/events" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"acknowledgedThrough": map[string]uint64{"run": 7},
		})
	}))
	t.Cleanup(server.Close)
	client := newLeaderClient(server.URL, "token")
	ack, err := client.events(context.Background(), workercontrol.EventBatch{
		SessionID: "session",
		Events: []executioncontract.EventEnvelope{{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "run", Sequence: 7,
			EventID: "event", IdempotencyKey: "run:7", Type: executioncontract.EventOutput, ObservedAt: time.Now().UTC(),
		}},
	})
	if err != nil || ack != 7 {
		t.Fatalf("events ack = %d, %v", ack, err)
	}
}

func TestLeaderClientClassifiesRejectedSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": workercontrol.ErrLeaseExpired.Error()})
	}))
	t.Cleanup(server.Close)
	client := newLeaderClient(server.URL, "token")
	_, err := client.poll(t.Context(), "expired", 0, 1)
	if !isRejectedSession(err) {
		t.Fatalf("poll error = %v, want rejected session", err)
	}
}
