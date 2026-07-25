package agent

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingWriteCloser struct {
	bytes.Buffer
	closed bool
}

func (w *recordingWriteCloser) Close() error {
	w.closed = true
	return nil
}

func TestConvoIOInstallStdinPipeRejectsOverwrite(t *testing.T) {
	c := convoIO{}
	first := &recordingWriteCloser{}
	second := &recordingWriteCloser{}

	if err := c.installStdinPipe(first); err != nil {
		t.Fatalf("installStdinPipe first: %v", err)
	}
	if err := c.installStdinPipe(second); err == nil {
		t.Fatal("installStdinPipe second succeeded, want overwrite rejection")
	}
	if err := c.writeStdin([]byte("hello")); err != nil {
		t.Fatalf("writeStdin: %v", err)
	}
	if got := first.String(); got != "hello" {
		t.Fatalf("first pipe got %q, want hello", got)
	}
	if got := second.String(); got != "" {
		t.Fatalf("second pipe got %q, want empty", got)
	}
}

func TestConvoIOReplaceStdinPipeClosesPrevious(t *testing.T) {
	c := convoIO{}
	first := &recordingWriteCloser{}
	second := &recordingWriteCloser{}

	c.replaceStdinPipe(first)
	c.replaceStdinPipe(second)

	if !first.closed {
		t.Fatal("replaceStdinPipe did not close previous pipe")
	}
	if err := c.writeStdin([]byte("next")); err != nil {
		t.Fatalf("writeStdin: %v", err)
	}
	if got := second.String(); got != "next" {
		t.Fatalf("second pipe got %q, want next", got)
	}
}

type blockingWriteCloser struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingWriteCloser) Write(_ []byte) (int, error) {
	close(w.started)
	<-w.release
	return 0, errors.New("released")
}

func (w *blockingWriteCloser) Close() error { return nil }

func TestConvoIOHasStdinPipeDoesNotBlockBehindWrite(t *testing.T) {
	c := convoIO{}
	w := &blockingWriteCloser{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	c.replaceStdinPipe(w)

	done := make(chan error, 1)
	go func() {
		done <- c.writeStdin([]byte("blocked"))
	}()
	<-w.started

	result := make(chan bool, 1)
	go func() {
		result <- c.hasStdinPipe()
	}()

	select {
	case got := <-result:
		if !got {
			t.Fatal("hasStdinPipe = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("hasStdinPipe blocked behind writeStdin")
	}

	close(w.release)
	<-done
}

// unblockOnCloseWriteCloser models a real *os.File: a blocked Write only
// returns once Close is called on the same handle, mirroring how closing a
// pipe/FIFO fd interrupts a pending write on it.
type unblockOnCloseWriteCloser struct {
	started   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (w *unblockOnCloseWriteCloser) Write(_ []byte) (int, error) {
	close(w.started)
	<-w.closed
	return 0, errors.New("write on closed pipe")
}

func (w *unblockOnCloseWriteCloser) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

// TestConvoIOCloseStdinPipeDoesNotBlockBehindWrite reproduces the deadlock
// chain from #2546: a wedged writeStdin call used to hold stdinMu for the
// entire blocking OS write, so closeStdinPipe (called by StopAgent /
// markAgentDone / the watchdog's stall recovery) blocked on the same mutex
// forever. closeStdinPipe must always be able to force the pipe closed even
// while a write is stuck on it.
func TestConvoIOCloseStdinPipeDoesNotBlockBehindWrite(t *testing.T) {
	c := convoIO{}
	w := &unblockOnCloseWriteCloser{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	c.replaceStdinPipe(w)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- c.writeStdin([]byte("blocked"))
	}()
	<-w.started

	closeDone := make(chan struct{})
	go func() {
		c.closeStdinPipe()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("closeStdinPipe blocked behind writeStdin")
	}

	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("writeStdin succeeded, want error once its pipe was force-closed")
		}
	case <-time.After(time.Second):
		t.Fatal("writeStdin did not unblock after closeStdinPipe")
	}

	if c.hasStdinPipe() {
		t.Fatal("hasStdinPipe = true after closeStdinPipe")
	}
}

// TestConvoIOForceClosePipeIgnoresStalePipe verifies the pointer-identity
// guard in forceClosePipe: a wedged write's timeout can fire after the pipe
// it was writing to has already been superseded by a newer
// replaceStdinPipe/closeStdinPipe call. It must not clobber the newer pipe.
func TestConvoIOForceClosePipeIgnoresStalePipe(t *testing.T) {
	c := convoIO{}
	old := &recordingWriteCloser{}
	newer := &recordingWriteCloser{}

	c.replaceStdinPipe(old)
	c.replaceStdinPipe(newer)

	c.forceClosePipe(old)

	if !c.hasStdinPipe() {
		t.Fatal("forceClosePipe on a stale pipe cleared the active pipe")
	}
	if err := c.writeStdin([]byte("still works")); err != nil {
		t.Fatalf("writeStdin after stale forceClosePipe: %v", err)
	}
	if got := newer.String(); got != "still works" {
		t.Fatalf("newer pipe got %q, want %q", got, "still works")
	}
}
