package workercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestDurableWorkerControlBehavior(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		t.Helper()
		ctx := t.Context()
		database := engine.Open(t)
		service := New(database)
		session := register(t, service, "worker-a")
		spec, command := startContract(t, "run-a", "effect-a")

		first, err := service.Enqueue(ctx, session.SessionID, &spec, command)
		if err != nil {
			t.Fatalf("Enqueue start: %v", err)
		}
		duplicate, err := service.Enqueue(ctx, session.SessionID, &spec, command)
		if err != nil || duplicate.Sequence != first.Sequence {
			t.Fatalf("idempotent Enqueue = %+v, %v; want sequence %d", duplicate, err, first.Sequence)
		}
		commands, err := service.PollCommands(ctx, session.SessionID, 0, 10, 0)
		if err != nil || len(commands) != 1 || commands[0].Envelope.CommandID != command.CommandID {
			t.Fatalf("PollCommands = %+v, %v", commands, err)
		}
		if err := service.AckCommands(ctx, session.SessionID, first.Sequence); err != nil {
			t.Fatalf("AckCommands: %v", err)
		}
		steer := executioncontract.CommandEnvelope{
			Version: executioncontract.CurrentVersion(), BuildVersion: "leader-test", CommandID: "command:steer-a",
			RunID: "run-a", IdempotencyKey: "steer:run-a:1", Type: executioncontract.CommandSteer,
			SentAt: time.Now().UTC(), Payload: json.RawMessage(`{"message":"continue"}`),
		}
		steerCommand, err := service.Enqueue(ctx, session.SessionID, nil, steer)
		if err != nil {
			t.Fatalf("Enqueue steer: %v", err)
		}

		events := []executioncontract.EventEnvelope{
			event("run-a", 1, executioncontract.EventStarted),
			event("run-a", 2, executioncontract.EventOutput),
		}
		acks, err := service.AppendEvents(ctx, EventBatch{SessionID: session.SessionID, Events: events})
		if err != nil || acks["run-a"] != 2 {
			t.Fatalf("AppendEvents = %v, %v", acks, err)
		}
		if _, err := service.AppendEvents(ctx, EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{event("run-a", 4, executioncontract.EventOutput)}}); !errors.Is(err, ErrEventGap) {
			t.Fatalf("gap error = %v, want ErrEventGap", err)
		}

		// A new service over a fresh handle models a leader restart. Durable
		// commands and events remain available at their cursors.
		restarted := New(engine.Open(t))
		replayed, err := restarted.ReplayEvents(ctx, "run-a", 0, 10)
		if err != nil || len(replayed) != 2 || replayed[1].Sequence != 2 {
			t.Fatalf("replay after restart = %+v, %v", replayed, err)
		}
		if err := restarted.AckEvents(ctx, session.SessionID, "run-a", 2); err != nil {
			t.Fatalf("AckEvents: %v", err)
		}

		replacement, err := restarted.Register(ctx, RegisterRequest{
			WorkerID: "worker-a", ResumeSessionID: session.SessionID, LastCommandAck: first.Sequence,
			Negotiation: executioncontract.Negotiation{
				ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
			},
		})
		if err != nil {
			t.Fatalf("resume registration: %v", err)
		}
		if _, err := restarted.PollCommands(ctx, session.SessionID, 0, 10, 0); !errors.Is(err, ErrStaleSession) {
			t.Fatalf("stale PollCommands error = %v", err)
		}
		if _, err := restarted.AppendEvents(ctx, EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{event("run-a", 3, executioncontract.EventOutput)}}); !errors.Is(err, ErrStaleSession) {
			t.Fatalf("stale AppendEvents error = %v", err)
		}
		if _, err := restarted.Enqueue(ctx, replacement.SessionID, &spec, command); err != nil {
			t.Fatalf("redelivered start after replacement: %v", err)
		}
		newCommands, err := restarted.PollCommands(ctx, replacement.SessionID, first.Sequence, 10, 0)
		if err != nil || len(newCommands) != 1 || newCommands[0].Sequence != steerCommand.Sequence {
			t.Fatalf("replacement did not receive only unacknowledged command: %+v, %v", newCommands, err)
		}
		if _, err := restarted.AppendEvents(ctx, EventBatch{SessionID: replacement.SessionID, Events: []executioncontract.EventEnvelope{event("run-a", 3, executioncontract.EventOutput)}}); err != nil {
			t.Fatalf("replacement could not continue event cursor: %v", err)
		}
		manifest := executioncontract.ArtifactManifest{
			Version: executioncontract.CurrentVersion(), BuildVersion: "worker-test", RunID: "run-a", ManifestID: "manifest-a",
			IdempotencyKey: "artifact:run-a:1", State: executioncontract.ArtifactsReady, GeneratedAt: time.Now().UTC(),
			Artifacts: []executioncontract.ArtifactEntry{{
				Name: "result", Kind: "bundle", Root: executioncontract.RootArtifact, Path: "result.bundle",
				DigestSHA256: strings.Repeat("a", 64), MediaType: "application/octet-stream", Sensitivity: executioncontract.SensitivityInternal,
			}},
		}
		upload := ArtifactUpload{SessionID: replacement.SessionID, Manifest: manifest, Content: []byte("artifact")}
		if err := restarted.UploadArtifact(ctx, upload); err != nil {
			t.Fatalf("UploadArtifact: %v", err)
		}
		if err := restarted.UploadArtifact(ctx, upload); err != nil {
			t.Fatalf("idempotent UploadArtifact: %v", err)
		}
		upload.Content = []byte("different")
		if err := restarted.UploadArtifact(ctx, upload); err == nil {
			t.Fatal("artifact idempotency key accepted different content")
		}
		if err := restarted.Drain(ctx, replacement.SessionID); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if _, err := restarted.PollCommands(ctx, replacement.SessionID, steerCommand.Sequence, 10, 0); err != nil {
			t.Fatalf("draining session could not finish command stream: %v", err)
		}
		stop := command
		stop.CommandID, stop.IdempotencyKey, stop.Type, stop.Payload = "command:stop-a", "stop:run-a", executioncontract.CommandStop, nil
		if _, err := restarted.Enqueue(ctx, replacement.SessionID, nil, stop); !errors.Is(err, ErrStaleSession) {
			t.Fatalf("draining session accepted new command: %v", err)
		}

		diagnostics, err := restarted.Diagnostics(ctx)
		if err != nil || len(diagnostics) < 2 {
			t.Fatalf("Diagnostics = %+v, %v", diagnostics, err)
		}
		encoded, _ := json.Marshal(diagnostics)
		for _, secret := range []string{spec.Prompt.Text, string(command.Payload), "credential"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("diagnostics leaked work content %q: %s", secret, encoded)
			}
		}
	})
}

func TestSessionLeaseAndProtocolFailuresAreExplicit(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	session := register(t, service, "worker-timeout")
	service.now = func() time.Time { return now.Add(time.Minute) }
	if _, err := service.PollCommands(t.Context(), session.SessionID, 0, 10, 0); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired lease error = %v", err)
	}
	if _, err := service.Heartbeat(t.Context(), session.SessionID, nil); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expired heartbeat error = %v, want ErrStaleSession", err)
	}
	_, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-future",
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.Version{Major: 2}, ProtocolMax: executioncontract.Version{Major: 2}, BuildVersion: "future",
		},
	})
	if !errors.Is(err, executioncontract.ErrUnsupportedMajor) {
		t.Fatalf("unsupported protocol error = %v", err)
	}
}

func TestNegotiatedLeaseIsStableAndSafelyCapped(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	// Deliberately retain sub-microsecond precision so the test exercises the
	// database timestamp seam on platforms whose wall clock is already rounded.
	now := time.Unix(1_800_000_000, 123_456_789).UTC()
	service.now = func() time.Time { return now }
	long, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-long-lease", LeaseSeconds: 300,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(10 * time.Second) }
	renewed, err := service.Heartbeat(t.Context(), long.SessionID, nil)
	wantRenewed := db.StoredTime(now.Add(310 * time.Second))
	if err != nil || !renewed.LeaseExpiresAt.Equal(wantRenewed) {
		t.Fatalf("renewed long lease = %v, %v; want %v", renewed.LeaseExpiresAt, err, wantRenewed)
	}
	service.now = func() time.Time { return now }
	capped, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-capped-lease", LeaseSeconds: int(^uint(0) >> 1),
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	})
	if err != nil || !capped.LeaseExpiresAt.Equal(db.StoredTime(now.Add(5*time.Minute))) {
		t.Fatalf("capped lease = %v, %v", capped.LeaseExpiresAt, err)
	}
}

func TestExpiredSessionCannotEnterDrainOrResume(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	session := register(t, service, "worker-expired-drain")
	service.now = func() time.Time { return now.Add(time.Minute) }
	if err := service.Drain(t.Context(), session.SessionID); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired drain error = %v, want ErrLeaseExpired", err)
	}
	if _, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-expired-drain", ResumeSessionID: session.SessionID,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	}); err == nil {
		t.Fatal("expired session resumed")
	}
}

func TestDrainingSessionCanHeartbeatAndResume(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	session := register(t, service, "worker-drain")
	spec, start := startContract(t, "run-drain", "effect-drain")
	if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, start); err != nil {
		t.Fatal(err)
	}
	if err := service.Drain(t.Context(), session.SessionID); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(40 * time.Second) }
	renewed, err := service.Heartbeat(t.Context(), session.SessionID, []string{"draining"})
	if err != nil || renewed.State != "draining" {
		t.Fatalf("draining heartbeat = %+v, %v", renewed, err)
	}
	service.now = func() time.Time { return now.Add(60 * time.Second) }
	replacement, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-drain", ResumeSessionID: session.SessionID,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	})
	if err != nil || replacement.State != "draining" {
		t.Fatalf("draining resume = %+v, %v", replacement, err)
	}
	if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: replacement.SessionID, Events: []executioncontract.EventEnvelope{event("run-drain", 1, executioncontract.EventTerminal)}}); err != nil {
		t.Fatalf("draining replacement could not complete run: %v", err)
	}
	stop := start
	stop.CommandID, stop.IdempotencyKey, stop.Type, stop.Payload = "command:drain-stop", "stop:drain", executioncontract.CommandStop, nil
	if _, err := service.Enqueue(t.Context(), replacement.SessionID, nil, stop); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("draining replacement accepted new command: %v", err)
	}
}

func TestCommandLongPollWakesWithoutDropping(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	session := register(t, service, "worker-long-poll")
	result := make(chan []Command, 1)
	go func() {
		commands, _ := service.PollCommands(context.Background(), session.SessionID, 0, 10, 2*time.Second)
		result <- commands
	}()
	time.Sleep(20 * time.Millisecond)
	spec, command := startContract(t, "run-poll", "effect-poll")
	if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, command); err != nil {
		t.Fatal(err)
	}
	select {
	case commands := <-result:
		if len(commands) != 1 {
			t.Fatalf("long poll commands = %+v", commands)
		}
	case <-time.After(time.Second):
		t.Fatal("long poll did not wake")
	}
}

func TestCommandNotificationWakesEveryWorkerPoll(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	sessionA := register(t, service, "worker-poll-a")
	sessionB := register(t, service, "worker-poll-b")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resultA, resultB := make(chan []Command, 1), make(chan []Command, 1)
	for session, result := range map[string]chan []Command{sessionA.SessionID: resultA, sessionB.SessionID: resultB} {
		go func() {
			commands, _ := service.PollCommands(ctx, session, 0, 10, 2*time.Second)
			result <- commands
		}()
	}
	time.Sleep(20 * time.Millisecond)
	spec, command := startContract(t, "run-poll-b", "effect-poll-b")
	if _, err := service.Enqueue(t.Context(), sessionB.SessionID, &spec, command); err != nil {
		t.Fatal(err)
	}
	select {
	case commands := <-resultB:
		if len(commands) != 1 {
			t.Fatalf("worker B commands = %+v", commands)
		}
	case <-time.After(time.Second):
		t.Fatal("worker B poll was not woken")
	}
}

func TestEveryControlCommandIsReplayableAndIdempotent(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	session := register(t, service, "worker-controls")
	spec, start := startContract(t, "run-controls", "effect-controls")
	if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, start); err != nil {
		t.Fatal(err)
	}
	for _, commandType := range []executioncontract.CommandType{
		executioncontract.CommandStop, executioncontract.CommandSteer, executioncontract.CommandApprovalResponse,
	} {
		envelope := executioncontract.CommandEnvelope{
			Version: executioncontract.CurrentVersion(), BuildVersion: "leader-test", CommandID: "command:" + string(commandType),
			RunID: "run-controls", IdempotencyKey: "control:" + string(commandType), Type: commandType,
			SentAt: time.Now().UTC(), Payload: json.RawMessage(`{"value":true}`),
		}
		first, err := service.Enqueue(t.Context(), session.SessionID, nil, envelope)
		if err != nil {
			t.Fatalf("Enqueue %s: %v", commandType, err)
		}
		second, err := service.Enqueue(t.Context(), session.SessionID, nil, envelope)
		if err != nil || second.Sequence != first.Sequence {
			t.Fatalf("replay %s = %+v, %v; want sequence %d", commandType, second, err, first.Sequence)
		}
		changed := envelope
		changed.Payload = json.RawMessage(`{"value":false}`)
		if _, err := service.Enqueue(t.Context(), session.SessionID, nil, changed); err == nil {
			t.Fatalf("%s accepted changed replay", commandType)
		}
	}
}

func TestResumeRejectsStaleSessionAndInvalidCursor(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	session := register(t, service, "worker-resume")
	if _, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-fresh-cursor", LastCommandAck: 100,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	}); err == nil {
		t.Fatal("fresh session accepted a nonzero acknowledgement cursor")
	}
	spec, start := startContract(t, "run-resume", "effect-resume")
	if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, start); err != nil {
		t.Fatal(err)
	}
	request := RegisterRequest{
		WorkerID: "worker-resume", ResumeSessionID: session.SessionID, LastCommandAck: 2,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	}
	if _, err := service.Register(t.Context(), request); err == nil {
		t.Fatal("resume accepted a cursor beyond durable commands")
	}
	request.LastCommandAck = 0
	replacement, err := service.Register(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(t.Context(), request); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("resume from replaced session error = %v, want ErrStaleSession", err)
	}
	if _, err := service.PollCommands(t.Context(), replacement.SessionID, 0, 10, 0); err != nil {
		t.Fatalf("replacement session was disrupted: %v", err)
	}
}

func TestResumePreservesCursorWhenEveryCommandWasAcknowledged(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	session := register(t, service, "worker-acked-resume")
	spec, start := startContract(t, "run-acked-resume", "effect-acked-resume")
	first, err := service.Enqueue(t.Context(), session.SessionID, &spec, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AckCommands(t.Context(), session.SessionID, first.Sequence); err != nil {
		t.Fatal(err)
	}
	replacement, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-acked-resume", ResumeSessionID: session.SessionID, LastCommandAck: first.Sequence,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AckCommands(t.Context(), replacement.SessionID, first.Sequence); err != nil {
		t.Fatalf("idempotent acknowledgement of inherited cursor: %v", err)
	}
	secondReplacement, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "worker-acked-resume", ResumeSessionID: replacement.SessionID, LastCommandAck: first.Sequence,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
	})
	if err != nil {
		t.Fatalf("second-generation resume with no transferred rows: %v", err)
	}
	replacement = secondReplacement
	stop := start
	stop.CommandID, stop.IdempotencyKey, stop.Type, stop.Payload = "command:after-resume", "stop:after-resume", executioncontract.CommandStop, nil
	next, err := service.Enqueue(t.Context(), replacement.SessionID, nil, stop)
	if err != nil {
		t.Fatal(err)
	}
	if next.Sequence != first.Sequence+1 {
		t.Fatalf("next sequence = %d, want %d", next.Sequence, first.Sequence+1)
	}
	commands, err := service.PollCommands(t.Context(), replacement.SessionID, first.Sequence, 10, 0)
	if err != nil || len(commands) != 1 || commands[0].Sequence != next.Sequence {
		t.Fatalf("commands after inherited cursor = %+v, %v", commands, err)
	}
	if err := service.AckCommands(t.Context(), replacement.SessionID, next.Sequence); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyReplayCannotCrossUnrelatedSession(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	sessionA := register(t, service, "worker-idempotency-a")
	sessionB := register(t, service, "worker-idempotency-b")
	spec, start := startContract(t, "run-idempotency", "effect-idempotency")
	if _, err := service.Enqueue(t.Context(), sessionA.SessionID, &spec, start); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enqueue(t.Context(), sessionB.SessionID, &spec, start); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("cross-session start replay error = %v, want ErrStaleSession", err)
	}
	differentKey := start
	differentKey.CommandID, differentKey.IdempotencyKey = "command:different-key", "start:different-key"
	if _, err := service.Enqueue(t.Context(), sessionB.SessionID, &spec, differentKey); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("cross-session effect replay error = %v, want ErrStaleSession", err)
	}
	stop := start
	stop.CommandID, stop.IdempotencyKey, stop.Type, stop.Payload = "command:idempotency-stop", "stop:idempotency", executioncontract.CommandStop, nil
	if _, err := service.Enqueue(t.Context(), sessionA.SessionID, nil, stop); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enqueue(t.Context(), sessionB.SessionID, nil, stop); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("cross-session control replay error = %v, want ErrStaleSession", err)
	}
}

func TestStartFenceAndEventStreamRejectChangedReplay(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)
	session := register(t, service, "worker-fence")
	spec, start := startContract(t, "run-fence", "effect-fence")
	if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, start); err != nil {
		t.Fatal(err)
	}

	changed := spec
	changed.Prompt.Text = "different prompt"
	payload, _ := json.Marshal(executioncontract.StartCommandPayload{Spec: &changed})
	changedStart := start
	changedStart.CommandID, changedStart.IdempotencyKey, changedStart.Payload = "command:changed", "start:changed", payload
	if _, err := service.Enqueue(t.Context(), session.SessionID, &changed, changedStart); err == nil {
		t.Fatal("same effect fence accepted a changed run spec")
	}

	mismatched := start
	mismatched.CommandID, mismatched.IdempotencyKey = "command:mismatch", "start:mismatch"
	if _, err := service.Enqueue(t.Context(), session.SessionID, &changed, mismatched); err == nil {
		t.Fatal("start accepted a run spec different from its payload")
	}

	first := event("run-fence", 1, executioncontract.EventStarted)
	if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{first}}); err != nil {
		t.Fatal(err)
	}
	mixedBuild := event("run-fence", 2, executioncontract.EventOutput)
	mixedBuild.BuildVersion = "other-worker"
	if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{mixedBuild}}); err == nil {
		t.Fatal("event stream accepted a different worker build")
	}
	terminal := event("run-fence", 2, executioncontract.EventTerminal)
	if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{terminal}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{event("run-fence", 3, executioncontract.EventOutput)}}); err == nil {
		t.Fatal("event accepted after terminal completion")
	}
}

func register(t *testing.T, service *Service, worker string) Session {
	t.Helper()
	session, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: worker,
		Negotiation: executioncontract.Negotiation{
			ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test",
		},
		Capabilities: []string{"provider:test", "os:test"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return session
}

func startContract(t *testing.T, runID, effectID string) (executioncontract.RunSpec, executioncontract.CommandEnvelope) {
	t.Helper()
	now := time.Now().UTC()
	spec := executioncontract.RunSpec{
		Version: executioncontract.CurrentVersion(), BuildVersion: "leader-test", RunID: runID,
		EffectID: effectID, IdempotencyKey: "start:" + effectID,
		Fence: executioncontract.GenerationFence{TaskID: "task-a", TaskGeneration: 1, WorkflowID: "ship", WorkflowGeneration: 1, StepID: "implement"},
		Role:  "implementation", Provider: executioncontract.ProviderIntent{Provider: "test", Model: "test-model"},
		Prompt: executioncontract.Prompt{Text: "sensitive prompt"}, Deadline: now.Add(time.Hour),
		Workspace: executioncontract.Workspace{BaseSHA: strings.Repeat("a", 40), BaseRef: "refs/heads/main", Roots: []executioncontract.LogicalRoot{executioncontract.RootWorktree}},
	}
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	command := executioncontract.CommandEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "leader-test", CommandID: "command:" + effectID,
		RunID: runID, IdempotencyKey: "start:" + effectID, Type: executioncontract.CommandStart, SentAt: now, Payload: payload,
	}
	return spec, command
}

func event(runID string, sequence uint64, eventType executioncontract.EventType) executioncontract.EventEnvelope {
	return executioncontract.EventEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "worker-test", RunID: runID, Sequence: sequence,
		EventID: fmt.Sprintf("event:%d", sequence), IdempotencyKey: fmt.Sprintf("%s:%d", runID, sequence),
		Type: eventType, ObservedAt: time.Now().UTC(), Payload: json.RawMessage(`{"content":"sensitive output"}`),
	}
}
