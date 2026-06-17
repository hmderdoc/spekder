package main

import (
	"math"

	gm "spekder/internal/game"
)

// Creature render models (bestiary). The sim treats every actor as a tank; these
// are silhouette-only swaps keyed on TankSnap.Body, used by Survival's waves. Each
// builds from boxes via the hull transform (so it faces the actor's heading) and
// animates limbs from the render clock so they look alive.

// tint scales a color toward black (for darker limbs/underside).
func tint(c [3]float64, f float64) [3]float64 { return [3]float64{c[0] * f, c[1] * f, c[2] * f} }

// rgb2hsv / hsv2rgb convert between color spaces for deriving the accent.
func rgb2hsv(c [3]float64) (h, s, v float64) {
	r, g, b := c[0], c[1], c[2]
	mx := math.Max(r, math.Max(g, b))
	mn := math.Min(r, math.Min(g, b))
	v = mx
	d := mx - mn
	if mx > 0 {
		s = d / mx
	}
	if d == 0 {
		return 0, s, v
	}
	switch mx {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	if h /= 6; h < 0 {
		h++
	}
	return
}

func hsv2rgb(h, s, v float64) [3]float64 {
	i := math.Floor(h * 6)
	f := h*6 - i
	p, q, t := v*(1-s), v*(1-f*s), v*(1-(1-f)*s)
	switch int(i) % 6 {
	case 0:
		return [3]float64{v, t, p}
	case 1:
		return [3]float64{q, v, p}
	case 2:
		return [3]float64{p, v, t}
	case 3:
		return [3]float64{p, q, v}
	case 4:
		return [3]float64{t, p, v}
	default:
		return [3]float64{v, p, q}
	}
}

// accentColor derives a complementary-yet-vivid secondary color from a model's
// primary (hue rotated ~180deg, forced saturated + bright) - used for eyes, accents,
// and the scoreboard swatch. Auto-generated so every color picks a distinct accent.
func accentColor(c [3]float64) [3]float64 {
	h, s, _ := rgb2hsv(c)
	h = math.Mod(h+0.5, 1)
	if s < 0.85 { // keep an accent even off a desaturated/gray primary
		s = 0.85
	}
	return hsv2rgb(h, s, 1.0)
}

// curAccent is the accent color of the model currently being built (set per-tank in
// appendTank; render is single-threaded, so a package var is safe and avoids
// threading an extra arg through every creature builder).
var curAccent = [3]float64{1, 1, 0.2}

// curAim is the current model's turret yaw (hull-relative) and curAir whether
// it's off the ground - same single-threaded package-var idiom as curAccent.
// Heads (and the scorpion's tail) rotate by curAim so creatures visibly track
// their aim the way a tank turret does; the gorilla switches to its pounce
// pose while curAir.
var (
	curAim        float64
	curAir        bool
	curShell      bool    // turtle tucked into its shell (limbs/head retracted)
	curShieldUp   bool    // minotaur barrier deployed (draw the frontal field)
	curShieldFrac float64 // minotaur barrier charge 0..1 (the field fades as it drains)
)

// builtObstacles/builtRamps are the active map's static geometry, cached by
// buildArena so the render side can ask "is this tank off the ground?".
var (
	builtObstacles []gm.Box
	builtRamps     []gm.Ramp
)

// aimAt wraps a body transform with a yaw about pivot (the neck), so a head
// cluster tracks the aim. Same rotation sense as the tank turret.
func aimAt(base func(V3) V3, aim float64, pivot V3) func(V3) V3 {
	if aim == 0 {
		return base
	}
	sy, cy := math.Sin(aim), math.Cos(aim)
	return func(l V3) V3 {
		p := V3{l.X - pivot.X, l.Y - pivot.Y, l.Z - pivot.Z}
		return base(V3{p.X*cy + p.Z*sy, p.Y, -p.X*sy + p.Z*cy}.Add(pivot))
	}
}

// eyePair adds two accent-colored eyes (the model's secondary color) symmetric about
// the centerline. Coords are pre-scale; s multiplies them.
func eyePair(dst []Tri, base func(V3) V3, sep, cy, cz, r, s float64) []Tri {
	e := V3{r * s, r * s, r * s}
	dst = box(dst, V3{sep * s, cy * s, cz * s}, e, curAccent, base)
	dst = box(dst, V3{-sep * s, cy * s, cz * s}, e, curAccent, base)
	return dst
}

// limbDown returns a transform for a limb modeled along local -Y (hanging down
// from the hip): swing it fore/aft (rotate about X) and splay it sideways (rotate
// about Z), then anchor at hip and apply the hull transform.
func limbDown(base func(V3) V3, hip V3, splay, fore float64) func(V3) V3 {
	ss, cs := math.Sin(splay), math.Cos(splay)
	sf, cf := math.Sin(fore), math.Cos(fore)
	return func(l V3) V3 {
		p := V3{l.X, l.Y*cf - l.Z*sf, l.Y*sf + l.Z*cf} // fore/aft about X
		p = V3{p.X*cs - p.Y*ss, p.X*ss + p.Y*cs, p.Z}  // splay about Z
		return base(p.Add(hip))
	}
}

// limb appends one tapered limb (a thin box hanging from the hip along -Y).
func limb(dst []Tri, base func(V3) V3, hip V3, splay, fore, length, r float64, col [3]float64) []Tri {
	xf := limbDown(base, hip, splay, fore)
	return box(dst, V3{0, -length / 2, 0}, V3{r, length / 2, r}, col, xf)
}

// fitCam auto-frames a model's tris so it fills the preview pane regardless of the
// model's true size - the preview's job is to show detail, not relative scale. It
// fits the model's bounding BOX (not sphere) to the pane's aspect: the horizontal
// extent (worst case over the spin) to the width, the vertical extent to the
// height, taking whichever binds. A sphere fit wastes space on wide/flat models.
// previewPad shrinks select bodies in the character-select preview (which otherwise
// auto-frames every model to the same fill): the T-Rex reads too large, so show it
// smaller. >1 = pull the camera back (smaller model); 1 = normal.
func previewPad(body int) float64 {
	switch body {
	case gm.BodyTrex:
		return 1.0 / 0.7 // ~70% of current preview size
	}
	return 1.0
}

func fitCam(tris []Tri, w, h int, angle, pad float64) Cam {
	if len(tris) == 0 {
		return Cam{pos: V3{Z: 5}, yaw: math.Pi}
	}
	lo, hi := tris[0].v[0], tris[0].v[0]
	for i := range tris {
		for _, v := range tris[i].v {
			lo = V3{math.Min(lo.X, v.X), math.Min(lo.Y, v.Y), math.Min(lo.Z, v.Z)}
			hi = V3{math.Max(hi.X, v.X), math.Max(hi.Y, v.Y), math.Max(hi.Z, v.Z)}
		}
	}
	center := V3{(lo.X + hi.X) / 2, (lo.Y + hi.Y) / 2, (lo.Z + hi.Z) / 2}
	halfY := (hi.Y - lo.Y) / 2
	radH := 0.001 // max horizontal radius, so no clipping at any spin angle
	for i := range tris {
		for _, v := range tris[i].v {
			if rh := math.Hypot(v.X-center.X, v.Z-center.Z); rh > radH {
				radH = rh
			}
		}
	}
	// focal = w/2 px (90deg horizontal FOV). Distance so the box fills ~fill of the
	// pane: width binds via radH, height via halfY scaled by the pane aspect.
	const fill = 0.76 // ~85% of full so models aren't crowding the pane edges
	dW := radH / fill
	dH := halfY * float64(w) / (fill * float64(h))
	D := math.Max(dW, dH) * pad  // pad > 1 pulls back to render the model smaller
	camY := center.Y + 0.3*halfY // gentle downward look for a 3D feel
	return Cam{
		pos:   V3{X: center.X + D*math.Sin(angle), Y: camY, Z: center.Z + D*math.Cos(angle)},
		yaw:   angle + math.Pi,
		pitch: math.Atan2(camY-center.Y, D), // aim back at the model center
	}
}

// Player-piloted beasts hold a higher minimum size than Survival bots, so a chosen
// character never renders tiny (a key roster fix); the chassis scale no longer sizes
// a beast - its size is normalized from its own geometry to one of these targets
// (max bounding dimension, world units).
const (
	playerBodySize = 2.6
	enemyBodySize  = 2.0
)

var natExtent [gm.BodyKinds]float64 // cached natural max-dimension at scale 1

func appendCreature(dst []Tri, t *gm.TankSnap, base func(V3) V3, col, bright [3]float64, _ float64, clock float64) []Tri {
	target := playerBodySize
	if t.Bot {
		target = enemyBodySize
	}
	target *= gm.BodySizeScale(t.Body) // a few species (T-Rex) tower over the rest
	s := 1.0
	if nat := naturalExtent(t.Body); nat > 0 {
		s = target / nat // normalize to a consistent on-screen size
	}
	curAim = t.TurretYaw
	curAir = t.Pos.Y > gm.GroundBoxes(builtObstacles, builtRamps, t.Pos.X, t.Pos.Z, 1e9)+0.25
	curShell = t.Shell
	curShieldUp, curShieldFrac = t.ShieldUp, t.ShieldFrac
	dst = buildBody(dst, t.Body, base, col, bright, s, clock)
	curAim, curAir, curShell, curShieldUp = 0, false, false, false
	return dst
}

// buildBody emits a creature's geometry at scale s (shared by the renderer and the
// natural-size measurement).
func buildBody(dst []Tri, body int, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	switch body {
	case gm.BodySpider:
		return appendSpider(dst, base, col, bright, s, clock)
	case gm.BodyQuad:
		return appendQuad(dst, base, col, bright, s, clock)
	case gm.BodyInsect:
		return appendInsect(dst, base, col, bright, s, clock)
	case gm.BodyHumanoid:
		return appendHumanoid(dst, base, col, bright, s, clock)
	case gm.BodyScorpion:
		return appendScorpion(dst, base, col, bright, s, clock)
	case gm.BodySerpent:
		return appendSerpent(dst, base, col, bright, s, clock)
	case gm.BodyTripod:
		return appendTripod(dst, base, col, bright, s, clock)
	case gm.BodyDrone:
		return appendDrone(dst, base, col, bright, s, clock)
	case gm.BodyCrab:
		return appendCrab(dst, base, col, bright, s, clock)
	case gm.BodyOctopod:
		return appendOctopod(dst, base, col, bright, s, clock)
	case gm.BodyButterfly:
		return appendButterfly(dst, base, col, bright, s, clock)
	case gm.BodyMantis:
		return appendMantis(dst, base, col, bright, s, clock)
	case gm.BodyTurtle:
		return appendTurtle(dst, base, col, bright, s, clock)
	case gm.BodyTrex:
		return appendTrex(dst, base, col, bright, s, clock)
	case gm.BodyGorilla:
		return appendGorilla(dst, base, col, bright, s, clock)
	case gm.BodyElephant:
		return appendElephant(dst, base, col, bright, s, clock)
	case gm.BodyFalcon:
		return appendFalcon(dst, base, col, bright, s, clock)
	case gm.BodyStag:
		return appendStag(dst, base, col, bright, s, clock)
	case gm.BodyMinotaur:
		return appendMinotaur(dst, base, col, bright, s, clock)
	}
	return dst
}

// appendButterfly: a slender body with two big flapping wing-pairs + antennae - the
// flying healer. (Its hover is the sim's flight mechanic; this is just the model.)
func appendButterfly(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	limbc := tint(col, 0.8)
	hx := aimAt(base, curAim, V3{0, 0.95 * s, 0})                                        // head tracks the aim
	dst = box(dst, V3{0, 0.55 * s, 0}, V3{0.12 * s, 0.4 * s, 0.18 * s}, col, base)       // thorax
	dst = box(dst, V3{0, 0.95 * s, 0.05 * s}, V3{0.1 * s, 0.1 * s, 0.1 * s}, bright, hx) // head
	dst = eyePair(dst, hx, 0.09, 0.95, 0.16, 0.034, s)                                   // eyes: wider apart, smaller so they don't bleed
	for _, side := range []float64{1, -1} {                                              // antennae rise up and OUT to club tips
		hip := V3{side * 0.09 * s, 1.0 * s, 0.05 * s}
		dst = limb(dst, hx, hip, side*0.6, -2.6, 0.28*s, 0.018*s, limbc)
		axf := limbDown(hx, hip, side*0.6, -2.6)
		dst = box(dst, V3{0, -0.28 * s, 0}, V3{0.03 * s, 0.03 * s, 0.03 * s}, limbc, axf) // club tip (same color as the antenna)
	}
	// Wings spread FLAT to the sides in the plane that faces the viewer (broad in X
	// and Y, thin in Z) - the classic butterfly silhouette: a large upper forewing
	// and a smaller lower hindwing per side. They flutter by tipping gently about the
	// vertical axis (toward/away from the camera), hinged at the body.
	flap := 0.28 * math.Sin(clock*8)
	wingXF := func(roll float64) func(V3) V3 { // rotate about Y: outer edge tips toward/away
		sr, cr := math.Sin(roll), math.Cos(roll)
		return func(l V3) V3 { return base(V3{l.X*cr - l.Z*sr, l.Y, l.X*sr + l.Z*cr}) }
	}
	for _, side := range []float64{1, -1} {
		xf := wingXF(side * flap)
		dst = box(dst, V3{side * 0.42 * s, 0.78 * s, 0}, V3{0.36 * s, 0.3 * s, 0.025 * s}, col, xf)     // upper forewing (large)
		dst = box(dst, V3{side * 0.64 * s, 0.92 * s, 0}, V3{0.16 * s, 0.18 * s, 0.025 * s}, bright, xf) // rounded outer corner
		dst = box(dst, V3{side * 0.34 * s, 0.34 * s, 0}, V3{0.26 * s, 0.22 * s, 0.025 * s}, bright, xf) // lower hindwing
		dst = box(dst, V3{side * 0.5 * s, 0.22 * s, 0}, V3{0.12 * s, 0.13 * s, 0.025 * s}, col, xf)     // rounded lower corner
	}
	return dst
}

// appendMantis: upright thorax + abdomen, big raptorial forearms held forward, head
// with eyes, four thin walking legs.
func appendMantis(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.6)
	hx := aimAt(base, curAim, V3{0, 1.4 * s, 0})                                         // head tracks the aim
	dst = box(dst, V3{0, 0.9 * s, 0}, V3{0.16 * s, 0.45 * s, 0.2 * s}, col, base)        // upright thorax
	dst = box(dst, V3{0, 0.55 * s, -0.3 * s}, V3{0.18 * s, 0.3 * s, 0.4 * s}, col, base) // abdomen
	dst = box(dst, V3{0, 1.4 * s, 0.05 * s}, V3{0.14 * s, 0.12 * s, 0.14 * s}, bright, hx)
	dst = box(dst, V3{0.08 * s, 1.45 * s, 0.16 * s}, V3{0.04 * s, 0.04 * s, 0.04 * s}, curAccent, hx)
	dst = box(dst, V3{-0.08 * s, 1.45 * s, 0.16 * s}, V3{0.04 * s, 0.04 * s, 0.04 * s}, curAccent, hx)
	strike := 0.2 * math.Sin(clock*3)
	for _, side := range []float64{1, -1} {
		dst = limb(dst, base, V3{side * 0.16 * s, 1.15 * s, 0.1 * s}, side*0.5, -1.0+strike, 0.4*s, 0.05*s, col)
		dst = box(dst, V3{side * 0.3 * s, 1.0 * s, 0.5 * s}, V3{0.05 * s, 0.05 * s, 0.28 * s}, bright, base) // folded forearm
	}
	zPos := []float64{0.1, -0.2}
	for k := 0; k < 2; k++ {
		for _, side := range []float64{1, -1} {
			swing := 0.25 * math.Sin(clock*6+float64(k)*0.8+math.Pi*(0.5-0.5*side))
			dst = limb(dst, base, V3{side * 0.16 * s, 0.7 * s, zPos[k] * s}, side*0.9, swing, 0.7*s, 0.035*s, legc)
		}
	}
	return dst
}

// appendTurtle: a domed shell on four stubby legs, head + tail, low and armored.
// Shelled up (curShell), only the armor shows: head, legs, and tail retract.
func appendTurtle(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	// carapace: a low wide rim + a tall center scute ringed by four lower side
	// scutes, so it reads as a rounded-yet-faceted turtle shell, not two boxes.
	shell := func(dst []Tri, lift float64) []Tri {
		dst = box(dst, V3{0, (0.34 + lift) * s, 0}, V3{0.56 * s, 0.18 * s, 0.58 * s}, col, base)             // rim
		dst = box(dst, V3{0, (0.54 + lift) * s, 0}, V3{0.24 * s, 0.14 * s, 0.26 * s}, tint(col, 0.92), base) // center scute (tallest)
		for _, p := range [][3]float64{{0, 0.46, 0.32}, {0, 0.46, -0.32}, {0.34, 0.44, 0}, {-0.34, 0.44, 0}} {
			dst = box(dst, V3{p[0] * s, (p[1] + lift) * s, p[2] * s}, V3{0.15 * s, 0.12 * s, 0.16 * s}, tint(col, 0.82), base) // side scutes, a touch lower
		}
		return dst
	}
	if curShell { // tucked in: just the hunkered shell, head/neck/legs retracted
		return shell(dst, -0.06)
	}
	legc := tint(col, 0.6)
	dst = shell(dst, 0)
	// a thin neck pokes forward out of the shell's front opening with the head on
	// its end (both track the aim); the carapace is short enough fore/aft that the
	// neck and tail clearly emerge rather than sprouting from a solid box.
	hx := aimAt(base, curAim, V3{0, 0.4 * s, 0.4 * s})
	dst = box(dst, V3{0, 0.38 * s, 0.58 * s}, V3{0.07 * s, 0.07 * s, 0.16 * s}, legc, hx)      // neck (thin, separate)
	dst = box(dst, V3{0, 0.4 * s, 0.78 * s}, V3{0.15 * s, 0.14 * s, 0.16 * s}, bright, hx)     // head
	dst = eyePair(dst, hx, 0.08, 0.45, 1.0, 0.045, s)                                          // accent eyes
	dst = box(dst, V3{0, 0.34 * s, -0.66 * s}, V3{0.045 * s, 0.045 * s, 0.18 * s}, legc, base) // tail, emerging from the rear
	for _, zf := range []float64{0.36, -0.36} {
		for _, side := range []float64{1, -1} {
			swing := 0.18 * math.Sin(clock*5+zf+side)
			dst = limb(dst, base, V3{side * 0.5 * s, 0.32 * s, zf * s}, side*0.5, swing, 0.3*s, 0.08*s, legc)
		}
	}
	return dst
}

// appendTrex: a big biped - thick legs, heavy tail, large jawed head, tiny arms.
func appendTrex(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	limbc := tint(col, 0.7)
	dst = box(dst, V3{0, 1.1 * s, 0}, V3{0.3 * s, 0.45 * s, 0.4 * s}, col, base)                                  // torso
	dst = box(dst, V3{0, 1.0 * s, -0.6 * s}, V3{0.12 * s, 0.12 * s, 0.5 * s}, col, base)                          // tail base
	dst = box(dst, V3{0, 0.9 * s, -1.0 * s}, V3{0.07 * s, 0.07 * s, 0.35 * s}, limbc, base)                       // tail tip
	hx := aimAt(base, curAim, V3{0, 1.7 * s, 0.1 * s})                                                            // the great head swings to the aim
	dst = box(dst, V3{0, 1.7 * s, 0.35 * s}, V3{0.26 * s, 0.26 * s, 0.4 * s}, bright, hx)                         // head
	dst = box(dst, V3{0, 1.55 * s, 0.62 * s}, V3{0.22 * s, 0.1 * s, 0.2 * s}, tint(col, 0.9), hx)                 // jaw
	dst = box(dst, V3{0, 1.5 * s, 0.79 * s}, V3{0.17 * s, 0.025 * s, 0.04 * s}, [3]float64{0.95, 0.93, 0.85}, hx) // upper teeth
	dst = box(dst, V3{0, 1.47 * s, 0.76 * s}, V3{0.15 * s, 0.02 * s, 0.04 * s}, [3]float64{0.92, 0.9, 0.82}, hx)  // lower teeth
	dst = eyePair(dst, hx, 0.14, 1.8, 0.72, 0.06, s)                                                              // eyes set into the front of the head
	for _, side := range []float64{1, -1} {                                                                       // the famous tiny arms: held out and forward, clawed
		dst = box(dst, V3{side * 0.27 * s, 1.05 * s, 0.42 * s}, V3{0.04 * s, 0.04 * s, 0.15 * s}, limbc, base) // forearm reaching forward
		for _, fx := range []float64{0.03, -0.03} {                                                            // two little claw fingers at the tip
			dst = box(dst, V3{(side*0.27 + fx) * s, 1.0 * s, 0.58 * s}, V3{0.014 * s, 0.014 * s, 0.06 * s}, limbc, base)
		}
	}
	swing := 0.4 * math.Sin(clock*4)
	dst = limb(dst, base, V3{0.18 * s, 0.72 * s, 0}, 0.1, swing, 0.78*s, 0.12*s, limbc)
	dst = limb(dst, base, V3{-0.18 * s, 0.72 * s, 0}, -0.1, -swing, 0.78*s, 0.12*s, limbc)
	return dst
}

// appendGorilla: a true knuckle-walker. A distinct head on a neck (proud of the
// shoulders, turning with the aim), heavy shoulder hump over a forward-leaning
// chest, long arms planted on knuckle fists ahead of the body, short bent rear
// legs with feet. Grounded, the four limbs swing in a knuckle-walk gait; in the
// air (the leap) the arms sweep up for the pounce and the legs tuck under -
// landing next to someone hurts (see leapLand).
func appendGorilla(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	limbc := tint(col, 0.7)
	dst = box(dst, V3{0, 1.18 * s, -0.1 * s}, V3{0.42 * s, 0.26 * s, 0.3 * s}, col, base)             // shoulder hump
	dst = box(dst, V3{0, 0.84 * s, 0.1 * s}, V3{0.34 * s, 0.28 * s, 0.26 * s}, col, base)             // chest, leaning in
	dst = box(dst, V3{0, 0.52 * s, -0.18 * s}, V3{0.24 * s, 0.2 * s, 0.2 * s}, tint(col, 0.85), base) // hips, low and back
	// the head is its own block on a neck pivot, clearly separate from the hump
	hx := aimAt(base, curAim, V3{0, 1.34 * s, 0.14 * s})
	dst = box(dst, V3{0, 1.46 * s, 0.22 * s}, V3{0.16 * s, 0.15 * s, 0.17 * s}, bright, hx)          // skull
	dst = box(dst, V3{0, 1.56 * s, 0.31 * s}, V3{0.15 * s, 0.045 * s, 0.06 * s}, tint(col, 0.6), hx) // heavy brow
	dst = box(dst, V3{0, 1.37 * s, 0.36 * s}, V3{0.11 * s, 0.075 * s, 0.1 * s}, tint(col, 0.85), hx) // muzzle/jaw
	dst = eyePair(dst, hx, 0.07, 1.5, 0.39, 0.05, s)
	const armLen, legLen = 1.04, 0.48
	for _, side := range []float64{1, -1} {
		gait := math.Sin(clock*5 + math.Pi*(0.5-0.5*side)) // limbs alternate L/R
		// negative fore reaches the limb down-FORWARD (the knuckle-walk stance);
		// the fist/foot then ride the limb's own transform so they stay attached.
		armFore := -0.5 + 0.2*gait
		legFore := -0.08 - 0.2*gait
		if curAir { // pounce: arms sweep up and out, legs tuck under the body
			armFore, legFore = -1.5, 1.25
		}
		armHip := V3{side * 0.4 * s, 1.16 * s, 0.1 * s}
		legHip := V3{side * 0.18 * s, 0.5 * s, -0.18 * s}
		dst = limb(dst, base, armHip, side*0.2, armFore, armLen*s, 0.11*s, limbc)
		dst = limb(dst, base, legHip, side*0.14, legFore, legLen*s, 0.13*s, limbc)
		if !curAir {
			// knuckle fist at the arm's tip, flat foot at the leg's tip (same
			// limb transform, so they move with the limb instead of floating).
			armXF := limbDown(base, armHip, side*0.2, armFore)
			dst = box(dst, V3{0, -armLen * s, 0.04 * s}, V3{0.14 * s, 0.11 * s, 0.16 * s}, limbc, armXF)
			legXF := limbDown(base, legHip, side*0.14, legFore)
			dst = box(dst, V3{0, -legLen * s, 0.06 * s}, V3{0.1 * s, 0.07 * s, 0.16 * s}, limbc, legXF)
		}
	}
	return dst
}

// naturalExtent is a body's max bounding dimension at scale 1 (cached), used to
// normalize every creature to a consistent rendered size regardless of its shape.
func naturalExtent(body int) float64 {
	if body < 0 || body >= gm.BodyKinds {
		return 1
	}
	if natExtent[body] > 0 {
		return natExtent[body]
	}
	white := [3]float64{1, 1, 1}
	savedAim, savedAir, savedShield := curAim, curAir, curShieldUp
	curAim, curAir, curShieldUp = 0, false, false // measure the neutral pose (no barrier)
	tris := buildBody(nil, body, func(l V3) V3 { return l }, white, white, 1, 0)
	curAim, curAir, curShieldUp = savedAim, savedAir, savedShield
	if len(tris) == 0 {
		natExtent[body] = 1
		return 1
	}
	lo, hi := tris[0].v[0], tris[0].v[0]
	for i := range tris {
		for _, v := range tris[i].v {
			lo = V3{math.Min(lo.X, v.X), math.Min(lo.Y, v.Y), math.Min(lo.Z, v.Z)}
			hi = V3{math.Max(hi.X, v.X), math.Max(hi.Y, v.Y), math.Max(hi.Z, v.Z)}
		}
	}
	ext := math.Max(hi.X-lo.X, math.Max(hi.Y-lo.Y, hi.Z-lo.Z))
	if ext <= 0 {
		ext = 1
	}
	natExtent[body] = ext
	return ext
}

// appendCrab: wide low shell, eyestalks, two big side pincers that open and close,
// and six scuttling side legs.
func appendCrab(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.6)
	hx := aimAt(base, curAim, V3{0, 0.5 * s, 0.05 * s})                           // eyestalks track the aim
	dst = box(dst, V3{0, 0.38 * s, 0}, V3{0.5 * s, 0.2 * s, 0.34 * s}, col, base) // wide shell
	for _, side := range []float64{1, -1} {                                       // eyestalks
		dst = box(dst, V3{side * 0.14 * s, 0.6 * s, 0.28 * s}, V3{0.035 * s, 0.12 * s, 0.035 * s}, legc, hx)
		dst = box(dst, V3{side * 0.14 * s, 0.72 * s, 0.28 * s}, V3{0.06 * s, 0.06 * s, 0.06 * s}, curAccent, hx) // eye
	}
	cl := 0.1 * math.Sin(clock*4) // pincers open/close
	for _, side := range []float64{1, -1} {
		// arms angle up and out; the pincers ride wider and higher than the
		// shell so they still read when the crab faces away (demo camera)
		dst = box(dst, V3{side * 0.56 * s, 0.46 * s, 0.3 * s}, V3{0.09 * s, 0.07 * s, 0.24 * s}, legc, base) // arm
		dst = box(dst, V3{side * 0.68 * s, (0.62 + cl) * s, 0.56 * s}, V3{0.2 * s, 0.08 * s, 0.17 * s}, bright, base)
		dst = box(dst, V3{side * 0.68 * s, (0.46 - cl) * s, 0.56 * s}, V3{0.2 * s, 0.08 * s, 0.17 * s}, bright, base)
	}
	zPos := []float64{0.18, -0.02, -0.22}
	for k := 0; k < 3; k++ {
		for _, side := range []float64{1, -1} {
			phase := float64(k)*0.8 + math.Pi*(0.5-0.5*side)
			swing := 0.3 * math.Sin(clock*7+phase)
			dst = limb(dst, base, V3{side * 0.46 * s, 0.36 * s, zPos[k] * s}, side*1.2, swing, 0.42*s, 0.04*s, legc)
		}
	}
	return dst
}

// appendOctopod: a bulbous mantle with big eyes and eight tentacles that splay
// radially and writhe (each on its own phase).
func appendOctopod(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	limbc := tint(col, 0.7)
	// bulbous mantle: a graduated stack, widest at the middle and tapering top and
	// bottom, so it reads as a round head instead of two rigid stacked boxes
	for _, L := range [][3]float64{{0.55, 0.30, 0.10}, {0.70, 0.36, 0.12}, {0.84, 0.34, 0.12}, {0.96, 0.26, 0.10}, {1.05, 0.16, 0.08}} {
		dst = box(dst, V3{0, L[0] * s, 0}, V3{L[1] * s, L[2] * s, L[1] * s}, col, base)
	}
	dst = eyePair(dst, aimAt(base, curAim, V3{0, 0.78 * s, 0}), 0.13, 0.78, 0.38, 0.07, s) // big accent eyes track the aim
	for i := 0; i < 8; i++ {                                                               // tentacles: hip on a ring, lean radially out, writhe
		a := float64(i) / 8 * 2 * math.Pi
		sa, ca := math.Sin(a), math.Cos(a)
		hip := V3{0.26 * s * sa, 0.56 * s, 0.26 * s * ca}
		writhe := 0.3 * math.Sin(clock*3+float64(i)*0.8)
		dst = limb(dst, base, hip, 0.6*sa, -0.6*ca+writhe, 0.55*s, 0.04*s, limbc)
	}
	return dst
}

// appendScorpion: low segmented body, six legs, forward claws, and a tail that
// arches up and over the back to a stinger (with a slow sway).
func appendScorpion(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.55)
	dst = box(dst, V3{0, 0.34 * s, 0.1 * s}, V3{0.32 * s, 0.18 * s, 0.42 * s}, col, base) // body
	dst = box(dst, V3{0, 0.36 * s, -0.32 * s}, V3{0.28 * s, 0.2 * s, 0.3 * s}, col, base) // rear
	dst = eyePair(dst, base, 0.1, 0.49, 0.46, 0.028, s)                                   // tiny eyes on the front carapace
	zPos := []float64{0.22, 0.0, -0.22}
	for k := 0; k < 3; k++ {
		for _, side := range []float64{1, -1} {
			phase := float64(k)*0.7 + math.Pi*(0.5-0.5*side)
			swing := 0.25 * math.Sin(clock*6+phase)
			dst = limb(dst, base, V3{side * 0.3 * s, 0.34 * s, zPos[k] * s}, side*1.05, swing, 0.5*s, 0.045*s, legc)
		}
	}
	for _, side := range []float64{1, -1} { // forward claw arms + pincers
		dst = box(dst, V3{side * 0.26 * s, 0.3 * s, 0.5 * s}, V3{0.05 * s, 0.05 * s, 0.22 * s}, legc, base)
		dst = box(dst, V3{side * 0.26 * s, 0.3 * s, 0.74 * s}, V3{0.11 * s, 0.09 * s, 0.12 * s}, bright, base)
	}
	// tail: segments arching back, up, then forward over the body; sway grows
	// along it. The whole tail rotates with the aim - it IS the weapon.
	tx := aimAt(base, curAim, V3{0, 0.4 * s, -0.1 * s})
	sway := 0.12 * math.Sin(clock*3)
	seg := []V3{{0, 0.5, -0.5}, {0, 0.74, -0.62}, {0, 0.98, -0.56}, {0, 1.16, -0.32}, {0, 1.24, -0.02}}
	for i, p := range seg {
		r := (0.13 - float64(i)*0.013) * s
		dst = box(dst, V3{p.X*s + sway*float64(i+1)*s, p.Y * s, p.Z * s}, V3{r, r, 0.15 * s}, col, tx)
	}
	dst = box(dst, V3{sway * 6 * s, 1.28 * s, 0.16 * s}, V3{0.05 * s, 0.1 * s, 0.09 * s}, bright, tx) // stinger
	return dst
}

// appendSerpent: a cobra. The tail slithers low behind; the front third rises
// in a curve to a raised head with a wide hood flared behind it, the whole rise
// tracking the aim (the venom comes from the mouth). A tongue flicks.
func appendSerpent(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	for i := 0; i < 6; i++ { // tail: low segments, slither grows toward the tip
		z := (-0.1 - float64(i)*0.18) * s
		x := 0.24 * s * math.Sin(clock*4-float64(i)*0.7) * (0.35 + float64(i)*0.13)
		r := (0.16 - float64(i)*0.013) * s
		dst = box(dst, V3{x, 0.12*s + r, z}, V3{r, r, 0.16 * s}, col, base)
	}
	hx := aimAt(base, curAim, V3{0, 0.3 * s, 0.1 * s}) // the rise swings with the aim
	sway := 0.05 * math.Sin(clock*2.5)
	neck := []V3{{0, 0.2, 0.1}, {0, 0.4, 0.22}, {0, 0.6, 0.3}, {0, 0.8, 0.34}}
	for i, p := range neck { // the climbing S-curve of the raised front
		r := (0.15 - float64(i)*0.012) * s
		dst = box(dst, V3{p.X*s + sway*float64(i)*s, p.Y * s, p.Z * s}, V3{r, r, 0.13 * s}, col, hx)
	}
	dst = box(dst, V3{sway * 3 * s, 0.88 * s, 0.26 * s}, V3{0.3 * s, 0.22 * s, 0.05 * s}, tint(col, 0.85), hx) // flared hood
	dst = box(dst, V3{sway * 4 * s, 0.95 * s, 0.42 * s}, V3{0.13 * s, 0.09 * s, 0.16 * s}, bright, hx)         // head
	dst = eyePair(dst, hx, 0.07, 0.99, 0.52, 0.035, s)
	for _, fx := range []float64{0.04, -0.04} { // a pair of fangs jut down from the mouth
		dst = box(dst, V3{sway*4*s + fx*s, 0.85 * s, 0.55 * s}, V3{0.012 * s, 0.05 * s, 0.015 * s}, [3]float64{0.95, 0.93, 0.85}, hx)
	}
	if math.Sin(clock*6) > 0.55 { // flicking tongue
		dst = box(dst, V3{sway * 4 * s, 0.93 * s, 0.64 * s}, V3{0.015 * s, 0.015 * s, 0.07 * s}, curAccent, hx)
	}
	return dst
}

// appendTripod: a tall central pod on three long legs (one fore, two aft) with a
// striding gait - an alien walker.
func appendTripod(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.6)
	dst = box(dst, V3{0, 1.3 * s, 0}, V3{0.32 * s, 0.3 * s, 0.34 * s}, col, base)             // pod
	dst = box(dst, V3{0, 1.55 * s, 0.12 * s}, V3{0.16 * s, 0.13 * s, 0.16 * s}, bright, base) // eye cluster
	legs := [][3]float64{{0, 0.5, 0}, {0.9, -0.4, 2.0}, {-0.9, -0.4, 4.0}}                    // splay, fore, phase
	for _, l := range legs {
		swing := 0.18 * math.Sin(clock*4+l[2])
		dst = limb(dst, base, V3{0, 1.25 * s, 0}, l[0], l[1]+swing, 1.3*s, 0.06*s, legc)
	}
	return dst
}

// appendDrone: a hovering orb that bobs up and down, single front eye, four fins.
func appendDrone(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	finc := tint(col, 0.6)
	cy := 0.95*s + 0.12*s*math.Sin(clock*2.5)
	dst = box(dst, V3{0, cy, 0}, V3{0.3 * s, 0.26 * s, 0.3 * s}, col, base)          // orb (two boxes
	dst = box(dst, V3{0, cy, 0}, V3{0.24 * s, 0.32 * s, 0.24 * s}, col, base)        // crossed to round it)
	dst = box(dst, V3{0, cy, 0.3 * s}, V3{0.1 * s, 0.1 * s, 0.06 * s}, bright, base) // eye
	for _, a := range []float64{0, math.Pi / 2, math.Pi, 3 * math.Pi / 2} {
		dst = box(dst, V3{0.34 * s * math.Sin(a), cy - 0.02*s, 0.34 * s * math.Cos(a)}, V3{0.06 * s, 0.03 * s, 0.06 * s}, finc, base)
	}
	return dst
}

// appendSpider: low cephalothorax + bulbous abdomen, eight splayed legs in a
// tripod-ish gait, two bright eyes.
func appendSpider(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.55)
	dst = box(dst, V3{0, 0.42 * s, 0.18 * s}, V3{0.34 * s, 0.20 * s, 0.34 * s}, col, base)         // cephalothorax
	dst = box(dst, V3{0, 0.46 * s, -0.34 * s}, V3{0.46 * s, 0.30 * s, 0.5 * s}, col, base)         // abdomen
	dst = eyePair(dst, aimAt(base, curAim, V3{0, 0.42 * s, 0.18 * s}), 0.12, 0.52, 0.57, 0.055, s) // eyes track the aim
	zPos := []float64{0.30, 0.12, -0.06, -0.26}
	fore := []float64{0.55, 0.20, -0.20, -0.55}
	for k := 0; k < 4; k++ {
		for _, side := range []float64{1, -1} {
			phase := float64(k)*0.7 + math.Pi*(0.5-0.5*side) // legs on a side alternate vs the other
			swing := 0.28 * math.Sin(clock*6+phase)
			hip := V3{side * 0.34 * s, 0.42 * s, zPos[k] * s}
			dst = limb(dst, base, hip, side*1.05, fore[k]+swing, 0.62*s, 0.05*s, legc)
		}
	}
	return dst
}

// appendQuad: a horizontal body on four legs - ears, snout, and a swaying tail
// so it reads as an animal, with a bob in the trot so it moves like one.
func appendQuad(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.6)
	bob := 0.03 * s * math.Sin(clock*10)                                                // body rides the trot
	hx := aimAt(base, curAim, V3{0, 0.7*s + bob, 0.5 * s})                              // head tracks the aim
	dst = box(dst, V3{0, 0.62*s + bob, 0}, V3{0.34 * s, 0.26 * s, 0.62 * s}, col, base) // body
	// Tiger stripes: dark bands ringing the torso (slightly proud of the body so
	// they read on any chosen color), so it actually looks like a tiger.
	stripe := tint(col, 0.2)
	for _, zk := range []float64{-0.46, -0.2, 0.06, 0.32, 0.54} {
		dst = box(dst, V3{0, 0.62*s + bob, zk * s}, V3{0.36 * s, 0.285 * s, 0.05 * s}, stripe, base)
	}
	dst = box(dst, V3{0, 0.78*s + bob, 0.78 * s}, V3{0.24 * s, 0.2 * s, 0.22 * s}, bright, hx)         // head
	dst = box(dst, V3{0, 0.7*s + bob, 1.02 * s}, V3{0.12 * s, 0.1 * s, 0.14 * s}, tint(col, 0.85), hx) // snout
	dst = box(dst, V3{0, 0.62*s + bob, 1.06 * s}, V3{0.1 * s, 0.025 * s, 0.1 * s}, tint(col, 0.5), hx) // mouth
	fang := [3]float64{0.95, 0.95, 0.85}                                                               // bared fangs (feline)
	for _, side := range []float64{1, -1} {
		dst = box(dst, V3{side * 0.05 * s, 0.585*s + bob, 1.12 * s}, V3{0.018 * s, 0.04 * s, 0.018 * s}, fang, hx)
	}
	for _, side := range []float64{1, -1} { // upright ears
		dst = box(dst, V3{side * 0.14 * s, (0.98 + 0.03*math.Sin(clock*10)) * s, 0.66 * s}, V3{0.045 * s, 0.1 * s, 0.03 * s}, legc, hx)
	}
	dst = eyePair(dst, hx, 0.11, 0.82, 0.99, 0.05, s) // accent eyes
	// Long feline tail in three swishing segments (more cat than the old stub).
	wag := 0.22 * math.Sin(clock*3)
	dst = box(dst, V3{wag * 0.5 * s, 0.86 * s, -0.8 * s}, V3{0.05 * s, 0.05 * s, 0.3 * s}, legc, base)
	dst = box(dst, V3{wag * 1.3 * s, 0.92 * s, -1.12 * s}, V3{0.04 * s, 0.04 * s, 0.26 * s}, legc, base)
	dst = box(dst, V3{wag * 2.2 * s, 1.02 * s, -1.4 * s}, V3{0.035 * s, 0.035 * s, 0.2 * s}, tint(col, 0.4), base) // dark tip
	// 4 legs: front pair + back pair, diagonally synced (trot)
	leg := func(side, zf, phase float64) {
		swing := 0.4 * math.Sin(clock*5+phase)
		hip := V3{side * 0.3 * s, 0.6*s + bob, zf * s}
		dst = limb(dst, base, hip, side*0.12, swing, 0.6*s, 0.07*s, legc)
	}
	leg(1, 0.5, 0)        // front-right
	leg(-1, 0.5, math.Pi) // front-left
	leg(1, -0.5, math.Pi) // back-right
	leg(-1, -0.5, 0)      // back-left
	return dst
}

// appendInsect: three body segments in a line, six legs, two antennae.
func appendInsect(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.5)
	hx := aimAt(base, curAim, V3{0, 0.42 * s, 0.3 * s})                                    // head tracks the aim
	dst = box(dst, V3{0, 0.4 * s, 0.5 * s}, V3{0.2 * s, 0.18 * s, 0.22 * s}, bright, hx)   // head
	dst = eyePair(dst, hx, 0.11, 0.45, 0.75, 0.05, s)                                      // accent eyes
	dst = box(dst, V3{0, 0.42 * s, 0.12 * s}, V3{0.26 * s, 0.2 * s, 0.28 * s}, col, base)  // thorax
	dst = box(dst, V3{0, 0.42 * s, -0.4 * s}, V3{0.3 * s, 0.24 * s, 0.4 * s}, col, base)   // abdomen
	dst = limb(dst, hx, V3{0.08 * s, 0.55 * s, 0.5 * s}, 0.3, -2.4, 0.34*s, 0.022*s, legc) // antennae: rise up and slightly forward
	dst = limb(dst, hx, V3{-0.08 * s, 0.55 * s, 0.5 * s}, -0.3, -2.4, 0.34*s, 0.022*s, legc)
	zPos := []float64{0.28, 0.06, -0.18}
	for k := 0; k < 3; k++ {
		for _, side := range []float64{1, -1} {
			phase := float64(k)*1.0 + math.Pi*(0.5-0.5*side)
			swing := 0.3 * math.Sin(clock*7+phase)
			hip := V3{side * 0.24 * s, 0.42 * s, zPos[k] * s}
			dst = limb(dst, base, hip, side*0.95, swing, 0.5*s, 0.035*s, legc)
		}
	}
	return dst
}

// appendHumanoid: upright torso + head, two arms swinging opposite two legs.
func appendHumanoid(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	limbc := tint(col, 0.7)
	hx := aimAt(base, curAim, V3{0, 1.55 * s, 0})                                           // head tracks the aim
	dst = box(dst, V3{0, 1.15 * s, 0}, V3{0.22 * s, 0.32 * s, 0.15 * s}, col, base)         // torso
	dst = box(dst, V3{0, 1.62 * s, 0.02 * s}, V3{0.16 * s, 0.16 * s, 0.16 * s}, bright, hx) // head
	dst = eyePair(dst, hx, 0.07, 1.64, 0.2, 0.04, s)                                        // accent eyes
	swing := 0.5 * math.Sin(clock*4)
	// legs from the hips (down), arms from the shoulders (down), opposed phase;
	// the firing arm raises and follows the aim (it IS the gun)
	gunXF := aimAt(base, curAim, V3{0, 1.42 * s, 0})
	dst = limb(dst, base, V3{0.12 * s, 0.85 * s, 0}, 0.1, swing, 0.8*s, 0.08*s, limbc)     // right leg
	dst = limb(dst, base, V3{-0.12 * s, 0.85 * s, 0}, -0.1, -swing, 0.8*s, 0.08*s, limbc)  // left leg
	dst = limb(dst, gunXF, V3{0.28 * s, 1.42 * s, 0}, 0.18, -1.35, 0.62*s, 0.06*s, limbc)  // right arm: leveled, tracks aim
	dst = limb(dst, base, V3{-0.28 * s, 1.42 * s, 0}, -0.18, swing, 0.62*s, 0.06*s, limbc) // left arm
	return dst
}

// appendElephant: a massive body on four pillar legs, big flat ears, bright
// tusks, and a hanging trunk built from a segment chain - head, ears, and trunk
// track the aim (the trunk IS the shield spray).
func appendElephant(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.7)
	dst = box(dst, V3{0, 1.1 * s, -0.15 * s}, V3{0.5 * s, 0.45 * s, 0.7 * s}, col, base) // body
	hx := aimAt(base, curAim, V3{0, 1.5 * s, 0.5 * s})
	dst = box(dst, V3{0, 1.5 * s, 0.72 * s}, V3{0.3 * s, 0.28 * s, 0.3 * s}, bright, hx) // head
	flapE := 0.12 * math.Sin(clock*3)
	for _, side := range []float64{1, -1} { // big floppy ears: broad, thin, swinging
		dst = box(dst, V3{side * (0.46 + flapE) * s, 1.52 * s, 0.58 * s}, V3{0.08 * s, 0.34 * s, 0.3 * s}, tint(col, 0.85), hx)
		// tusks: ivory, curving forward under the head
		dst = box(dst, V3{side * 0.16 * s, 1.26 * s, 0.95 * s}, V3{0.045 * s, 0.045 * s, 0.22 * s}, [3]float64{0.95, 0.93, 0.8}, hx)
	}
	dst = eyePair(dst, hx, 0.17, 1.62, 0.99, 0.05, s)
	sway := 0.13 * math.Sin(clock*2.2) // the trunk hangs and swings (loose, wagging)
	trunk := []V3{{0, 1.3, 0.98}, {0, 1.1, 1.06}, {0, 0.9, 1.12}, {0, 0.72, 1.16}}
	for i, p := range trunk {
		r := (0.1 - float64(i)*0.012) * s
		dst = box(dst, V3{p.X*s + sway*float64(i+1)*0.4*s, p.Y * s, p.Z * s}, V3{r, 0.11 * s, r}, tint(col, 0.85), hx)
	}
	dst = box(dst, V3{0.22 * s * math.Sin(clock*2.6), 1.05 * s, -0.92 * s}, V3{0.035 * s, 0.035 * s, 0.2 * s}, legc, base) // tail, wagging
	for _, zf := range []float64{0.42, -0.55} {                                                                            // pillar legs, slow heavy gait
		for _, side := range []float64{1, -1} {
			swing := 0.14 * math.Sin(clock*3+math.Pi*(0.5-0.5*side)+zf)
			dst = limb(dst, base, V3{side * 0.34 * s, 0.72 * s, zf * s}, side*0.05, swing, 0.68*s, 0.13*s, legc)
		}
	}
	return dst
}

// appendFalcon: a bird - compact body, head with an accent beak tracking the
// aim, one big flapping wing pair, a fanned tail, tucked talons. The sim flies
// it (bodyDef fly); this is just the model.
func appendFalcon(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	limbc := tint(col, 0.7)
	hx := aimAt(base, curAim, V3{0, 1.0 * s, 0.25 * s})                                       // head tracks the aim
	dst = box(dst, V3{0, 0.85 * s, 0}, V3{0.16 * s, 0.16 * s, 0.32 * s}, col, base)           // body
	dst = box(dst, V3{0, 1.05 * s, 0.34 * s}, V3{0.11 * s, 0.1 * s, 0.12 * s}, bright, hx)    // head
	dst = box(dst, V3{0, 1.02 * s, 0.52 * s}, V3{0.04 * s, 0.03 * s, 0.1 * s}, curAccent, hx) // hooked beak
	dst = eyePair(dst, hx, 0.06, 1.1, 0.42, 0.03, s)
	flap := 0.7 * math.Sin(clock*7)
	wingXF := func(hip V3, roll float64) func(V3) V3 {
		sr, cr := math.Sin(roll), math.Cos(roll)
		return func(l V3) V3 { return base(V3{l.X*cr - l.Y*sr, l.X*sr + l.Y*cr, l.Z}.Add(hip)) }
	}
	for _, side := range []float64{1, -1} { // one broad wing pair, strong beat
		dst = box(dst, V3{side * 0.56 * s, 0, 0}, V3{0.56 * s, 0.025 * s, 0.26 * s}, col, wingXF(V3{side * 0.14 * s, 0.9 * s, 0.02 * s}, side*(flap+0.15)))
		dst = box(dst, V3{side * 0.96 * s, 0, -0.06 * s}, V3{0.2 * s, 0.02 * s, 0.18 * s}, bright, wingXF(V3{side * 0.14 * s, 0.9 * s, 0.02 * s}, side*(flap+0.15))) // wingtip feathers
	}
	dst = box(dst, V3{0, 0.82 * s, -0.46 * s}, V3{0.13 * s, 0.02 * s, 0.2 * s}, bright, base) // fanned tail
	for _, side := range []float64{1, -1} {                                                   // legs ending in gripping talons (three little toes each)
		hip := V3{side * 0.07 * s, 0.7 * s, 0.05 * s}
		dst = limb(dst, base, hip, side*0.15, 0.5, 0.2*s, 0.03*s, limbc)
		xf := limbDown(base, hip, side*0.15, 0.5)
		for _, tx := range []float64{0, 0.045, -0.045} {
			dst = box(dst, V3{tx * s, -0.2 * s, 0.05 * s}, V3{0.013 * s, 0.045 * s, 0.013 * s}, curAccent, xf)
		}
	}
	return dst
}

// appendStag: a slim quadruped with a raised neck and a big branching antler
// rack - the rack rides the head and turns with the aim, so the healer reads
// at a glance. Trot gait with a light prance.
func appendStag(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.6)
	bob := 0.03 * s * math.Sin(clock*10)
	dst = box(dst, V3{0, 0.78*s + bob, -0.05 * s}, V3{0.24 * s, 0.22 * s, 0.5 * s}, col, base) // body
	dst = box(dst, V3{0, 1.05*s + bob, 0.42 * s}, V3{0.1 * s, 0.24 * s, 0.12 * s}, col, base)  // raised neck
	hx := aimAt(base, curAim, V3{0, 1.3*s + bob, 0.5 * s})
	dst = box(dst, V3{0, 1.32*s + bob, 0.6 * s}, V3{0.1 * s, 0.1 * s, 0.16 * s}, bright, hx)             // head
	dst = box(dst, V3{0, 1.28*s + bob, 0.8 * s}, V3{0.055 * s, 0.05 * s, 0.08 * s}, tint(col, 0.85), hx) // muzzle
	dst = eyePair(dst, hx, 0.06, 1.37, 0.7, 0.035, s)
	antler := [3]float64{0.9, 0.85, 0.7} // pale bone
	for _, side := range []float64{1, -1} {
		dst = box(dst, V3{side * 0.12 * s, (1.56 + 0.03) * s, 0.55 * s}, V3{0.03 * s, 0.17 * s, 0.03 * s}, antler, hx) // main beam up
		dst = box(dst, V3{side * 0.24 * s, 1.72 * s, 0.52 * s}, V3{0.13 * s, 0.03 * s, 0.03 * s}, antler, hx)          // crown spread
		dst = box(dst, V3{side * 0.34 * s, 1.82 * s, 0.52 * s}, V3{0.03 * s, 0.11 * s, 0.03 * s}, antler, hx)          // outer tine up
		dst = box(dst, V3{side * 0.14 * s, 1.62 * s, 0.66 * s}, V3{0.025 * s, 0.025 * s, 0.1 * s}, antler, hx)         // brow tine forward
	}
	dst = box(dst, V3{0, 0.92 * s, -0.6 * s}, V3{0.03 * s, 0.07 * s, 0.04 * s}, bright, base) // flag tail
	leg := func(side, zf, phase float64) {                                                    // light prancing trot
		swing := 0.45 * math.Sin(clock*5+phase)
		dst = limb(dst, base, V3{side * 0.2 * s, 0.64*s + bob, zf * s}, side*0.08, swing, 0.62*s, 0.045*s, legc)
	}
	leg(1, 0.38, 0)
	leg(-1, 0.38, math.Pi)
	leg(1, -0.42, math.Pi)
	leg(-1, -0.42, 0)
	return dst
}

// appendMinotaur: a broad bull-headed bruiser - heavy torso and shoulders, a
// horned head that tracks the aim, thick legs, and a hammer carried forward.
// When its barrier is up (curShieldUp), a translucent-looking frontal energy
// lattice is drawn in front (body-frontal, matching the sim's block arc),
// fading as the barrier's charge drains.
func appendMinotaur(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	limbc := tint(col, 0.7)
	dst = box(dst, V3{0, 1.05 * s, 0}, V3{0.4 * s, 0.4 * s, 0.28 * s}, col, base)                   // chest
	dst = box(dst, V3{0, 1.38 * s, 0}, V3{0.5 * s, 0.16 * s, 0.3 * s}, col, base)                   // shoulders
	dst = box(dst, V3{0, 0.62 * s, 0}, V3{0.3 * s, 0.22 * s, 0.22 * s}, tint(col, 0.85), base)      // hips
	hx := aimAt(base, curAim, V3{0, 1.62 * s, 0})                                                   // the head turns to the aim
	dst = box(dst, V3{0, 1.66 * s, 0.04 * s}, V3{0.2 * s, 0.18 * s, 0.2 * s}, bright, hx)           // head
	dst = box(dst, V3{0, 1.58 * s, 0.22 * s}, V3{0.12 * s, 0.1 * s, 0.12 * s}, tint(col, 0.85), hx) // snout
	dst = eyePair(dst, hx, 0.1, 1.7, 0.2, 0.04, s)
	horn := [3]float64{0.92, 0.88, 0.7}
	for _, side := range []float64{1, -1} { // horns out, up, then curling forward
		dst = box(dst, V3{side * 0.22 * s, 1.78 * s, 0.04 * s}, V3{0.06 * s, 0.05 * s, 0.05 * s}, horn, hx)
		dst = box(dst, V3{side * 0.3 * s, 1.9 * s, 0.04 * s}, V3{0.05 * s, 0.09 * s, 0.05 * s}, horn, hx)
		dst = box(dst, V3{side * 0.34 * s, 2.02 * s, 0.1 * s}, V3{0.04 * s, 0.05 * s, 0.06 * s}, horn, hx)
	}
	swing := 0.3 * math.Sin(clock*4)
	dst = limb(dst, base, V3{0.18 * s, 0.6 * s, 0}, 0.08, swing, 0.62*s, 0.11*s, limbc)    // right leg
	dst = limb(dst, base, V3{-0.18 * s, 0.6 * s, 0}, -0.08, -swing, 0.62*s, 0.11*s, limbc) // left leg
	dst = limb(dst, base, V3{-0.46 * s, 1.32 * s, 0}, -0.2, swing, 0.7*s, 0.09*s, limbc)   // off arm swings
	gx := aimAt(base, curAim, V3{0, 1.32 * s, 0})                                          // hammer arm levels to the aim
	dst = limb(dst, gx, V3{0.46 * s, 1.32 * s, 0}, 0.2, -1.2, 0.7*s, 0.1*s, limbc)
	dst = box(dst, V3{0.5 * s, 1.32 * s, 0.72 * s}, V3{0.15 * s, 0.19 * s, 0.14 * s}, tint(col, 0.6), gx) // hammer head
	if curShieldUp {
		dst = appendBarrier(dst, base, col, s)
	}
	return dst
}

// appendBarrier draws the minotaur's deployed frontal shield as a bright energy
// lattice (a framed grid, so it reads as a field without fully occluding what's
// behind it). It rides the hull transform (not the aim), so it faces wherever
// the body faces - the same frontal arc the sim absorbs from. Brightness tracks
// the remaining charge so a near-broken barrier visibly dims.
func appendBarrier(dst []Tri, base func(V3) V3, col [3]float64, s float64) []Tri {
	f := 0.4 + 0.6*curShieldFrac
	// Tint the barrier to the owner's colors so you can tell who's behind it from
	// the front: the frame in a brightened primary, the scanlines in the accent.
	bright := func(c [3]float64, k float64) [3]float64 {
		return [3]float64{math.Min(c[0]*1.4+0.15, 1) * k, math.Min(c[1]*1.4+0.15, 1) * k, math.Min(c[2]*1.4+0.15, 1) * k}
	}
	bc := bright(col, f)
	scan := bright(curAccent, f)
	const z, hw, top, bot, th = 1.25, 1.3, 2.15, 0.15, 0.04
	midY, halfY := (top+bot)/2, (top-bot)/2
	// A thin outer frame, then a few horizontal energy scanlines with open gaps
	// between them. The engine can't do real alpha, so the field is mostly empty
	// space (you see the model and the world through it) with just enough lit
	// structure to read as a barrier.
	dst = box(dst, V3{0, top * s, z * s}, V3{hw * s, th * s, th * s}, bc, base)           // top rail
	dst = box(dst, V3{0, bot * s, z * s}, V3{hw * s, th * s, th * s}, bc, base)           // bottom rail
	dst = box(dst, V3{-hw * s, midY * s, z * s}, V3{th * s, halfY * s, th * s}, bc, base) // left edge
	dst = box(dst, V3{hw * s, midY * s, z * s}, V3{th * s, halfY * s, th * s}, bc, base)  // right edge
	const lines = 5
	for k := 1; k <= lines; k++ {
		y := bot + (top-bot)*float64(k)/float64(lines+1)
		dst = box(dst, V3{0, y * s, z * s}, V3{hw * 0.95 * s, 0.016 * s, 0.016 * s}, scan, base) // scanline
	}
	return dst
}
