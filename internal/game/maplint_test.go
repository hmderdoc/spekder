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
			if !ok {
				t.Errorf("%s: %s at %v has no walkable cell nearby", m.Name, p.name, p.pos)
				continue
			}
			if g.astar(start, goal) == nil {
				// A pickup perched on top of geometry is a deliberate jump-bonus
				// (ASCENT's corner platforms, CITADEL's wall-top crown): the
				// clearance ring keeps box tops out of the path graph, so
				// they're reachable by jumping, not by route.
				if p.bonus && g.floor[goal] > g.floor[start]+stepUp {
					t.Logf("%s: %s at %v is a jump-only bonus spot", m.Name, p.name, p.pos)
					continue
				}
				// Behind a destructible? Retry with breakables dead: a POI that
				// opens up once a gate falls is authored design, not a defect.
				if openedUp(w, m.Spawns[0], p.pos) {
					t.Logf("%s: %s at %v is gated by a destructible", m.Name, p.name, p.pos)
					continue
				}
				t.Errorf("%s: %s at %v unreachable from spawn[0]", m.Name, p.name, p.pos)
			}
		}
	}
}
