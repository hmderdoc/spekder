package game

import "testing"

// Every authored elevated KOTH hill must be CLIMBABLE by bots: at least one bot
// reaches the platform within a sane time. (Whether they then capture depends on
// the FFA sole-presence brawl, which is the same on flat hills and not what this
// guards.) This is the regression net for "bots circle the base, hill looks dead".
func TestBotsReachElevatedHills(t *testing.T) {
	for mi := range Maps {
		m := Maps[mi]
		surfY := 0.0
		for _, e := range m.Entities {
			if e.Zone != nil && e.Pos.Y > surfY {
				surfY = e.Pos.Y
			}
		}
		if surfY < 1.0 { // only the elevated, authored hills
			continue
		}
		w := NewWorld(3, ModeFFAKotH)
		w.PinMap(mi)
		drive(w, countdownTime+0.2, 1.0/30, map[int]Input{})
		for i := range w.Tanks {
			w.Tanks[i].body, w.Tanks[i].Vehicle = BodyTank, ChassisFor(BodyTank)
		}
		z := &w.zones[0]
		reached, when := false, 0
		for s := 0; s < 100 && !reached; s++ {
			drive(w, 1, 1.0/30, map[int]Input{})
			for i := range w.Tanks {
				if !w.Tanks[i].Dead && w.inZone(z, w.Tanks[i].Pos) {
					reached, when = true, s+1
				}
			}
		}
		if !reached {
			t.Errorf("%s (hill surfaceY=%.1f): no bot climbed onto the hill in 100s", m.Name, z.Pos.Y)
		} else {
			t.Logf("%-10s climbed @%ds (surfaceY=%.1f)", m.Name, when, z.Pos.Y)
		}
	}
}
