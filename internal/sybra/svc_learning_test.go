package sybra

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/learning"
)

type emittedEvent struct {
	event string
	data  any
}

func newTestLearningService(t *testing.T) (svc *LearningService, events *[]emittedEvent) {
	t.Helper()
	store, err := learning.New(t.TempDir())
	if err != nil {
		t.Fatalf("learning.New: %v", err)
	}
	events = &[]emittedEvent{}
	svc = &LearningService{
		store: store,
		emit: func(event string, data any) {
			*events = append(*events, emittedEvent{event, data})
		},
	}
	return svc, events
}

func TestLearningServiceEmitsOnStore(t *testing.T) {
	svc, events := newTestLearningService(t)

	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	until := since.AddDate(0, 0, 7)
	d := learning.Digest{
		SchemaVersion: learning.SchemaVersion,
		GeneratedAt:   until,
		Since:         since,
		Until:         until,
		ReportDigest:  "digest-1",
	}

	stored, err := svc.StoreDigest(d)
	if err != nil || !stored {
		t.Fatalf("first StoreDigest: stored=%v err=%v", stored, err)
	}
	if len(*events) != 1 {
		t.Fatalf("expected 1 emitted event, got %d", len(*events))
	}
	if (*events)[0].event != "learning:summary" {
		t.Errorf("unexpected event name: %q", (*events)[0].event)
	}

	stored, err = svc.StoreDigest(d)
	if err != nil {
		t.Fatalf("duplicate StoreDigest: unexpected err %v", err)
	}
	if stored {
		t.Error("duplicate StoreDigest should report stored=false")
	}
	if len(*events) != 1 {
		t.Fatalf("duplicate store must not emit again, got %d total events", len(*events))
	}

	digests, err := svc.ListDigests()
	if err != nil {
		t.Fatalf("ListDigests: %v", err)
	}
	if len(digests) != 1 {
		t.Fatalf("expected 1 digest via ListDigests, got %d", len(digests))
	}

	latest, ok, err := svc.GetLatestDigest()
	if err != nil || !ok {
		t.Fatalf("GetLatestDigest: ok=%v err=%v", ok, err)
	}
	if latest.ReportDigest != "digest-1" {
		t.Errorf("unexpected latest digest: %q", latest.ReportDigest)
	}
}
