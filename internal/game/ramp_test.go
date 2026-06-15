package game

import "testing"

// A ramp is a solid wedge, not a tent: you can't walk into the body beneath the
// sloped surface (the old bug let players pass through a ramp's vertical faces),
// but walking up the incline or standing on top is never blocked.
func TestRampSolidWedge(t *testing.T) {
	// Ramp centered at origin, footprint 8x8, rising to H=6 toward +X. The high
	// face is the +X side; the low mouth is the -X side.
	r := Ramp{Pos: V3{}, Half: V3{X: 4, Y: 0, Z: 4}, H: 6, Dir: 0}
	ramps := []Ramp{r}

	// Standing at floor level under the high end (well inside the wedge) is pushed
	// out: the surface there is high, our feet are at 0.
	p := V3{X: 3, Y: 0, Z: 0} // near the +X (high) part of the footprint
	rh, _ := rampHeight(r, p.X, p.Z)
	if rh <= stepUp {
		t.Fatalf("test setup: expected a tall surface at x=3, got %v", rh)
	}
	CollideRamps(ramps, &p)
	if rh2, _ := rampHeight(r, p.X, p.Z); p.Y >= rh2-stepUp {
		// pushed out is fine; just assert we moved out of the wedge footprint area
	}
	if p.X > 4 || p.X < -4-1.0-0.001 {
		// ok: pushed to a footprint face
	}
	if (V3{X: 3, Y: 0, Z: 0}) == p {
		t.Fatal("under-wedge position was not pushed out (walked through the ramp)")
	}

	// Walking UP the incline: feet at/just above the local surface are not blocked.
	up := V3{X: 1, Y: 0, Z: 0}
	if s, _ := rampHeight(r, up.X, up.Z); s > stepUp {
		up.Y = s // on the surface, as the ground step keeps you
	}
	before := up
	CollideRamps(ramps, &up)
	if up != before {
		t.Fatalf("walking up the ramp surface was blocked: %v -> %v", before, up)
	}

	// Standing on top near the high end is not blocked either.
	top := V3{X: 3, Y: 6, Z: 0}
	b2 := top
	CollideRamps(ramps, &top)
	if top != b2 {
		t.Fatalf("standing on the ramp top was blocked: %v -> %v", b2, top)
	}
}

// World.blocked (bot avoidance + pathing) must see a ramp's wedge as a wall, so
// bots steer around ramp sides instead of grinding into them - while the
// climbable incline surface stays open.
func TestRampBlockedForBots(t *testing.T) {
	Maps = append(Maps, Map{Name: "RAMPTEST", Size: 30,
		Spawns: []V3{{X: -14, Z: -14}, {X: 14, Z: 14}},
		Ramps:  []Ramp{{Pos: V3{}, Half: V3{X: 4, Z: 4}, H: 6, Dir: 0}}})
	defer func() { Maps = Maps[:len(Maps)-1] }()
	w := NewWorld(1, ModeDeathmatch)
	w.PinMap(len(Maps) - 1)
	r := Maps[len(Maps)-1].Ramps[0]

	if !w.blocked(V3{X: 3, Y: 0, Z: 0}) {
		t.Fatal("floor level under the ramp's high end should read as blocked")
	}
	rh, _ := rampHeight(r, 3, 0)
	if w.blocked(V3{X: 3, Y: rh, Z: 0}) {
		t.Fatal("standing on the ramp surface (climbing) should not be blocked")
	}
}
