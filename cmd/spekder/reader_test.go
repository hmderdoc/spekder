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

// ReadTimeout honors d via a read deadline so a lone Esc resolves to quit instead
// of blocking the reader (mirrors the real terminal's timed peek).
func (p pipeTerm) ReadTimeout(b []byte, d time.Duration) (int, error) {
	p.r.SetReadDeadline(time.Now().Add(d))
	n, err := p.r.Read(b)
	p.r.SetReadDeadline(time.Time{})
	if os.IsTimeout(err) {
		return 0, nil
	}
	return n, err
}

// newReaderOnBytes starts the input reader on a pipe and writes the given bytes
// to it, returning the input so the test can inspect quitCh / events.
func newReaderOnBytes(t *testing.T, b []byte) (*input, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	in := &input{quitCh: make(chan struct{}), events: make(chan menuKey, 32), runes: make(chan rune, 64)}
	in.setBinds(effectiveBinds(nil)) // default key bindings, as main() applies at startup (effectiveBinds returns one map)
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

// Esc is the quit key (a lone Esc, resolved via the timed peek).
func TestReaderEscQuits(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("\x1b"))
	defer w.Close()
	if !quitWithin(in, 500*time.Millisecond) {
		t.Fatal("a lone Esc should quit")
	}
}

// Esc followed by a non-sequence key is still a (lone) Esc: quit.
func TestReaderEscThenKeyQuits(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("\x1bz"))
	defer w.Close()
	if !quitWithin(in, 500*time.Millisecond) {
		t.Fatal("'Esc z' should quit (Esc is not an arrow intro)")
	}
}

// Ctrl-C remains a hard quit.
func TestReaderCtrlCQuits(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte{3})
	defer w.Close()
	if !quitWithin(in, 500*time.Millisecond) {
		t.Fatal("Ctrl-C should quit")
	}
}

// 'q' is now inert (no accidental quit mid-game).
func TestReaderQIsInert(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("q"))
	defer w.Close()
	if quitWithin(in, 120*time.Millisecond) {
		t.Fatal("'q' must no longer quit")
	}
}

// A real arrow sequence (ESC [ A) produces its event and must NOT quit.
func TestReaderArrow(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("\x1b[A"))
	defer w.Close()
	select {
	case k := <-in.events:
		if k != mkUp {
			t.Fatalf("expected mkUp from up-arrow, got %v", k)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("up-arrow should produce an mkUp event")
	}
	if quitWithin(in, 120*time.Millisecond) {
		t.Fatal("a bare arrow sequence must not quit")
	}
}

// A cursor-position report (the parked-cursor size probe's reply, ESC[rows;colsR)
// drives a live resize: cols/rows arrive on resizeCh as {cols,rows}. This is the
// path that makes the view responsive on SyncTERM, which ignores ESC[18t.
func TestReaderCursorSizeReportResizes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer w.Close()
	in := &input{
		quitCh:   make(chan struct{}),
		events:   make(chan menuKey, 32),
		runes:    make(chan rune, 64),
		cpr:      make(chan time.Time, 1),
		resizeCh: make(chan [2]int, 1),
	}
	in.setBinds(effectiveBinds(nil))
	go in.reader(pipeTerm{r: r})
	// Terminal reports 36 rows x 120 cols in response to the parked CPR.
	if _, err := w.Write([]byte("\x1b[36;120R")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case sz := <-in.resizeCh:
		if sz[0] != 120 || sz[1] != 36 {
			t.Fatalf("expected {cols,rows}={120,36}, got %v", sz)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a cursor-position report should drive a resize on resizeCh")
	}
	// The same report also clocks the info-panel ping.
	select {
	case <-in.cpr:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("a cursor-position report should also pong the latency clock")
	}
}

// A degenerate/too-small cursor report is rejected (no resize, no panic).
func TestReaderCursorSizeReportRejectsTiny(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer w.Close()
	in := &input{
		quitCh:   make(chan struct{}),
		events:   make(chan menuKey, 32),
		runes:    make(chan rune, 64),
		cpr:      make(chan time.Time, 1),
		resizeCh: make(chan [2]int, 1),
	}
	in.setBinds(effectiveBinds(nil))
	go in.reader(pipeTerm{r: r})
	if _, err := w.Write([]byte("\x1b[2;5R")); err != nil { // 5 rows x 2 cols: below the 8x20 floor
		t.Fatalf("write: %v", err)
	}
	select {
	case sz := <-in.resizeCh:
		t.Fatalf("a sub-minimum size report must not resize, got %v", sz)
	case <-time.After(150 * time.Millisecond):
	}
}

// Uppercase W/A/S/D are cruise-control keys (distinct events from menu nav).
func TestReaderCruiseKeys(t *testing.T) {
	in, w := newReaderOnBytes(t, []byte("W"))
	defer w.Close()
	select {
	case k := <-in.events:
		if k != mkCruiseF {
			t.Fatalf("expected mkCruiseF from Shift+W, got %v", k)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Shift+W should produce mkCruiseF")
	}
}
