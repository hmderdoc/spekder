package game

import "testing"

// Bots must contest the hill in KotH: in an all-bot match, hold points have to
// accrue without any human steering anyone onto the zone. (This was visibly
// broken in demo mode: scores never moved because bots only chased enemies.)
func TestBotsContestTheHill(t *testing.T) {
	Maps = append(Maps, Map{Name: "TEST-HILL", Size: 18,
		Spawns:   []V3{{X: -14, Z: -14}, {X: 14, Z: 14}, {X: -14, Z: 14}, {X: 14, Z: -14}},
		Entities: []Entity{{Pos: V3{Y: 0.1}, Half: V3{X: 4, Y: 1, Z: 4}, Zone: &ZoneTrait{}}}})
	defer func() { Maps = Maps[:len(Maps)-1] }()
	// Two bots: every kill leaves a solo capture window. Pin them to the grounded
	// tank after the countdown so the test exercises the capture MECHANIC, not the
	// luck of the character roll (a flyer hovering above the zone, or a healer that
	// won't fight, can otherwise leave the hill uncontested - that's roster, not a
	// KotH bug).
	w := NewWorld(2, ModeFFAKotH)
	w.PinMap(len(Maps) - 1)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{})
	for i := range w.Tanks {
		w.Tanks[i].body, w.Tanks[i].Vehicle = BodyTank, ChassisFor(BodyTank)
	}
	for s := 0; s < 120; s++ { // sample each sim-second; bot rolls are random
		drive(w, 1, 1.0/30, map[int]Input{})
		for i := range w.Tanks {
			if w.Tanks[i].holdScore > 0 {
				return // someone captured and held: the mode functions
			}
		}
	}
	t.Fatal("120s of all-bot KotH and nobody scored a hold point")
}
