package game

import (
	"fmt"
	"testing"
)

// openedUp reports whether dst becomes reachable from src once every
// destructible solid is destroyed (a fresh world with breakables marked dead).
func openedUp(w *World, src, dst V3) bool {
	w2 := &World{MapIdx: w.MapIdx, Phase: PhaseActive}
	w2.Tanks = []Tank{{Bot: true, Pos: src}}
	w2.entities = w.ActiveMap().NewEntities()
	for i := range w2.entities {
		if w2.entities[i].Destruct != nil {
			w2.entities[i].Dead = true
		}
	}
	g := w2.navEnsure()
	scx, scy := g.cellAt(src.X, src.Z)
	start, ok := g.snapOpen(scy*g.n+scx, src.Y)
	if !ok {
		return false
	}
	cx, cy := g.cellAt(dst.X, dst.Z)
	y := dst.Y
	if f := g.floor[cy*g.n+cx]; f > y {
		y = f
	}
	goal, ok := g.snapOpen(cy*g.n+cx, y)
	return ok && g.astar(start, goal) != nil
}

// servedByMechanism reports whether an authored traversal mechanic delivers a
// player to pos: a trampoline whose bounce pad sits on reachable ground near pos's
// column (you launch up to it), or a teleporter on reachable ground whose dest
// lands near pos (you warp in). Either is authored design - a flag on a high deck
// or sealed in a vault is reached on purpose, not stranded.
func servedByMechanism(w *World, g *navGrid, start int, pos V3) bool {
	reachable := func(at V3) bool {
		cx, cy := g.cellAt(at.X, at.Z)
		s, ok := g.snapOpen(cy*g.n+cx, at.Y)
		return ok && g.astar(start, s) != nil
	}
	near := func(a V3) bool {
		dx, dz := a.X-pos.X, a.Z-pos.Z
		return dx*dx+dz*dz <= 36 // within ~6 units horizontally of the target column
	}
	for i := range w.entities {
		e := &w.entities[i]
		switch {
		case e.Bounce != nil && near(e.Pos) && reachable(e.Pos):
			return true
		case e.Teleport != nil && near(e.Teleport.Dest) && reachable(e.Pos):
			return true
		}
	}
	return false
}

// TestMapsNavigable lints every embedded map (campaign levels included): from
// the first spawn, the nav grid must reach every other spawn, every flag,
// every zone, and every authored pickup spot. A failure here means authored
// geometry strands bots (or players) - the exact class of bug CROSSROADS had.
func TestMapsNavigable(t *testing.T) {
	base := len(Maps)
	Maps = append(Maps, CampaignMaps...) // lint plays them the way the runner does
	defer func() { Maps = Maps[:base] }()
	for mi := range Maps {
		m := Maps[mi]
		if len(m.Spawns) == 0 {
			t.Errorf("%s: no spawns", m.Name)
			continue
		}
		w := &World{MapIdx: mi, Phase: PhaseActive}
		w.Tanks = []Tank{{Bot: true, Pos: m.Spawns[0]}}
		w.entities = m.NewEntities() // gates/solids count, as in a live match
		g := w.navEnsure()

		type poi struct {
			name  string
			pos   V3
			bonus bool // pickups may be jump-only perches; objectives may not
		}
		var pois []poi
		for i, s := range m.Spawns[1:] {
			pois = append(pois, poi{fmt.Sprintf("spawn[%d]", i+1), s, false})
		}
		for i := range m.Entities {
			e := &m.Entities[i]
			switch {
			case e.Flag != nil:
				pois = append(pois, poi{fmt.Sprintf("flag(team %d)", e.Flag.Team), e.Pos, false})
			case e.Zone != nil:
				pois = append(pois, poi{"zone", e.Pos, false})
			}
		}
		for i, p := range m.Pickups {
			pois = append(pois, poi{fmt.Sprintf("pickup[%d]", i), p.Pos, true})
		}

		scx, scy := g.cellAt(m.Spawns[0].X, m.Spawns[0].Z)
		start, ok := g.snapOpen(scy*g.n+scx, m.Spawns[0].Y)
		if !ok {
			t.Errorf("%s: spawn[0] %v has no walkable cell nearby", m.Name, m.Spawns[0])
			continue
		}
		for _, p := range pois {
			cx, cy := g.cellAt(p.pos.X, p.pos.Z)
			y := p.pos.Y
			if f := g.floor[cy*g.n+cx]; f > y {
				y = f // legacy spots author y=0 even on platform tops
			}
			goal, ok := g.snapOpen(cy*g.n+cx, y)
			if ok && g.astar(start, goal) != nil {
				continue // plainly reachable on foot
			}
			// Not foot-reachable (no walkable cell, or no route): allow the authored
			// exceptions - a jump-bonus perch, a destructible gate, or a traversal
			// mechanic (trampoline / teleporter) that delivers you there.
			if p.bonus && ok && g.floor[goal] > g.floor[start]+stepUp {
				t.Logf("%s: %s at %v is a jump-only bonus spot", m.Name, p.name, p.pos)
				continue
			}
			if openedUp(w, m.Spawns[0], p.pos) {
				t.Logf("%s: %s at %v is gated by a destructible", m.Name, p.name, p.pos)
				continue
			}
			if servedByMechanism(w, g, start, p.pos) {
				t.Logf("%s: %s at %v is reached by a trampoline/teleporter", m.Name, p.name, p.pos)
				continue
			}
			// A level whose forced pilot can fly, leap, or wall-climb reaches flags
			// that foot-pathing can't - in open air, over a wall, or inside a sealed
			// box. That traversal IS the lesson (falcon/butterfly fly, mantis/etc.
			// leap, insect climbs).
			if m.Intro != nil {
				if bd := bodyDefFor(m.Intro.Body); bd.fly || bd.leap || bd.climb {
					t.Logf("%s: %s at %v is reachable by the pilot's flight/leap/climb", m.Name, p.name, p.pos)
					continue
				}
			}
			t.Errorf("%s: %s at %v unreachable from spawn[0]", m.Name, p.name, p.pos)
		}
	}
}
