package main

import (
	"os"
	"testing"
	"time"
)

// pipeTerm adapts a pipe read end to the Term interface for the reader tests.
type pipeTerm struct{ r *os.File }

func (p pipeTerm) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeTerm) Write(b []byte) (int, error) { return len(b), nil }
func (p pipeTerm) Close() error                { return p.r.Close() }
func (p pipeTerm) ReadTimeout(b []byte, _ time.Duration) (int, error) {
	return p.r.Read(b)
}

// newReaderOnBytes starts the input reader on a pipe and writes the given bytes
// to it, returning the input so the test can inspect quitCh / events.
func newReaderOnBytes(t *testing.T, b []byte) (*input, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	in := &input{quitCh: make(chan struct{}), events: make(chan menuKey, 32)}
	go in.reader(pipeTerm{r: r})
	if _, err := w.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
	return in, w
}

func quitWithin(in *input, d time.Duration) bool {
	select {
	case <-in.quitCh:
		return true
	case <-time.After(d):
		return false
	}
}

// A bare 'q' quits. (sanity)
func TestReaderPlainQuit(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("q"))
	defer w.Close()
	if !quitWithin(in, 500*time.Millisecond) {
		t.Fatal("plain 'q' should quit")
	}
}

// A standalone ESC immediately followed by 'q' must still quit — the ESC must
// not swallow the following key. This is the playtest bug: stray/fragmented ESC
// ate the next keypress so Q never registered.
func TestReaderEscThenQuitNotSwallowed(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("\x1bq"))
	defer w.Close()
	if !quitWithin(in, 500*time.Millisecond) {
		t.Fatal("'ESC q' should quit: the ESC must not swallow the q")
	}
}

// An SS3-style intro 'ESC O' followed by 'q' must also not swallow the q.
func TestReaderSS3ThenQuit(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("\x1bOq"))
	defer w.Close()
	if !quitWithin(in, 500*time.Millisecond) {
		t.Fatal("'ESC O q' should quit: only the 'O' is consumed, not the q")
	}
}

// A real arrow sequence (ESC [ A) followed by 'q' parses the arrow and still
// quits on the q.
func TestReaderArrowThenQuit(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("\x1b[Aq"))
	defer w.Close()
	select {
	case k := <-in.events:
		if k != mkUp {
			t.Fatalf("expected mkUp from up-arrow, got %v", k)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("up-arrow should produce an mkUp event")
	}
	if !quitWithin(in, 500*time.Millisecond) {
		t.Fatal("arrow then 'q' should still quit")
	}
}

// An arrow sequence alone must NOT quit (regression guard against treating a
// lone/leading ESC as quit, which would false-fire on fragmented arrows).
func TestReaderArrowDoesNotQuit(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("\x1b[A"))
	defer w.Close()
	if quitWithin(in, 120*time.Millisecond) {
		t.Fatal("a bare arrow sequence must not quit")
	}
}
