package agent

import (
	"bytes"
	"testing"
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
