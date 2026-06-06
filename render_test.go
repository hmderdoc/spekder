package main

import (
	"math"
	"testing"
)

// pipPixels returns the offsets-from-center of every body-direction pip pixel
// drawn by drawReticle for the given turret offset. The crosshair is drawn black
// so only the (fixed-color) pip is matched.
func pipPixels(turret float64) (cx, cy int, pts [][2]int) {
	r := newRenderer(80, 50)
	r.drawReticle([3]byte{0, 0, 0}, turret)
	cx, cy = r.W/2, r.H/2
	for y := 0; y < r.H; y++ {
		for x := 0; x < r.W; x++ {
			o := (y*r.W + x) * 3
			if r.fb[o] == 230 && r.fb[o+1] == 240 && r.fb[o+2] == 255 {
				pts = append(pts, [2]int{x - cx, y - cy})
			}
		}
	}
	return
}

// The twist gauge sweeps WITH your aim: turret 0 -> up, turn right (+90) -> marker
// right (clockwise), turn left -> marker left, reversed -> down.
func TestBodyPipDirection(t *testing.T) {
	cases := []struct {
		name           string
		turret         float64
		wantDX, wantDY int // sign of expected average offset (0 = ~centered axis)
	}{
		{"centered=up", 0, 0, -1},
		{"turn-right=marker-right", math.Pi / 2, 1, 0},
		{"turn-left=marker-left", -math.Pi / 2, -1, 0},
		{"reversed=down", math.Pi, 0, 1},
		{"turn-right-45=up-right", math.Pi / 4, 1, -1}, // 5th element (tick) appears
	}
	for _, c := range cases {
		_, _, pts := pipPixels(c.turret)
		if len(pts) == 0 {
			t.Fatalf("%s: no pip pixels drawn", c.name)
		}
		var sx, sy int
		for _, p := range pts {
			sx += p[0]
			sy += p[1]
		}
		ax, ay := float64(sx)/float64(len(pts)), float64(sy)/float64(len(pts))
		if c.wantDX != 0 && sign(ax) != c.wantDX {
			t.Errorf("%s: pip x avg %.1f, want sign %d", c.name, ax, c.wantDX)
		}
		if c.wantDY != 0 && sign(ay) != c.wantDY {
			t.Errorf("%s: pip y avg %.1f, want sign %d", c.name, ay, c.wantDY)
		}
		// On a cardinal direction the off-axis coordinate should be ~0.
		if c.wantDX == 0 && math.Abs(ax) > 1.5 {
			t.Errorf("%s: pip should be vertical, x avg %.1f", c.name, ax)
		}
		if c.wantDY == 0 && math.Abs(ay) > 1.5 {
			t.Errorf("%s: pip should be horizontal, y avg %.1f", c.name, ay)
		}
	}
}

func sign(f float64) int {
	if f > 0.001 {
		return 1
	}
	if f < -0.001 {
		return -1
	}
	return 0
}
