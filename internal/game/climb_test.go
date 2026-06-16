package game

import "testing"

// driveInto puts a tank of the given body at the south face of a tall box and
// holds throttle into it for 3s, returning the PEAK height it reached. (Peak, not
// final: an insect crests the box and - throttle still held - walks across the top
// and off the far side, so its final Y is back on the floor.)
func driveInto(t *testing.T, body int) float64 {
	t.Helper()
	Maps = append(Maps, Map{Name: "TEST-WALL", Size: 20,
		Spawns:    []V3{{X: 0, Z: 2}},
		Obstacles: []Box{{Pos: V3{Y: 2, Z: 6}, Half: V3{X: 2, Y: 2, Z: 2}}}}) // top at 4
	defer func() { Maps = Maps[:len(Maps)-1] }()
	w := NewWorld(0, ModeDeathmatch)
	w.PinMap(len(Maps) - 1)
	me := w.AddPlayer([3]float64{}, "P", BodyTank)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{}) // start the match
	w.Tanks[me].body = body
	w.Tanks[me].Pos = V3{X: 0, Z: 2}
	w.Tanks[me].HullYaw = 0 // face +Z, toward the box
	peak := 0.0
	for i := 0; i < 90; i++ { // 3s at 30 Hz
		w.Update(1.0/30, map[int]Input{me: {Throttle: true}})
		if y := w.Tanks[me].Pos.Y; y > peak {
			peak = y
		}
	}
	return peak
}

// The insect scales an obstacle wall by pressing into it; a normal tank just
// bumps the face and stays on the ground.
func TestInsectWallClimb(t *testing.T) {
	if y := driveInto(t, BodyInsect); y < 3 {
		t.Fatalf("insect should scale the wall (top 4); only reached Y=%.2f", y)
	}
	if y := driveInto(t, BodyTank); y > 1 {
		t.Fatalf("a normal tank should NOT climb the wall; reached Y=%.2f", y)
	}
}
