package main

import (
	"testing"
	"time"
)

// TestParseMMLTickCount guards the SOUND TEST visualizer: parseMML must account
// for every tick the composer wrote, or the falling-note timeline drifts and
// notes vanish. The classic regression was treating 'B'/'F' note letters as the
// MB/MF mode flags (which only exist in the emit-time prefix, never the body),
// silently dropping every B and F note and inflating bar heights.
func TestParseMMLTickCount(t *testing.T) {
	for _, tempo := range []int{96, 123, 150} {
		s := composeSongWith(songParams{scaleIdx: 0, root: 36, tempo: tempo, progIdx: -1, riffIdx: -1})
		sixteenth := time.Minute / time.Duration(tempo) / 4
		for li := range s.levels {
			for si := range s.levels[li] {
				_, total := parseMML(s.levels[li][si])
				want := int(float64(s.durs[li][si]-150*time.Millisecond)/float64(sixteenth) + 0.5)
				if total != want {
					t.Errorf("tempo %d level %d sec %d: parseMML ticks=%d, composer wrote %d\n  mml=%q",
						tempo, li, si, total, want, s.levels[li][si])
				}
			}
		}
	}
}

// TestParseMMLKeepsBandF: B and F are note letters in the body and must parse.
func TestParseMMLKeepsBandF(t *testing.T) {
	evs, total := parseMML("MLT120O3F4B4F8B8")
	if len(evs) != 4 {
		t.Fatalf("want 4 notes (F4 B4 F8 B8), got %d", len(evs))
	}
	if total != 4+4+2+2 {
		t.Errorf("want 12 ticks, got %d", total)
	}
	// F3=41, B3=47
	if evs[0].pitch != 41 || evs[1].pitch != 47 {
		t.Errorf("want F3=41 B3=47, got %d %d", evs[0].pitch, evs[1].pitch)
	}
}
