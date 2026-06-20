package main

import (
	"strings"
	"testing"
)

func scaleByName(t *testing.T, name string) musScale {
	t.Helper()
	for _, s := range musScales {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no scale %q", name)
	return musScale{}
}

func TestDegreePitch(t *testing.T) {
	maj := scaleByName(t, "major")
	// C major from C0 (pitch 0): C D E F G A B C
	want := []int{0, 2, 4, 5, 7, 9, 11, 12}
	for i, w := range want {
		if got := degreePitch(0, maj, i+1); got != w {
			t.Errorf("major degree %d = %d, want %d", i+1, got, w)
		}
	}
	// degree below 1 drops an octave: degree 0 == degree 7 minus an octave.
	if got := degreePitch(12, maj, 0); got != 11 {
		t.Errorf("degree 0 from C1 = %d, want 11 (B0)", got)
	}
}

func TestHarmonicMinorMajorV(t *testing.T) {
	// The defining feature: the raised 7th makes the chord on degree 5 MAJOR
	// (third = 4 semitones), where natural minor's v is minor (3 semitones).
	hm := scaleByName(t, "harmonic minor")
	nm := scaleByName(t, "natural minor")
	third := func(s musScale) int { return degreePitch(0, s, 7) - degreePitch(0, s, 5) }
	if g := third(hm); g != 4 {
		t.Errorf("harmonic-minor V third = %d semitones, want 4 (major)", g)
	}
	if g := third(nm); g != 3 {
		t.Errorf("natural-minor v third = %d semitones, want 3 (minor)", g)
	}
}

func TestRolePitch(t *testing.T) {
	maj := scaleByName(t, "major")
	root := 48 // C4
	one, _ := rolePitch(root, maj, 1, '1')
	oct, _ := rolePitch(root, maj, 1, '8')
	bass, _ := rolePitch(root, maj, 1, 'B')
	if oct != one+12 {
		t.Errorf("octave-root %d should be root+12 (%d)", oct, one+12)
	}
	if bass != one-12 {
		t.Errorf("bass-root %d should be root-12 (%d)", bass, one-12)
	}
	if _, ok := rolePitch(root, maj, 1, '-'); ok {
		t.Error("'-' should be a rest (ok=false)")
	}
}

func TestLenTok(t *testing.T) {
	cases := map[int]string{1: "16", 2: "8", 3: "8.", 4: "4", 6: "4.", 8: "2", 16: "1"}
	for ticks, want := range cases {
		if got := lenTok(ticks); got != want {
			t.Errorf("lenTok(%d) = %q, want %q", ticks, got, want)
		}
	}
}

func TestRiffsBarAligned(t *testing.T) {
	// Every riff must fill exactly one 4/4 bar (16 sixteenth-ticks), else the
	// progression and section timing drift. Guards new riffs added to the library.
	for i, rf := range riffs {
		sum := 0
		for _, n := range rf {
			sum += n.ticks
		}
		if sum != 16 {
			t.Errorf("riff %d sums to %d ticks, want 16", i, sum)
		}
	}
}

func TestComposeSong(t *testing.T) {
	s := composeSong()
	if len(s.levels) != 3 || len(s.durs) != 3 {
		t.Fatalf("want 3 variation levels, got %d/%d", len(s.levels), len(s.durs))
	}
	for lvl := range s.levels {
		secs := s.levels[lvl]
		if len(secs) < 2 || len(secs) != len(s.durs[lvl]) {
			t.Fatalf("level %d: want >=2 matching sections/durs, got %d/%d", lvl, len(secs), len(s.durs[lvl]))
		}
		if lvl == 0 && secs[0] != secs[1] {
			t.Errorf("base level: the first two sections should match")
		}
		for i, sec := range secs {
			if !strings.HasPrefix(sec, "MLT") {
				t.Errorf("level %d section %d should start MLT..., got %q", lvl, i, sec[:min(6, len(sec))])
			}
			if s.durs[lvl][i] <= 0 {
				t.Errorf("level %d section %d duration must be positive", lvl, i)
			}
		}
	}
}

func TestConsecutiveSongsDiffer(t *testing.T) {
	// Two songs in a row should not be identical, and never blues-after-blues.
	a := composeSong()
	aProg := append([]int(nil), lastMus.progA...)
	aBlues := progIsBlues(aProg)
	b := composeSong()
	if a.levels[0][0] == b.levels[0][0] {
		t.Error("consecutive songs produced an identical base section")
	}
	if aBlues && progIsBlues(lastMus.progA) {
		t.Error("blues followed by blues - progressions should change")
	}
}

func TestTransformsPreserveBar(t *testing.T) {
	// Every variation transform must keep each riff at exactly 16 ticks, or the
	// progression and section timing drift.
	all := append(append([]riffXform{}, complicateXforms...), simplifyXforms...)
	for xi, x := range all {
		for ri, base := range riffs {
			for iter := 0; iter < 20; iter++ { // transforms are probabilistic - exercise the branches
				got := x(append(riff(nil), base...))
				sum := 0
				for _, n := range got {
					sum += n.ticks
				}
				if sum != 16 {
					t.Fatalf("transform %d on riff %d -> %d ticks, want 16", xi, ri, sum)
				}
			}
		}
	}
}
