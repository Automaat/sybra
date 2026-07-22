package agent

import (
	"bytes"
	"errors"
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
