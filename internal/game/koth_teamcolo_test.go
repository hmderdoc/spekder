package game

import "testing"

// The exact demo scenario the user saw: TEAM KOTH forced onto generic maps
// (colosseum/canyon/suburbia) via the fallback pool. The auto hill must land on
// reachable ground and actually get captured, not freeze on a central dais.
func TestTeamKothGenericMapsCapture(t *testing.T) {
	for _, name := range []string{"COLOSSEUM", "CANYON", "SUBURBIA"} {
		mi := -1
		for i := range Maps {
			if Maps[i].Name == name {
				mi = i
			}
		}
		if mi < 0 {
			t.Errorf("%s not found", name)
			continue
		}
		w := NewWorld(8, ModeTeamKotH)
		w.PinMap(mi)
		drive(w, countdownTime+0.2, 1.0/30, map[int]Input{})
		for i := range w.Tanks {
			w.Tanks[i].body = BodyTank
		}
		z := &w.zones[0]
		captured, when := false, 0
		for s := 0; s < 90 && !captured; s++ {
			drive(w, 1, 1.0/30, map[int]Input{})
			if w.teamScore[0] > 0 || w.teamScore[1] > 0 {
				captured, when = true, s+1
			}
		}
		if !captured {
			t.Errorf("%s: TEAM KOTH hill (surfaceY=%.1f) never scored in 90s", name, z.Pos.Y)
		} else {
			t.Logf("%-10s TEAM KOTH scored @%ds (hill at %.1f,%.1f surfaceY=%.1f)", name, when, z.Pos.X, z.Pos.Z, z.Pos.Y)
		}
	}
}
