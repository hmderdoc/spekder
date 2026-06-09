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
func fitCam(tris []Tri, w, h int, angle float64) Cam {
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
	D := math.Max(dW, dH)
	camY := center.Y + 0.3*halfY // gentle downward look for a 3D feel
	return Cam{
		pos:   V3{X: center.X + D*math.Sin(angle), Y: camY, Z: center.Z + D*math.Cos(angle)},
		yaw:   angle + math.Pi,
		pitch: math.Atan2(camY-center.Y, D), // aim back at the model center
	}
}

// bodyScale normalizes each creature's footprint so the species read at roughly
// tank size (their proportions in the builders differ a lot in raw height/width).
func bodyScale(body int) float64 {
	switch body {
	case gm.BodyHumanoid:
		return 0.8 // tall: trim so it isn't oversized next to tanks
	case gm.BodySpider:
		return 1.15 // low + wide: bump so it isn't tiny
	case gm.BodyInsect:
		return 1.05
	case gm.BodyQuad:
		return 1.0
	case gm.BodyScorpion:
		return 1.1
	case gm.BodySerpent:
		return 1.1
	case gm.BodyTripod:
		return 0.82 // tall walker: trim a touch
	case gm.BodyDrone:
		return 1.0
	case gm.BodyCrab:
		return 1.05
	case gm.BodyOctopod:
		return 1.0
	}
	return 1.0
}

func appendCreature(dst []Tri, t *gm.TankSnap, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	s *= bodyScale(t.Body)
	switch t.Body {
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
	}
	return dst
}

// appendCrab: wide low shell, eyestalks, two big side pincers that open and close,
// and six scuttling side legs.
func appendCrab(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.6)
	dst = box(dst, V3{0, 0.38 * s, 0}, V3{0.5 * s, 0.2 * s, 0.34 * s}, col, base) // wide shell
	for _, side := range []float64{1, -1} {                                       // eyestalks
		dst = box(dst, V3{side * 0.14 * s, 0.6 * s, 0.28 * s}, V3{0.035 * s, 0.12 * s, 0.035 * s}, legc, base)
		dst = box(dst, V3{side * 0.14 * s, 0.72 * s, 0.28 * s}, V3{0.06 * s, 0.06 * s, 0.06 * s}, bright, base)
	}
	cl := 0.09 * math.Sin(clock*4) // pincers open/close
	for _, side := range []float64{1, -1} {
		dst = box(dst, V3{side * 0.5 * s, 0.36 * s, 0.32 * s}, V3{0.08 * s, 0.06 * s, 0.2 * s}, legc, base) // arm
		dst = box(dst, V3{side * 0.56 * s, (0.42 + cl) * s, 0.54 * s}, V3{0.14 * s, 0.06 * s, 0.12 * s}, bright, base)
		dst = box(dst, V3{side * 0.56 * s, (0.3 - cl) * s, 0.54 * s}, V3{0.14 * s, 0.06 * s, 0.12 * s}, bright, base)
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
	dst = box(dst, V3{0, 0.75 * s, 0}, V3{0.34 * s, 0.32 * s, 0.34 * s}, col, base) // mantle
	dst = box(dst, V3{0, 0.96 * s, 0}, V3{0.26 * s, 0.22 * s, 0.26 * s}, col, base) // dome
	dst = box(dst, V3{0.13 * s, 0.78 * s, 0.3 * s}, V3{0.07 * s, 0.07 * s, 0.05 * s}, bright, base)
	dst = box(dst, V3{-0.13 * s, 0.78 * s, 0.3 * s}, V3{0.07 * s, 0.07 * s, 0.05 * s}, bright, base)
	for i := 0; i < 8; i++ { // tentacles: hip on a ring, lean radially out, writhe
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
	// tail: segments arching back, up, then forward over the body; sway grows along it
	sway := 0.12 * math.Sin(clock*3)
	seg := []V3{{0, 0.5, -0.5}, {0, 0.74, -0.62}, {0, 0.98, -0.56}, {0, 1.16, -0.32}, {0, 1.24, -0.02}}
	for i, p := range seg {
		r := (0.13 - float64(i)*0.013) * s
		dst = box(dst, V3{p.X*s + sway*float64(i+1)*s, p.Y * s, p.Z * s}, V3{r, r, 0.15 * s}, col, base)
	}
	dst = box(dst, V3{sway * 6 * s, 1.28 * s, 0.16 * s}, V3{0.05 * s, 0.1 * s, 0.09 * s}, bright, base) // stinger
	return dst
}

// appendSerpent: a tapering chain of segments that undulates side to side (a
// traveling sine wave), bright head with eyes, no legs.
func appendSerpent(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	const n = 8
	headX := 0.0
	for i := 0; i < n; i++ {
		z := (0.55 - float64(i)*0.18) * s
		x := 0.26 * s * math.Sin(clock*4-float64(i)*0.7)
		r := (0.22 - float64(i)*0.016) * s
		c := col
		if i == 0 {
			c, headX = bright, x
		}
		dst = box(dst, V3{x, 0.2*s + r, z}, V3{r, r, 0.17 * s}, c, base)
	}
	eye := [3]float64{0.05, 0.05, 0.05}
	dst = box(dst, V3{headX + 0.1*s, 0.3 * s, 0.66 * s}, V3{0.04 * s, 0.04 * s, 0.04 * s}, eye, base)
	dst = box(dst, V3{headX - 0.1*s, 0.3 * s, 0.66 * s}, V3{0.04 * s, 0.04 * s, 0.04 * s}, eye, base)
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
	dst = box(dst, V3{0, 0.42 * s, 0.18 * s}, V3{0.34 * s, 0.20 * s, 0.34 * s}, col, base) // cephalothorax
	dst = box(dst, V3{0, 0.46 * s, -0.34 * s}, V3{0.46 * s, 0.30 * s, 0.5 * s}, col, base) // abdomen
	dst = box(dst, V3{0.12 * s, 0.52 * s, 0.46 * s}, V3{0.05 * s, 0.05 * s, 0.05 * s}, bright, base)
	dst = box(dst, V3{-0.12 * s, 0.52 * s, 0.46 * s}, V3{0.05 * s, 0.05 * s, 0.05 * s}, bright, base)
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

// appendQuad: a horizontal body on four legs, head up front, a tail.
func appendQuad(dst []Tri, base func(V3) V3, col, bright [3]float64, s, clock float64) []Tri {
	legc := tint(col, 0.6)
	dst = box(dst, V3{0, 0.62 * s, 0}, V3{0.34 * s, 0.26 * s, 0.62 * s}, col, base)           // body
	dst = box(dst, V3{0, 0.78 * s, 0.78 * s}, V3{0.24 * s, 0.22 * s, 0.26 * s}, bright, base) // head
	dst = box(dst, V3{0, 0.86 * s, -0.7 * s}, V3{0.05 * s, 0.05 * s, 0.3 * s}, legc, base)    // tail
	// 4 legs: front pair + back pair, diagonally synced (trot)
	leg := func(side, zf, phase float64) {
		swing := 0.4 * math.Sin(clock*5+phase)
		hip := V3{side * 0.3 * s, 0.6 * s, zf * s}
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
	dst = box(dst, V3{0, 0.4 * s, 0.5 * s}, V3{0.2 * s, 0.18 * s, 0.22 * s}, bright, base)   // head
	dst = box(dst, V3{0, 0.42 * s, 0.12 * s}, V3{0.26 * s, 0.2 * s, 0.28 * s}, col, base)    // thorax
	dst = box(dst, V3{0, 0.42 * s, -0.4 * s}, V3{0.3 * s, 0.24 * s, 0.4 * s}, col, base)     // abdomen
	dst = limb(dst, base, V3{0.08 * s, 0.55 * s, 0.66 * s}, 0.4, -1.0, 0.3*s, 0.025*s, legc) // antennae (point up-fwd)
	dst = limb(dst, base, V3{-0.08 * s, 0.55 * s, 0.66 * s}, -0.4, -1.0, 0.3*s, 0.025*s, legc)
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
	dst = box(dst, V3{0, 1.15 * s, 0}, V3{0.22 * s, 0.32 * s, 0.15 * s}, col, base)           // torso
	dst = box(dst, V3{0, 1.62 * s, 0.02 * s}, V3{0.16 * s, 0.16 * s, 0.16 * s}, bright, base) // head
	swing := 0.5 * math.Sin(clock*4)
	// legs from the hips (down), arms from the shoulders (down), opposed phase
	dst = limb(dst, base, V3{0.12 * s, 0.85 * s, 0}, 0.1, swing, 0.8*s, 0.08*s, limbc)     // right leg
	dst = limb(dst, base, V3{-0.12 * s, 0.85 * s, 0}, -0.1, -swing, 0.8*s, 0.08*s, limbc)  // left leg
	dst = limb(dst, base, V3{0.28 * s, 1.42 * s, 0}, 0.18, -swing, 0.62*s, 0.06*s, limbc)  // right arm
	dst = limb(dst, base, V3{-0.28 * s, 1.42 * s, 0}, -0.18, swing, 0.62*s, 0.06*s, limbc) // left arm
	return dst
}
