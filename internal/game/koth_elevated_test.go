package game

import "testing"

// elevatedKothWorld builds a world on a central platform (top 3.2) ringed by
// ramps, with a KOTH zone on the platform - the canonical "climb the hill" setup.
func elevatedKothWorld(t *testing.T, bots int) *World {
	t.Helper()
	Maps = append(Maps, Map{Name: "TEST-MESA", Size: 18,
		Spawns:    []V3{{X: -14, Z: -14}, {X: 14, Z: 14}, {X: -14, Z: 14}, {X: 14, Z: -14}},
		Obstacles: []Box{{Pos: V3{Y: 1.6}, Half: V3{X: 4.5, Y: 1.6, Z: 4.5}}}, // top at 3.2
		Ramps: []Ramp{
			{Pos: V3{Z: -7}, Half: V3{X: 3, Z: 2.5}, H: 3.2, Dir: 2},
			{Pos: V3{Z: 7}, Half: V3{X: 3, Z: 2.5}, H: 3.2, Dir: 3},
			{Pos: V3{X: -7}, Half: V3{X: 2.5, Z: 3}, H: 3.2, Dir: 0},
			{Pos: V3{X: 7}, Half: V3{X: 2.5, Z: 3}, H: 3.2, Dir: 1},
		},
		Entities: []Entity{{Pos: V3{Y: 3.4}, Half: V3{X: 4, Y: 1.2, Z: 4}, Zone: &ZoneTrait{Capture: 4}}}})
	w := NewWorld(bots, ModeFFAKotH)
	w.PinMap(len(Maps) - 1)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{}) // start the match so zones/spawns exist
	return w
}

// You hold an elevated hill only when actually up on the platform - not standing
// on the ground beneath it (where the platform's own sides push you out anyway).
func TestZoneMembershipElevation(t *testing.T) {
	defer func() { Maps = Maps[:len(Maps)-1] }()
	w := elevatedKothWorld(t, 0)
	z := &w.zones[0]
	if !w.inZone(z, V3{0, 3.2, 0}) {
		t.Fatal("standing on the platform top should hold the zone")
	}
	if w.inZone(z, V3{0, 0, 0}) {
		t.Fatal("ground level under the platform should NOT hold the zone")
	}
	if w.inZone(z, V3{0, 6, 0}) {
		t.Fatal("flying high over the platform should NOT hold the zone")
	}
	if w.inZone(z, V3{6, 3.2, 0}) {
		t.Fatal("outside the XZ footprint should not hold the zone")
	}
}

// Bots must be able to climb onto an elevated hill (reach it). Whether they then
// CAPTURE depends on the FFA sole-presence brawl, which is stochastic and the same
// on flat hills - so this guards reachability, the thing the ramp-seeking fixes.
func TestBotsClimbElevatedHill(t *testing.T) {
	defer func() { Maps = Maps[:len(Maps)-1] }()
	w := elevatedKothWorld(t, 3)
	for i := range w.Tanks {
		w.Tanks[i].body = BodyTank
	}
	z := &w.zones[0]
	for s := 0; s < 120; s++ {
		drive(w, 1, 1.0/30, map[int]Input{})
		for i := range w.Tanks {
			if !w.Tanks[i].Dead && w.inZone(z, w.Tanks[i].Pos) {
				t.Logf("a bot reached the elevated hill after %ds", s+1)
				return
			}
		}
	}
	t.Fatal("no bot climbed onto the elevated hill in 120s")
}
