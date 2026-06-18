package game

import (
	"math"
	"math/rand"
)

// Grid pathfinding for bots.
//
// The whisker probes (avoidYaw) are purely reactive: they deflect a heading
// around the nearest face, which strands bots in concave geometry - corridor
// junctions, U-shaped cover, room mouths. This layer bakes a coarse
// walkability grid from the same collision queries the sim uses (so nav and
// physics can't disagree), runs A* over it, and hands the behavior code a
// heading along the route. The whiskers stay on top of it for dynamic, local
// avoidance (other tanks, fresh hazards).
//
// The grid is a single layer sampled at each cell's TOP surface, so ramps and
// the platforms they reach are walkable; a cell under an overhead bridge is
// charted at the bridge deck, not the ground - acceptable until maps gain
// real multi-level interiors.

const (
	navCell    = 1.0  // world units per grid cell
	navMaxIter = 8000 // A* expansion cap; a failed search falls back to direct steering
	navHopRise = 1.5  // max rise an edge may climb beyond stepUp (jump link, costed)
	navDrop    = 4.0  // max drop an edge may fall
)

type navGrid struct {
	n     int       // cells per side
	half  float64   // world half-extent covered
	floor []float64 // per-cell supported surface height (top layer)
	open  []bool    // per-cell: a tank can stand here (clearance + not hazard)
	sig   uint64    // geometry signature the grid was baked against
	noHop bool      // walk-only routing: reject jump links (proves jump-0 reachability)
}

func (g *navGrid) cellAt(x, z float64) (int, int) {
	cx := int((x + g.half) / navCell)
	cy := int((z + g.half) / navCell)
	if cx < 0 {
		cx = 0
	} else if cx >= g.n {
		cx = g.n - 1
	}
	if cy < 0 {
		cy = 0
	} else if cy >= g.n {
		cy = g.n - 1
	}
	return cx, cy
}

func (g *navGrid) center(idx int) (float64, float64) {
	cx, cy := idx%g.n, idx/g.n
	return float64(cx)*navCell - g.half + navCell/2, float64(cy)*navCell - g.half + navCell/2
}

// edgeOK reports whether a tank can step from cell a to adjacent cell b, and
// the cost multiplier for doing so (hop links and drops are discouraged, not
// forbidden, so flat routes win when they exist).
func (g *navGrid) edgeOK(a, b int) (float64, bool) {
	if !g.open[b] {
		return 0, false
	}
	rise := g.floor[b] - g.floor[a]
	switch {
	case rise <= stepUp+0.05 && rise >= -(stepUp+0.05):
		return 1, true
	case rise > 0 && rise <= stepUp+navHopRise:
		if g.noHop {
			return 0, false // walk-only: a jump-0 body can't take a hop link
		}
		return 4, true // jump link: most characters can hop this; cost it
	case rise < 0 && -rise <= navDrop:
		return 1.2, true // dropping off a ledge is cheap
	}
	return 0, false
}

// navSolidSig fingerprints everything the grid is baked from: the map and
// which solid entities are currently alive (a destroyed gate must repath).
func (w *World) navSolidSig() uint64 {
	sig := uint64(w.MapIdx)*1000003 + uint64(len(w.ActiveMap().Obstacles))
	for i := range w.entities {
		e := &w.entities[i]
		if e.Solid && !e.Dead {
			sig = sig*1099511628211 + uint64(i+1)
		}
	}
	return sig
}

// navEnsure returns the current grid, baking (or re-baking) if the map or its
// solid entities changed. Bake is O(cells x boxes): sub-millisecond at arena
// sizes, and rare.
func (w *World) navEnsure() *navGrid {
	sig := w.navSolidSig()
	if w.nav != nil && w.nav.sig == sig {
		return w.nav
	}
	half := w.half()
	n := int((2*half)/navCell) + 1
	g := &navGrid{n: n, half: half, sig: sig,
		floor: make([]float64, n*n), open: make([]bool, n*n)}
	boxes := w.collidables()
	ramps := w.ActiveMap().Ramps
	for i := 0; i < n*n; i++ {
		x, z := g.center(i)
		if math.Abs(x) > half || math.Abs(z) > half {
			continue // outside the play boundary
		}
		f := GroundBoxes(boxes, ramps, x, z, 1e9) // top surface
		g.floor[i] = f
		p := V3{X: x, Y: f + 0.25, Z: z}
		g.open[i] = !navTight(boxes, half, p) && !w.inHazard(V3{X: x, Y: f + 0.05, Z: z})
	}
	w.nav = g
	return g
}

// navTight mirrors World.blocked but with extra clearance: the grid charts a
// cell open only when a tank fits with margin to spare, so razor-thin slots
// the collision system would wedge a tank into never become routes (A* loves
// exactly those shortcuts; the sim then pins the bot in them).
func navTight(boxes []Box, half float64, p V3) bool {
	const rad = 1.5 // World.blocked's 1.1 plus walking margin
	if math.Abs(p.X) > half || math.Abs(p.Z) > half {
		return true
	}
	for _, b := range boxes {
		// A box whose top is at most a step above this cell's surface is
		// terrain, not obstruction - without this, the upper run of every
		// ramp reads as blocked by the very platform it climbs onto.
		if b.Pos.Y+b.Half.Y <= p.Y+stepUp-0.15 {
			continue
		}
		if math.Abs(p.X-b.Pos.X) < b.Half.X+rad && math.Abs(p.Z-b.Pos.Z) < b.Half.Z+rad {
			return true
		}
	}
	return false
}

// lineOpen reports whether a straight ground route runs from a to b: every
// sample is an open cell and the surface never steps more than a tank climbs.
func (g *navGrid) lineOpen(a, b V3) bool {
	dx, dz := b.X-a.X, b.Z-a.Z
	dist := math.Hypot(dx, dz)
	if dist < navCell {
		return true
	}
	steps := int(dist/(navCell*0.5)) + 1
	cx, cy := g.cellAt(a.X, a.Z)
	prevF := g.floor[cy*g.n+cx]
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x, z := a.X+dx*t, a.Z+dz*t
		cx, cy = g.cellAt(x, z)
		idx := cy*g.n + cx
		// The first body-length is exempt from the openness check: a tank
		// hugging cover sits inside the inflation ring, and its own cell
		// must not blind it.
		if !g.open[idx] && dist*t > 1.5 {
			return false
		}
		f := g.floor[idx]
		if math.Abs(f-prevF) > stepUp+0.05 {
			return false
		}
		prevF = f
	}
	return true
}

// snapOpen finds the open cell nearest to idx at roughly height y (small ring
// search), for positions that sit inside an obstacle's clearance ring. The
// height matters: a spot beside a bunker must snap to the ground alongside
// it, not onto its (also walkable) rooftop - A* from the wrong layer goes
// nowhere. ok=false if nothing suitable is nearby.
func (g *navGrid) snapOpen(idx int, y float64) (int, bool) {
	if g.open[idx] && math.Abs(g.floor[idx]-y) <= stepUp+navHopRise {
		return idx, true
	}
	cx0, cy0 := idx%g.n, idx/g.n
	best, bestDF := -1, math.MaxFloat64
	for r := 1; r <= 4; r++ {
		for cy := cy0 - r; cy <= cy0+r; cy++ {
			for cx := cx0 - r; cx <= cx0+r; cx++ {
				if cx < 0 || cy < 0 || cx >= g.n || cy >= g.n {
					continue
				}
				if abs(cx-cx0) != r && abs(cy-cy0) != r {
					continue // ring only
				}
				i := cy*g.n + cx
				if !g.open[i] {
					continue
				}
				if df := math.Abs(g.floor[i] - y); df < bestDF {
					best, bestDF = i, df
				}
			}
		}
		if best >= 0 && bestDF <= stepUp+0.05 {
			return best, true // same-level cell on the nearest possible ring
		}
	}
	if best >= 0 && bestDF <= stepUp+navHopRise {
		return best, true // close-enough level (a hop away)
	}
	return idx, false
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// navHeap is a tiny binary min-heap over (cell, fScore) for A*.
type navHeap struct {
	idx []int
	f   []float64
}

func (h *navHeap) push(i int, f float64) {
	h.idx = append(h.idx, i)
	h.f = append(h.f, f)
	c := len(h.idx) - 1
	for c > 0 {
		p := (c - 1) / 2
		if h.f[p] <= h.f[c] {
			break
		}
		h.idx[p], h.idx[c] = h.idx[c], h.idx[p]
		h.f[p], h.f[c] = h.f[c], h.f[p]
		c = p
	}
}

func (h *navHeap) pop() int {
	top := h.idx[0]
	last := len(h.idx) - 1
	h.idx[0], h.f[0] = h.idx[last], h.f[last]
	h.idx, h.f = h.idx[:last], h.f[:last]
	c := 0
	for {
		l, r := 2*c+1, 2*c+2
		s := c
		if l < len(h.idx) && h.f[l] < h.f[s] {
			s = l
		}
		if r < len(h.idx) && h.f[r] < h.f[s] {
			s = r
		}
		if s == c {
			break
		}
		h.idx[c], h.idx[s] = h.idx[s], h.idx[c]
		h.f[c], h.f[s] = h.f[s], h.f[c]
		c = s
	}
	return top
}

// astar returns the cell path from start to goal (inclusive), or nil.
func (g *navGrid) astar(start, goal int) []int {
	if start == goal {
		return []int{goal}
	}
	n := g.n
	gx, gy := goal%n, goal/n
	hf := func(i int) float64 { // octile distance
		dx, dy := float64(abs(i%n-gx)), float64(abs(i/n-gy))
		if dx < dy {
			dx, dy = dy, dx
		}
		return dx + 0.41421*dy
	}
	gScore := make([]float64, n*n)
	came := make([]int32, n*n)
	state := make([]uint8, n*n) // 0 untouched / 1 open / 2 closed
	for i := range gScore {
		gScore[i] = math.MaxFloat64
	}
	h := &navHeap{}
	gScore[start] = 0
	came[start] = -1
	h.push(start, hf(start))
	state[start] = 1
	for iter := 0; len(h.idx) > 0 && iter < navMaxIter; iter++ {
		cur := h.pop()
		if cur == goal {
			var path []int
			for c := int32(goal); c >= 0; c = came[c] {
				path = append(path, int(c))
			}
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			return path
		}
		if state[cur] == 2 {
			continue // stale heap entry
		}
		state[cur] = 2
		cx, cy := cur%n, cur/n
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				if dx == 0 && dy == 0 {
					continue
				}
				nx, ny := cx+dx, cy+dy
				if nx < 0 || ny < 0 || nx >= n || ny >= n {
					continue
				}
				nb := ny*n + nx
				if state[nb] == 2 {
					continue
				}
				if dx != 0 && dy != 0 { // no cutting corners around solids
					if !g.open[cy*n+nx] || !g.open[ny*n+cx] {
						continue
					}
				}
				mul, ok := g.edgeOK(cur, nb)
				if !ok {
					continue
				}
				step := 1.0
				if dx != 0 && dy != 0 {
					step = 1.41421
				}
				if ng := gScore[cur] + step*mul; ng < gScore[nb] {
					gScore[nb], came[nb] = ng, int32(cur)
					h.push(nb, ng+hf(nb))
					state[nb] = 1
				}
			}
		}
	}
	return nil
}

// navYaw returns the heading bot i should drive to reach goal: `direct` when
// the straight route is open (the common case - no search runs), else a
// heading along a cached/recomputed A* path. Falls back to `direct` when no
// route exists, leaving the whiskers to do what they can.
func (w *World) navYaw(i int, goal V3, direct float64) float64 {
	b := &w.Tanks[i]
	if bodyDefFor(b.body).fly {
		return direct // flyers soar over the geometry
	}
	g := w.navEnsure()
	if g.sig != b.navSig { // grid re-baked (map/gate change): cached path is void
		b.navPath, b.navSig = nil, g.sig
	}
	if g.lineOpen(b.Pos, goal) {
		b.navPath = nil
		return direct
	}
	cx, cy := g.cellAt(b.Pos.X, b.Pos.Z)
	start, sok := g.snapOpen(cy*g.n+cx, b.Pos.Y)
	gx, gy := g.cellAt(goal.X, goal.Z)
	gidx, gok := g.snapOpen(gy*g.n+gx, goal.Y)
	if !sok || !gok {
		return direct
	}
	// Repath when due, when the goal wandered to another cell, or pathless.
	if len(b.navPath) == 0 || w.ticks >= b.navTick || gidx != b.navGoal {
		b.navGoal = gidx
		b.navPath = g.astar(start, gidx)
		b.navAt = 0
		if b.navPath == nil {
			b.navTick = w.ticks + 45 // unreachable: retry occasionally, drive direct
			return direct
		}
		b.navTick = w.ticks + 18 + int64(rand.Intn(12)) // ~0.6-1s at 30Hz, staggered
	}
	// Follow: consume reached waypoints, then steer at the furthest one we can
	// walk straight to (string pulling), looking a handful of cells ahead.
	for b.navAt+1 < len(b.navPath) {
		x, z := g.center(b.navPath[b.navAt])
		if math.Hypot(x-b.Pos.X, z-b.Pos.Z) > navCell*1.3 {
			break
		}
		b.navAt++
	}
	aim := -1
	lim := b.navAt + 10
	if lim >= len(b.navPath) {
		lim = len(b.navPath) - 1
	}
	for j := lim; j >= b.navAt; j-- {
		x, z := g.center(b.navPath[j])
		if g.lineOpen(b.Pos, V3{X: x, Z: z}) {
			aim = j
			break
		}
	}
	if aim < 0 { // strayed off the route entirely: force a repath next tick
		b.navPath, b.navTick = nil, 0
		return direct
	}
	x, z := g.center(b.navPath[aim])
	return math.Atan2(x-b.Pos.X, z-b.Pos.Z)
}
