package game

import (
	"math"
	"testing"
)

// navWalk steps a fake bot along navYaw headings (with the sim's collide so it
// can't ghost through walls) and returns its closest approach to goal.
func navWalk(t *testing.T, w *World, goal V3, maxSec float64) float64 {
	t.Helper()
	b := &w.Tanks[0]
	const dt = 1.0 / 30
	best := math.MaxFloat64
	for i := 0; i < int(maxSec/dt); i++ {
		w.ticks++
		d := goal.Sub(b.Pos)
		direct := math.Atan2(d.X, d.Z)
		yaw := w.navYaw(0, goal, direct)
		f := fwd(yaw)
		b.Pos = b.Pos.Add(V3{f.X * 6 * dt, 0, f.Z * 6 * dt})
		clampArena(&b.Pos, w.half())
		w.collide(&b.Pos)
		b.Pos.Y = w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y+stepUp)
		if dd := math.Hypot(goal.X-b.Pos.X, goal.Z-b.Pos.Z); dd < best {
			best = dd
		}
		if best < 2.5 {
			break
		}
	}
	return best
}

func navTestWorld(m Map, start V3) *World {
	Maps = append(Maps, m)
	w := &World{MapIdx: len(Maps) - 1, Phase: PhaseActive}
	w.Tanks = []Tank{{Bot: true, HP: 100, Pos: start, Team: -1, Carrying: -1}}
	return w
}

// A tall wall with one gap at the far end: direct heading is blocked, so the
// bot must route around through the gap. The whiskers alone fail this (the
// wall is concave relative to the goal); the grid path must solve it.
func TestNavRoutesAroundWall(t *testing.T) {
	m := Map{Name: "NAV-WALL", Size: 20, Spawns: []V3{{X: 0, Z: -12}},
		Obstacles: []Box{{Pos: V3{X: -3, Y: 1.5, Z: 0}, Half: V3{X: 16, Y: 1.5, Z: 1.4}}}}
	w := navTestWorld(m, V3{X: 0, Z: -12})
	goal := V3{X: 0, Z: 12}
	if got := navWalk(t, w, goal, 40); got > 3 {
		t.Fatalf("bot never reached the far side of the wall: closest %.1f", got)
	}
}

// An empty arena: navYaw must return the direct heading untouched (no search,
// no deflection) when the straight route is open.
func TestNavDirectWhenOpen(t *testing.T) {
	m := Map{Name: "NAV-EMPTY", Size: 20, Spawns: []V3{{X: 0, Z: -10}}}
	w := navTestWorld(m, V3{X: -8, Z: -8})
	goal := V3{X: 8, Z: 8}
	d := goal.Sub(w.Tanks[0].Pos)
	direct := math.Atan2(d.X, d.Z)
	if got := w.navYaw(0, goal, direct); got != direct {
		t.Fatalf("open field: navYaw deflected direct heading %.2f -> %.2f", direct, got)
	}
}

// The real CROSSROADS map (four long walls in a plus): corner to opposite
// corner used to strand the reactive AI on the wall faces.
func TestNavCrossroads(t *testing.T) {
	idx := -1
	for i := range Maps {
		if Maps[i].Name == "CROSSROADS" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Skip("CROSSROADS not in the embedded map set")
	}
	w := &World{MapIdx: idx, Phase: PhaseActive}
	w.Tanks = []Tank{{Bot: true, HP: 100, Pos: V3{X: -17, Z: -17}, Team: -1, Carrying: -1}}
	if got := navWalk(t, w, V3{X: 17, Z: 17}, 40); got > 3 {
		t.Fatalf("bot never crossed CROSSROADS: closest %.1f", got)
	}
}
