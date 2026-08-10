package task

import (
	"errors"
	"fmt"
	"testing"
)

// countingCreatePersistence is a minimal Persistence stub exercising only
// CreateBy, the one mintAndCreateBy calls — it fails every call with either
// ErrCreateIDCollision (retry, the transient case) or an unrelated error
// (stop immediately, the real-failure case).
type countingCreatePersistence struct {
	calls int
	err   error
}

func (p *countingCreatePersistence) Get(string) (Task, error) { return Task{}, errors.New("unused") }
func (p *countingCreatePersistence) List() ([]Task, error)    { return nil, errors.New("unused") }
func (p *countingCreatePersistence) PutBy(Task, string, []string) (Task, error) {
	return Task{}, errors.New("unused")
}
func (p *countingCreatePersistence) PutFnBy(string, string, func(Task) (Task, []string, error)) (Task, error) {
	return Task{}, errors.New("unused")
}
func (p *countingCreatePersistence) CreateBy(t Task, actor string) (Task, error) {
	p.calls++
	return Task{}, p.err
}
func (p *countingCreatePersistence) UpdateFieldsBy(string, string, func(Task) (Update, error)) (Task, Status, error) {
	return Task{}, "", errors.New("unused")
}
func (p *countingCreatePersistence) DeleteBy(string, string) error  { return errors.New("unused") }
func (p *countingCreatePersistence) RestoreBy(string, string) error { return errors.New("unused") }

var _ Persistence = (*countingCreatePersistence)(nil)

func TestMintAndCreateByStopsImmediatelyOnNonCollisionError(t *testing.T) {
	t.Parallel()
	realErr := errors.New("connection refused")
	p := &countingCreatePersistence{err: realErr}

	_, err := mintAndCreateBy(p, Task{Title: "t"}, "actor")
	if !errors.Is(err, realErr) {
		t.Fatalf("err = %v, want to wrap %v", err, realErr)
	}
	if p.calls != 1 {
		t.Fatalf("CreateBy calls = %d, want 1 (a real error must not be retried)", p.calls)
	}
}

func TestMintAndCreateByRetriesOnCollisionUntilExhausted(t *testing.T) {
	t.Parallel()
	p := &countingCreatePersistence{err: fmt.Errorf("wrapped: %w", ErrCreateIDCollision)}

	_, err := mintAndCreateBy(p, Task{Title: "t"}, "actor")
	if err == nil {
		t.Fatal("expected an error after exhausting mint attempts")
	}
	if p.calls != maxTaskIDMintAttempts {
		t.Fatalf("CreateBy calls = %d, want %d (every attempt collided)", p.calls, maxTaskIDMintAttempts)
	}
}
