package workercontrol

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

var (
	updateOld = strings.Repeat("a", 40)
	updateNew = strings.Repeat("b", 40)
)

func registerUpdateWorker(t *testing.T, service *Service, build, resume string) Session {
	t.Helper()
	session, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: "update-worker", ResumeSessionID: resume,
		Negotiation:  executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: build},
		Capabilities: []string{"capacity=2", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce", "readiness=ready", "buffered_events=0", "pending_artifacts=0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestUpdateHoldPreservesOperatorStateAcrossRestarts(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		service := New(engine.Open(t))
		service.SetUpdateRevision(updateNew)
		session := registerUpdateWorker(t, service, updateOld, "")
		request := UpdateRequest{SessionID: session.SessionID, ID: strings.Repeat("c", 32), Revision: updateNew}
		hold, err := service.BeginUpdate(t.Context(), request)
		if err != nil || hold.PreviousRevision != updateOld {
			t.Fatalf("begin = %+v, %v", hold, err)
		}
		duplicate, err := service.BeginUpdate(t.Context(), request)
		if err != nil || duplicate != hold {
			t.Fatalf("lost reply retry = %+v, %v", duplicate, err)
		}
		spec, command := startContract(t, "update-new-run", "update-new-effect")
		if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, command); !errors.Is(err, ErrUpdateHeld) {
			t.Fatalf("direct start during hold = %v", err)
		}
		placement := placementRequest(t, "update-placed-run", "update-placed-effect")
		placement.NodeOverride = session.WorkerID
		if _, err := service.ScheduleStart(t.Context(), placement); !errors.Is(err, ErrNoEligibleWorker) {
			t.Fatalf("placement during hold = %v", err)
		}
		// Neither an operator enable nor a replacement session bypasses the hold.
		if err := service.SetWorkerDisabled(t.Context(), session.WorkerID, false); err != nil {
			t.Fatal(err)
		}
		restarted := New(engine.Open(t))
		restarted.SetUpdateRevision(updateNew)
		next := registerUpdateWorker(t, restarted, updateNew, session.SessionID)
		request.SessionID = next.SessionID
		rows, err := restarted.Diagnostics(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for i := range rows {
			row := &rows[i]
			if row.SessionID == next.SessionID {
				found = row.UpdateHeld && row.AvailableCapacity == 0
			}
		}
		if !found {
			t.Fatal("replacement bypassed durable hold")
		}
		if err := restarted.SetWorkerDisabled(t.Context(), next.WorkerID, true); err != nil {
			t.Fatal(err)
		}
		wrong := request
		wrong.ID = strings.Repeat("d", 32)
		if err := restarted.FinishUpdate(t.Context(), wrong); !errors.Is(err, ErrUpdateHeld) {
			t.Fatalf("foreign hold release = %v", err)
		}
		if err := restarted.FinishUpdate(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		if err := restarted.FinishUpdate(t.Context(), request); err != nil {
			t.Fatalf("repeated finish = %v", err)
		}
		current, err := restarted.session(t.Context(), next.SessionID)
		if err != nil || current.State != "disabled" {
			t.Fatalf("operator disable overwritten: %+v, %v", current, err)
		}
	})
}

func TestUpdateHoldDrainsAcceptedWork(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		service := New(engine.Open(t))
		service.SetUpdateRevision(updateNew)
		session := registerUpdateWorker(t, service, updateOld, "")
		spec, command := startContract(t, "accepted-update-run", "accepted-update-effect")
		if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, command); err != nil {
			t.Fatal(err)
		}
		request := UpdateRequest{SessionID: session.SessionID, ID: strings.Repeat("c", 32), Revision: updateNew}
		if _, err := service.BeginUpdate(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		if err := service.FinishUpdate(t.Context(), request); !errors.Is(err, ErrUpdateHeld) {
			t.Fatalf("released while accepted work remains: %v", err)
		}
		if err := service.CheckUpdate(t.Context(), request); !errors.Is(err, ErrUpdateHeld) {
			t.Fatalf("check ignored accepted work: %v", err)
		}
		request.WorkerID = session.WorkerID
		if err := service.CheckUpdateOwnership(t.Context(), request); !errors.Is(err, ErrUpdateHeld) {
			t.Fatalf("offline rollback ignored accepted work: %v", err)
		}
		commands, err := service.PollCommands(t.Context(), session.SessionID, 0, 10, 0)
		if err != nil || len(commands) != 1 {
			t.Fatalf("accepted command lost during drain: %v, %v", commands, err)
		}
		terminal := event(spec.RunID, 1, executioncontract.EventTerminal)
		terminal.BuildVersion = updateOld
		if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{terminal}}); err != nil {
			t.Fatalf("handback refused during update: %v", err)
		}
		if err := service.FinishUpdate(t.Context(), request); err != nil {
			t.Fatalf("abort on healthy retained build failed: %v", err)
		}
		if err := service.CheckUpdateOwnership(t.Context(), request); !errors.Is(err, ErrUpdateHeld) {
			t.Fatalf("absent hold authorized rollback: %v", err)
		}
	})
}

func TestUpdateOwnershipSurvivesLeaseLossButNotRelease(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		service := New(engine.Open(t))
		service.SetUpdateRevision(updateNew)
		session := registerUpdateWorker(t, service, updateOld, "")
		request := UpdateRequest{SessionID: session.SessionID, WorkerID: session.WorkerID, ID: strings.Repeat("e", 32), Revision: updateNew}
		if _, err := service.BeginUpdate(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		clock := service.now()
		service.now = func() time.Time { return clock.Add(time.Hour) }
		if err := service.CheckUpdate(t.Context(), request); !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("expired session accepted: %v", err)
		}
		if err := service.CheckUpdateOwnership(t.Context(), request); err != nil {
			t.Fatalf("offline hold proof failed: %v", err)
		}
		wrong := request
		wrong.WorkerID = "another-worker"
		if err := service.CheckUpdateOwnership(t.Context(), wrong); !errors.Is(err, ErrUpdateHeld) {
			t.Fatalf("another worker authorized: %v", err)
		}
	})
}

func TestUpdateHoldRacesPlacement(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		service := New(engine.Open(t))
		service.SetUpdateRevision(updateNew)
		session := registerUpdateWorker(t, service, updateOld, "")
		request := UpdateRequest{SessionID: session.SessionID, ID: strings.Repeat("c", 32), Revision: updateNew}
		start := placementRequest(t, "race-update-run", "race-update-effect")
		start.NodeOverride = session.WorkerID
		var wg sync.WaitGroup
		var holdErr, placeErr error
		wg.Go(func() { _, holdErr = service.BeginUpdate(t.Context(), request) })
		wg.Go(func() { _, placeErr = service.ScheduleStart(t.Context(), start) })
		wg.Wait()
		if holdErr != nil {
			t.Fatal(holdErr)
		}
		if placeErr != nil && !errors.Is(placeErr, ErrNoEligibleWorker) {
			t.Fatal(placeErr)
		}
		rows, err := service.Diagnostics(t.Context())
		if err != nil || len(rows) != 1 || !rows[0].UpdateHeld || rows[0].AvailableCapacity != 0 {
			t.Fatalf("race diagnostics = %+v, %v", rows, err)
		}
		if placeErr == nil && rows[0].ActiveRuns != 1 {
			t.Fatal("accepted pre-hold reservation not visible to drain")
		}
		later := placementRequest(t, "late-update-run", "late-update-effect")
		later.NodeOverride = session.WorkerID
		if _, err := service.ScheduleStart(t.Context(), later); !errors.Is(err, ErrNoEligibleWorker) {
			t.Fatalf("post-hold placement = %v", err)
		}
	})
}
