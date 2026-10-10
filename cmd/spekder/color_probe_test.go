package main

import (
	"testing"
	"time"

	"github.com/hmderdoc/termprobe"
)

func TestColorModeForDepth(t *testing.T) {
	if colorModeForDepth(termprobe.DepthTrue) != colorTrue {
		t.Fatal("truecolor")
	}
	if colorModeForDepth(termprobe.Depth256) != color256 {
		t.Fatal("256")
	}
	if colorModeForDepth(termprobe.Depth16) != color16 {
		t.Fatal("16")
	}
}

// fakeTerm is a scripted Term: it answers reads from a fixed byte string and
// records writes.
type fakeTerm struct {
	in  []byte
	out []byte
}

func (f *fakeTerm) Write(p []byte) (int, error) { f.out = append(f.out, p...); return len(p), nil }
func (f *fakeTerm) Close() error                { return nil }
func (f *fakeTerm) Read(p []byte) (int, error)  { return f.ReadTimeout(p, 0) }
func (f *fakeTerm) ReadTimeout(p []byte, d time.Duration) (int, error) {
	if len(f.in) == 0 {
		return 0, nil
	}
	n := copy(p, f.in)
	f.in = f.in[n:]
	return n, nil
}

// The silent terminal (NetRunner, the Linux console) must land on 16 colors;
// SyncTERM's CTerm Device Attributes reply must keep truecolor; and anything
// the caller typed during the probe must survive through pushbackTerm.
func TestProbeColorDepth(t *testing.T) {
	silent := &fakeTerm{}
	if m, res := probeColorDepth(silent, 30*time.Millisecond); m != color16 {
		t.Fatalf("silent terminal: %s (%s)", colorModeName(m), res)
	}
	if string(silent.out) != termprobe.Queries {
		t.Fatalf("queries sent: %q", silent.out)
	}

	st := &fakeTerm{in: []byte("w\x1b[=67;84;101;114;109;1;3;0c")}
	m, res := probeColorDepth(st, time.Second)
	if m != colorTrue {
		t.Fatalf("SyncTERM: %s (%s)", colorModeName(m), res)
	}
	var term Term = &pushbackTerm{Term: st, pending: res.Leftover}
	buf := make([]byte, 8)
	if n, _ := term.Read(buf); n != 1 || buf[0] != 'w' {
		t.Fatalf("pushback lost the keystroke: %q", buf[:n])
	}

	nr := &fakeTerm{in: []byte("\x1bP1$r0;38;5;16m\x1b\\")}
	if m, res := probeColorDepth(nr, 30*time.Millisecond); m != color256 {
		t.Fatalf("quantizing terminal: %s (%s)", colorModeName(m), res)
	}
}
