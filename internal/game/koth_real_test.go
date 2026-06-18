package game

import (
	"fmt"
	"testing"
)

// TestElevatedHillsWalkable is the DETERMINISTIC jump-0 guarantee: for every
// elevated KOTH hill, a walk-only route (every step <= stepUp, no jump links)
// must exist from a spawn to the zone. This is what "hills shouldn't be gated by
// whether a character can jump" means in code - a gentle ramp OR a flight of
// shallow steps satisfies it; a sheer ledge or a too-steep ramp does not. Unlike
// the bot-driven climb tests below (stochastic AI/RNG), this is pure geometry, so
// it never flakes and it's the real regression net for ramp/stair authoring.
func TestElevatedHillsWalkable(t *testing.T) {
	for mi := range Maps {
		m := Maps[mi]
		var zone V3
		elevated := false
		for i := range m.Entities {
			if e := &m.Entities[i]; e.Zone != nil && e.Pos.Y >= 1.0 {
				zone, elevated = e.Pos, true
				break
			}
		}
		if !elevated {
			continue
		}
		if len(m.Spawns) == 0 {
			t.Errorf("%s: elevated hill but no spawns", m.Name)
			continue
		}
		w := &World{MapIdx: mi, Phase: PhaseActive}
		w.Tanks = []Tank{{Bot: true, Pos: m.Spawns[0]}}
		w.entities = m.NewEntities()
		g := w.navEnsure()
		g.noHop = true // walk-only: model a jump-0 body (no hop links)

		gx, gy := g.cellAt(zone.X, zone.Z)
		gy0 := zone.Y
		if f := g.floor[gy*g.n+gx]; f > gy0 {
			gy0 = f
		}
		goal, ok := g.snapOpen(gy*g.n+gx, gy0)
		if !ok {
			t.Errorf("%s: hill zone %v has no walkable cell", m.Name, zone)
			continue
		}
		reached := ""
		for si, sp := range m.Spawns {
			sx, sy := g.cellAt(sp.X, sp.Z)
			start, ok := g.snapOpen(sy*g.n+sx, sp.Y)
			if !ok {
				continue
			}
			if g.astar(start, goal) != nil {
				reached = fmt.Sprintf("spawn[%d]", si)
				break
			}
		}
		if reached == "" {
			t.Errorf("%s: NO walk-only (jump-0) path from any spawn to the elevated hill (surfaceY=%.1f) - add a ramp or shallow steps (rise <= %.2f each)", m.Name, zone.Y, stepUp)
		}
	}
}

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
			// A jumping body: this is the bot-AI "do bots find their way up at all"
			// smoke test. The jump-0 (tank) geometry guarantee is the deterministic
			// TestElevatedHillsWalkable above; don't entangle flaky AI with it.
			w.Tanks[i].body = BodyHumanoid
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
