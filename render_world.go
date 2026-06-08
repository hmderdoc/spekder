package main

import (
	"math"

	gm "spekder/internal/game"
)

const hitFlashTime = 0.30 // sec the red damage flash lingers, fading to clear

// ---------------------------------------------------------------------------
// dynamic entity models (game state -> world-space triangles, per frame)
// ---------------------------------------------------------------------------

// rotY rotates a local point about Y by (sin, cos), mapping local +Z onto the
// heading so a model built facing +Z faces the tank's yaw.
func rotY(l V3, s, c float64) V3 { return V3{l.X*c + l.Z*s, l.Y, -l.X*s + l.Z*c} }

func quad(dst []Tri, a, b, c, d V3, col [3]float64) []Tri {
	return append(dst,
		Tri{v: [3]V3{a, b, c}, col: col, wire: true},
		Tri{v: [3]V3{a, c, d}, col: col, wire: true})
}

// box appends a box (local center/half, transformed to world by xf).
func box(dst []Tri, center, half V3, col [3]float64, xf func(V3) V3) []Tri {
	cx, cy, cz := center.X, center.Y, center.Z
	hx, hy, hz := half.X, half.Y, half.Z
	p := [8]V3{
		xf(V3{cx - hx, cy - hy, cz - hz}), xf(V3{cx + hx, cy - hy, cz - hz}),
		xf(V3{cx + hx, cy + hy, cz - hz}), xf(V3{cx - hx, cy + hy, cz - hz}),
		xf(V3{cx - hx, cy - hy, cz + hz}), xf(V3{cx + hx, cy - hy, cz + hz}),
		xf(V3{cx + hx, cy + hy, cz + hz}), xf(V3{cx - hx, cy + hy, cz + hz}),
	}
	sh := func(f float64) [3]float64 { return [3]float64{col[0] * f, col[1] * f, col[2] * f} }
	dst = quad(dst, p[4], p[5], p[6], p[7], sh(1.00))
	dst = quad(dst, p[1], p[0], p[3], p[2], sh(0.78))
	dst = quad(dst, p[5], p[1], p[2], p[6], sh(0.88))
	dst = quad(dst, p[0], p[4], p[7], p[3], sh(0.70))
	dst = quad(dst, p[3], p[7], p[6], p[2], sh(1.00))
	dst = quad(dst, p[4], p[0], p[1], p[5], sh(0.55))
	return dst
}

func appendTank(dst []Tri, t *gm.TankSnap) []Tri {
	sh, ch := math.Sin(t.HullYaw), math.Cos(t.HullYaw)
	sa, ca := math.Sin(t.HullYaw+t.TurretYaw), math.Cos(t.HullYaw+t.TurretYaw)
	xfHull := func(l V3) V3 { return rotY(l, sh, ch).Add(t.Pos) }
	xfTur := func(l V3) V3 { return rotY(l, sa, ca).Add(t.Pos) }
	// The gun barrel also pitches: rotate about the turret pivot (local Y = 1.05*s)
	// in the YZ plane, then yaw, then translate. +pitch raises the muzzle.
	sp, cp := math.Sin(t.TurretPitch), math.Cos(t.TurretPitch)
	col := t.Color
	if t.Shield { // spawn-protected: washed-out so it reads as invulnerable
		col = [3]float64{(col[0] + 0.6) / 2, (col[1] + 0.6) / 2, (col[2] + 0.6) / 2}
	}
	if t.Hit { // just took damage: flash white
		col = [3]float64{1, 1, 1}
	}
	bright := [3]float64{math.Min(col[0]*1.25, 1), math.Min(col[1]*1.25, 1), math.Min(col[2]*1.25, 1)}
	gun := [3]float64{0.22, 0.22, 0.26}
	s := gm.Veh(t.Vehicle).Scale // vehicle class sizes the model
	pivotY := 1.05 * s
	xfBarrel := func(l V3) V3 {
		ly := l.Y - pivotY
		pp := V3{l.X, ly*cp + l.Z*sp + pivotY, -ly*sp + l.Z*cp}
		return rotY(pp, sa, ca).Add(t.Pos)
	}
	dst = box(dst, V3{0, 0.45 * s, 0}, V3{0.9 * s, 0.45 * s, 1.3 * s}, col, xfHull)            // hull
	dst = box(dst, V3{0, 1.05 * s, 0}, V3{0.55 * s, 0.32 * s, 0.55 * s}, bright, xfTur)        // turret
	dst = box(dst, V3{0, 1.05 * s, 1.05 * s}, V3{0.12 * s, 0.12 * s, 0.85 * s}, gun, xfBarrel) // barrel (pitches)
	return dst
}

func appendShot(dst []Tri, pos V3) []Tri {
	xf := func(l V3) V3 { return l.Add(pos) }
	return box(dst, V3{}, V3{0.16, 0.16, 0.16}, [3]float64{1.0, 0.85, 0.30}, xf)
}

// appendRamp builds a drive-up wedge: a sloped top surface plus vertical skirts
// down to the floor (degenerate zero-height skirts are skipped by the rasterizer).
func appendRamp(dst []Tri, r gm.Ramp) []Tri {
	hx, hz := r.Half.X, r.Half.Z
	px, pz := r.Pos.X, r.Pos.Z
	// footprint corners A(-,-) B(+,-) C(+,+) D(-,+), heights set by rise direction
	a := V3{px - hx, 0, pz - hz}
	b := V3{px + hx, 0, pz - hz}
	c := V3{px + hx, 0, pz + hz}
	d := V3{px - hx, 0, pz + hz}
	switch r.Dir {
	case 0: // +X high
		b.Y, c.Y = r.H, r.H
	case 1: // -X high
		a.Y, d.Y = r.H, r.H
	case 2: // +Z high
		c.Y, d.Y = r.H, r.H
	default: // -Z high
		a.Y, b.Y = r.H, r.H
	}
	dst = quad(dst, a, b, c, d, r.Color) // sloped top
	// skirts: each footprint edge down to y=0
	dark := [3]float64{r.Color[0] * 0.7, r.Color[1] * 0.7, r.Color[2] * 0.7}
	edge := func(p, q V3) {
		dst = quad(dst, V3{p.X, 0, p.Z}, V3{q.X, 0, q.Z}, q, p, dark)
	}
	edge(a, b)
	edge(b, c)
	edge(c, d)
	edge(d, a)
	return dst
}

// appendProp builds decorative scenery (an "obelisk"/pyramid). yawAnim spins
// about the world origin, so only center-placed obelisks animate (others static).
func appendProp(dst []Tri, p gm.Prop) []Tri {
	const base = 1.0
	apex := V3{p.Pos.X, p.Pos.Y + p.H, p.Pos.Z}
	y0 := p.Pos.Y + 0.2
	c := []V3{
		{p.Pos.X - base, y0, p.Pos.Z - base}, {p.Pos.X + base, y0, p.Pos.Z - base},
		{p.Pos.X + base, y0, p.Pos.Z + base}, {p.Pos.X - base, y0, p.Pos.Z + base},
	}
	anim := p.Kind == "obelisk" && p.Pos.X == 0 && p.Pos.Z == 0
	for i := 0; i < 4; i++ {
		a, b := c[i], c[(i+1)%4]
		dst = append(dst, Tri{v: [3]V3{a, b, apex}, col: p.Color, wire: true, yawAnim: anim})
	}
	return dst
}

// appendFlag builds a flag: a pale pole with a colored banner. Neutral (Flag
// Run) flags are yellow; CTF flags are team-colored and get a base pad at home
// so the objective stays visible even while the flag is carried away.
func appendFlag(dst []Tri, f gm.FlagSnap) []Tri {
	col := [3]float64{0.98, 0.86, 0.18} // neutral yellow (Flag Run)
	switch f.Team {
	case 0:
		col = [3]float64{0.95, 0.30, 0.28} // red team
	case 1:
		col = [3]float64{0.30, 0.50, 0.95} // blue team
	}
	if f.Team >= 0 { // CTF base pad at home
		dim := [3]float64{col[0] * 0.5, col[1] * 0.5, col[2] * 0.5}
		bxf := func(l V3) V3 { return l.Add(f.Home) }
		dst = box(dst, V3{0, 0.05, 0}, V3{1.4, 0.05, 1.4}, dim, bxf)
	}
	yoff := 0.0
	if f.Carried {
		yoff = 1.2 // ride above the carrier so it reads at a glance
	}
	xf := func(l V3) V3 { return l.Add(V3{f.Pos.X, f.Pos.Y + yoff, f.Pos.Z}) }
	dst = box(dst, V3{0, 0.9, 0}, V3{0.06, 0.9, 0.06}, [3]float64{0.85, 0.85, 0.9}, xf) // pole
	dst = box(dst, V3{0.34, 1.5, 0}, V3{0.34, 0.24, 0.04}, col, xf)                     // banner
	return dst
}

// pickupColor returns the banner/box color for a power-up kind.
func pickupColor(kind int) [3]float64 {
	switch kind {
	case gm.PickRepair:
		return [3]float64{0.30, 0.90, 0.40} // green cross = heal
	case gm.PickShield:
		return [3]float64{0.35, 0.65, 0.95} // blue = shield
	case gm.PickRapid:
		return [3]float64{0.95, 0.70, 0.20} // amber = rapid fire
	case gm.PickCloak:
		return [3]float64{0.75, 0.45, 0.90} // violet = cloak
	}
	return [3]float64{0.85, 0.85, 0.85}
}

// appendPickup builds a small floating box marking a power-up drop, colored by
// kind so players learn the palette at a glance.
func appendPickup(dst []Tri, p gm.PickupSnap) []Tri {
	col := pickupColor(p.Kind)
	xf := func(l V3) V3 { return l.Add(V3{p.Pos.X, p.Pos.Y + 0.6, p.Pos.Z}) }
	dst = box(dst, V3{0, 0, 0}, V3{0.34, 0.34, 0.34}, col, xf)
	return dst
}

// appendEntity builds a map trait-object from its static template (e) merged
// with its current dynamic state (st from the wire). A turret is a base plus a
// head+barrel that rotates to st.Yaw; other kinds render as a solid block.
// Destructibles redden as their HP falls; a dead entity draws nothing.
func appendEntity(dst []Tri, e gm.Entity, st gm.EntitySnap) []Tri {
	if st.Dead {
		return dst
	}
	if e.Flag != nil || e.Zone != nil {
		return dst // inert objective marker; the runtime flag/zone snap renders it
	}
	col := e.Color
	if col == ([3]float64{}) {
		col = [3]float64{0.55, 0.57, 0.62}
	}
	if e.Destruct != nil && e.Destruct.MaxHP > 0 { // damage tint: fade toward red
		if f := float64(st.HP) / float64(e.Destruct.MaxHP); f < 1 {
			if f < 0 {
				f = 0
			}
			col = [3]float64{col[0] + (0.90-col[0])*(1-f), col[1] * f, col[2] * f}
		}
	}
	switch e.Kind {
	case "turret":
		baseXf := func(l V3) V3 { return l.Add(e.Pos) }
		dst = box(dst, V3{}, e.Half, col, baseXf) // base, centered on Pos like an obstacle
		s, c := math.Sin(st.Yaw), math.Cos(st.Yaw)
		top := V3{e.Pos.X, e.Pos.Y + e.Half.Y, e.Pos.Z}
		headXf := func(l V3) V3 { return rotY(l, s, c).Add(top) }
		sp, cp := math.Sin(st.Pitch), math.Cos(st.Pitch)
		barXf := func(l V3) V3 { // pitch the barrel about the head pivot (local Y = 0.3)
			ly := l.Y - 0.3
			pp := V3{l.X, ly*cp + l.Z*sp + 0.3, -ly*sp + l.Z*cp}
			return rotY(pp, s, c).Add(top)
		}
		bright := [3]float64{math.Min(col[0]*1.2, 1), math.Min(col[1]*1.2, 1), math.Min(col[2]*1.2, 1)}
		gun := [3]float64{0.20, 0.20, 0.24}
		dst = box(dst, V3{0, 0.3, 0}, V3{0.45, 0.3, 0.45}, bright, headXf) // rotating head
		dst = box(dst, V3{0, 0.3, 0.55}, V3{0.1, 0.1, 0.55}, gun, barXf)   // barrel (pitches)
	case "trampoline":
		// A low pad with a brighter raised "mat" on top so it reads as springy.
		xf := func(l V3) V3 { return l.Add(e.Pos) }
		dst = box(dst, V3{}, V3{e.Half.X, e.Half.Y * 0.5, e.Half.Z}, [3]float64{col[0] * 0.5, col[1] * 0.5, col[2] * 0.5}, xf)
		mat := [3]float64{math.Min(col[0]*1.3, 1), math.Min(col[1]*1.3, 1), math.Min(col[2]*1.3, 1)}
		dst = box(dst, V3{0, e.Half.Y, 0}, V3{e.Half.X * 0.92, e.Half.Y * 0.2, e.Half.Z * 0.92}, mat, xf)
	default:
		xf := func(l V3) V3 { return l.Add(e.Pos) }
		dst = box(dst, V3{}, e.Half, col, xf) // generic solid block (wall, cover, ...)
	}
	return dst
}

// appendZone builds a King-of-the-Hill control zone: a low pad tinted by the
// controller's color (grey when neutral), brightening with capture progress so
// you can read who's taking it.
func appendZone(dst []Tri, z gm.ZoneSnap) []Tri {
	col := z.Color
	if z.Prog > 0 { // brighten toward white as it's captured
		f := z.Prog
		col = [3]float64{col[0] + (1-col[0])*f, col[1] + (1-col[1])*f, col[2] + (1-col[2])*f}
	}
	xf := func(l V3) V3 { return l.Add(V3{z.Pos.X, 0, z.Pos.Z}) }
	dim := [3]float64{col[0] * 0.55, col[1] * 0.55, col[2] * 0.55}
	dst = box(dst, V3{0, 0.05, 0}, V3{z.Half.X, 0.05, z.Half.Z}, dim, xf)               // floor pad
	dst = box(dst, V3{0, 0.55, 0}, V3{z.Half.X * 0.18, 0.55, z.Half.Z * 0.18}, col, xf) // center post (visible from afar)
	return dst
}

// ---------------------------------------------------------------------------
// world rendering
// ---------------------------------------------------------------------------

func (r *Renderer) putPx(x, y int, col [3]byte) {
	if x < 0 || y < 0 || x >= r.W || y >= r.H {
		return
	}
	o := (y*r.W + x) * 3
	r.fb[o], r.fb[o+1], r.fb[o+2] = col[0], col[1], col[2]
}

// tintRed blends the framebuffer toward red by alpha (capped) for the damage flash.
func (r *Renderer) tintRed(a float64) {
	if a <= 0 {
		return
	}
	if a > 0.6 {
		a = 0.6
	}
	const rr, rg, rb = 210.0, 18.0, 18.0
	for i := 0; i < len(r.fb); i += 3 {
		r.fb[i] = clampB(float64(r.fb[i]) + (rr-float64(r.fb[i]))*a)
		r.fb[i+1] = clampB(float64(r.fb[i+1]) + (rg-float64(r.fb[i+1]))*a)
		r.fb[i+2] = clampB(float64(r.fb[i+2]) + (rb-float64(r.fb[i+2]))*a)
	}
}

// drawReticle paints the center crosshair, which carries two signals at once:
//   - color = reload state: the 4 cardinal arms are drawn in `col` (red=reloading,
//     green=ready, amber=rapid), so the crosshair is the reload gauge.
//   - turn/twist gauge: how far (and which way) the turret is rotated off the hull,
//     snapped to 8 points and sweeping WITH your aim — turn right and the lit
//     element rotates clockwise; to re-align, turn back the other way (or tap C to
//     recenter). Centered turret = lit straight up. A cardinal lights that arm; a
//     diagonal adds a 5th "/" or "\" tick.
//
// `turret` is the turret offset relative to the hull. The screen mapping mirrors
// drawRadar's axes; +turret sweeps clockwise.
func (r *Renderer) drawReticle(col [3]byte, turret float64) {
	cx, cy := r.W/2, r.H/2
	body := [3]byte{230, 240, 255} // fixed twist-gauge color (distinct from col)

	// Snap the turret offset to one of 8 screen points; +turret sweeps clockwise
	// (aim right -> marker right) so you turn the opposite way to re-center.
	ux, uy := math.Sin(turret), -math.Cos(turret)
	k := int(math.Round(math.Atan2(ux, -uy) / (math.Pi / 4)))
	k = ((k % 8) + 8) % 8 // 0=up,1=NE,2=E,3=SE,4=S,5=SW,6=W,7=NW

	// 4 cardinal arms; the one matching the twist direction (even k) lights up.
	cards := [4][2]int{{0, -1}, {1, 0}, {0, 1}, {-1, 0}} // up, right, down, left
	for i, dir := range cards {
		c := col
		if k == i*2 {
			c = body
		}
		for d := 2; d <= 5; d++ {
			r.putPx(cx+dir[0]*d, cy+dir[1]*d, c)
		}
	}
	// Body on a diagonal: add the 5th element, a short diagonal tick.
	if k%2 == 1 {
		var dx, dy int
		switch k {
		case 1:
			dx, dy = 1, -1 // NE
		case 3:
			dx, dy = 1, 1 // SE
		case 5:
			dx, dy = -1, 1 // SW
		case 7:
			dx, dy = -1, -1 // NW
		}
		for d := 2; d <= 5; d++ {
			px := cx + int(math.Round(0.7071*float64(dx*d)))
			py := cy + int(math.Round(0.7071*float64(dy*d)))
			r.putPx(px, py, body)
		}
	}
}

// renderWorld draws the arena, every tank except our own (me) and the dead, all
// projectiles, then the damage flash and aiming reticle.
func (r *Renderer) renderWorld(cam Cam, t float64, tanks []gm.TankSnap, shots []gm.V3, flags []gm.FlagSnap, pickups []gm.PickupSnap, entT []gm.Entity, entS []gm.EntitySnap, zones []gm.ZoneSnap, me int, flash float64, topdown bool, reticle [3]byte, selfTurret float64) {
	r.clear()
	r.drawTris(cam, arena, t)
	var dyn []Tri
	for i := range zones {
		dyn = appendZone(dyn, zones[i])
	}
	for i := range tanks {
		tk := &tanks[i]
		if tk.Dead || (!topdown && tk.ID == me) { // skip the dead; in 1st person, our own hull
			continue
		}
		if tk.Cloak && tk.ID != me { // cloaked enemies are invisible (you still see your own)
			continue
		}
		dyn = appendTank(dyn, tk)
	}
	for _, s := range shots {
		dyn = appendShot(dyn, s)
	}
	for _, f := range flags {
		dyn = appendFlag(dyn, f)
	}
	for _, p := range pickups {
		dyn = appendPickup(dyn, p)
	}
	for i := range entT {
		var st gm.EntitySnap
		if i < len(entS) {
			st = entS[i]
		}
		dyn = appendEntity(dyn, entT[i], st)
	}
	r.drawTris(cam, dyn, 0)
	if flash > 0 {
		r.tintRed(flash / hitFlashTime * 0.6)
	}
	if topdown {
		return // overhead view is its own map; no radar/reticle
	}
	r.drawRadar(cam, tanks, flags, pickups, entT, entS, me)
	r.drawReticle(reticle, selfTurret)
}

// radarRect returns the radar's cell bounds (c0..c1 columns, r0..r1 rows) in the
// top-right corner. Shared by the blip painter (framebuffer) and the corner
// glyphs (text overlay) so they line up. cols == r.W (one cell per pixel column).
func radarRect(cols int) (c0, c1, r0, r1 int) {
	const rw, rh = 18, 8
	c1 = cols - 2
	c0 = c1 - rw + 1
	if c0 < 1 {
		c0 = 1
	}
	r0, r1 = 1, rh
	return
}

var radarRange = 42.0 // world units shown from center to edge (set per-map in buildArena)

// drawRadar paints the radar contents into the framebuffer's top-right: just the
// player at center (facing up) and a blip per other live tank, floating over the
// empty sky (no disc/box). The corner brackets are drawn separately as text.
func (r *Renderer) drawRadar(cam Cam, tanks []gm.TankSnap, flags []gm.FlagSnap, pickups []gm.PickupSnap, entT []gm.Entity, entS []gm.EntitySnap, me int) {
	c0, c1, r0, r1 := radarRect(r.W)
	pcx := (c0 + c1) / 2
	pcy := ((r0-1)*2 + r1*2 - 1) / 2
	scale := float64(r1 - r0 + 1) // px from center to edge (square-ish)
	sn, cs := math.Sin(cam.yaw), math.Cos(cam.yaw)
	// rel maps a world point to a radar pixel relative to the player's facing.
	blip := func(wx, wz float64, col [3]byte) {
		rx, rz := wx-cam.pos.X, wz-cam.pos.Z
		if rx*rx+rz*rz > radarRange*radarRange {
			return
		}
		bx := pcx + int((rx*cs-rz*sn)/radarRange*scale)
		by := pcy - int((rx*sn+rz*cs)/radarRange*scale)
		r.putPx(bx, by, col)
	}

	// flags first (so tank blips draw over them)
	for _, f := range flags {
		col := [3]byte{240, 220, 60} // neutral yellow
		switch f.Team {
		case 0:
			col = [3]byte{240, 90, 80}
		case 1:
			col = [3]byte{90, 130, 240}
		}
		blip(f.Pos.X, f.Pos.Z, col)
	}
	// power-up drops, colored by kind
	for _, p := range pickups {
		c := pickupColor(p.Kind)
		blip(p.Pos.X, p.Pos.Z, [3]byte{clampB(c[0] * 255), clampB(c[1] * 255), clampB(c[2] * 255)})
	}
	// live turret emplacements show as orange threat blips
	for i := range entT {
		if entT[i].Turret == nil {
			continue
		}
		if i < len(entS) && entS[i].Dead {
			continue
		}
		blip(entT[i].Pos.X, entT[i].Pos.Z, [3]byte{245, 150, 40})
	}
	// player marker + short heading tick pointing up
	r.putPx(pcx, pcy, [3]byte{130, 205, 255})
	r.putPx(pcx, pcy-1, [3]byte{95, 165, 220})
	for i := range tanks {
		t := &tanks[i]
		if t.ID == me || t.Dead || t.Cloak {
			continue // cloaked enemies don't show on radar
		}
		rx := t.Pos.X - cam.pos.X
		rz := t.Pos.Z - cam.pos.Z
		if rx*rx+rz*rz > radarRange*radarRange {
			continue
		}
		side := rx*cs - rz*sn // +right
		fwd := rx*sn + rz*cs  // +forward (up)
		bx := pcx + int(side/radarRange*scale)
		by := pcy - int(fwd/radarRange*scale)
		col := [3]byte{clampB(t.Color[0] * 255), clampB(t.Color[1] * 255), clampB(t.Color[2] * 255)}
		r.putPx(bx, by, col)
		r.putPx(bx+1, by, col) // 2px wide so blips read clearly
	}
}
