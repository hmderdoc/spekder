// Spekder door - terminal 3D tank combat. Cross-BBS, cross-platform.
//
// Spekder rasterizes flat-shaded, wireframe-edged 3D into the terminal as
// truecolor half-blocks, rendered from each player's own camera. The arena
// server carries game STATE (tank/projectile positions), never pixels; every
// node renders its own view, so it stays fast over a BBS link.
//
// I/O is abstracted behind the Term interface (term.go): in door mode it speaks
// over the socket the BBS hands us via DOOR32.SYS, otherwise over stdin/stdout.
// The platform specifics (raw tty + fd socket on unix, console VT + Winsock on
// Windows) live in io_unix.go / io_windows.go.
//
// Controls: W/S drive, A/D turn hull, ,/. or left/right arrows aim turret,
// up/down arrows elevate/depress the gun, SPACE fire, ENTER jump, C recenter
// turret (and level the gun), TAB top-down, Q or Ctrl-C quit.
package main

import (
	"bufio"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gm "spekder/internal/game"
	"spekder/internal/tdf"
)

// version is stamped at build time by the release workflow (-X main.version).
var version = "dev"

// dbg, when set, writes a lifecycle log next to the binary (spekder.log) so an
// unexpected exit is never invisible.
var dbg *log.Logger

func logf(format string, a ...any) {
	if dbg != nil {
		dbg.Printf(format, a...)
	}
}

// ---------------------------------------------------------------------------
// vector math
// ---------------------------------------------------------------------------

// V3 aliases the simulation's vector type so renderer and sim share one type.
type V3 = gm.V3

// ---------------------------------------------------------------------------
// scene geometry - flat-shaded triangles in world space
// ---------------------------------------------------------------------------

type Tri struct {
	v       [3]V3      // world-space vertices, CCW from the lit side
	col     [3]float64 // base color 0..1
	wire    bool       // draw the silhouette edges (the "vector" look)
	yawAnim bool       // spin this tri about its centroid over time (demo)
}

// arena holds the static world geometry (floor, walls, obstacles, scenery).
// Dynamic entities are built per-frame. arenaSize is the active map's half-extent
// (set by buildArena), shared by the fog/radar/top-down camera.
var arena []Tri
var arenaSize = gm.ArenaA

func addQuad(a, b, c, d V3, col [3]float64, wire bool) {
	arena = append(arena,
		Tri{v: [3]V3{a, b, c}, col: col, wire: wire},
		Tri{v: [3]V3{a, c, d}, col: col, wire: wire})
}

// buildArena rebuilds the static render geometry for a map: fixed floor + walls,
// plus the map's obstacles (solid boxes) and scenery (decorative props).
func buildArena(m gm.Map) {
	arena = arena[:0]
	A := gm.ArenaA // arena half-extent (per-map; default if unset)
	if m.Size > 0 {
		A = m.Size
	}
	arenaSize = A
	radarRange = A * 1.9 // radar shows the whole map
	// Floor grid: ~2-unit cells, count derived so it spans exactly 2A.
	n := int(math.Round(A))
	if n < 4 {
		n = 4
	}
	cell := 2 * A / float64(n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			x0 := -A + float64(i)*cell
			z0 := -A + float64(j)*cell
			c := [3]float64{0.05, 0.20, 0.10}
			if (i+j)&1 == 0 {
				c = [3]float64{0.08, 0.30, 0.16}
			}
			addQuad(
				V3{x0, 0, z0}, V3{x0 + cell, 0, z0},
				V3{x0 + cell, 0, z0 + cell}, V3{x0, 0, z0 + cell}, c, true)
		}
	}
	// Four walls, normals facing inward.
	const wh = 3.0
	steel := [3]float64{0.30, 0.38, 0.55}
	addQuad(V3{-A, 0, A}, V3{A, 0, A}, V3{A, wh, A}, V3{-A, wh, A}, steel, true)     // +z
	addQuad(V3{A, 0, -A}, V3{-A, 0, -A}, V3{-A, wh, -A}, V3{A, wh, -A}, steel, true) // -z
	addQuad(V3{A, 0, A}, V3{A, 0, -A}, V3{A, wh, -A}, V3{A, wh, A}, steel, true)     // +x
	addQuad(V3{-A, 0, -A}, V3{-A, 0, A}, V3{-A, wh, A}, V3{-A, wh, -A}, steel, true) // -x

	for _, b := range m.Obstacles {
		arena = box(arena, b.Pos, b.Half, b.Color, func(l V3) V3 { return l })
	}
	for _, r := range m.Ramps {
		arena = appendRamp(arena, r)
	}
	for _, p := range m.Scenery {
		arena = appendProp(arena, p)
	}
}

// ---------------------------------------------------------------------------
// renderer
// ---------------------------------------------------------------------------

type Cam struct {
	pos        V3
	yaw, pitch float64
}

var lightDir = V3{0.4, 1.0, 0.3}.Norm()

type Renderer struct {
	W, H   int       // pixel grid (cols x 2*rows3d)
	fb     []byte    // rgb
	zb     []float64 // inverse depth (1/z); larger = nearer
	focal  float64
	fogCol [3]float64
}

func newRenderer(w, h int) *Renderer {
	return &Renderer{W: w, H: h, fb: make([]byte, w*h*3), zb: make([]float64, w*h),
		focal: float64(w) / 2, fogCol: [3]float64{0.04, 0.05, 0.09}}
}

func clampB(f float64) byte {
	if f <= 0 {
		return 0
	}
	if f >= 255 {
		return 255
	}
	return byte(f)
}

func (r *Renderer) clear() {
	for y := 0; y < r.H; y++ {
		// vertical sky gradient: dark navy at top easing toward the horizon
		t := float64(y) / float64(r.H)
		cr := clampB((0.02 + 0.06*t) * 255)
		cg := clampB((0.03 + 0.07*t) * 255)
		cb := clampB((0.08 + 0.12*t) * 255)
		for x := 0; x < r.W; x++ {
			i := (y*r.W + x) * 3
			r.fb[i], r.fb[i+1], r.fb[i+2] = cr, cg, cb
		}
	}
	for i := range r.zb {
		r.zb[i] = 0
	}
}

// camera-space vertex with screen projection
type cv struct {
	x, y, z float64 // camera space
}

func (c Cam) toCam(p V3) cv {
	d := p.Sub(c.pos)
	sa, ca := math.Sin(c.yaw), math.Cos(c.yaw)
	x1 := d.X*ca - d.Z*sa
	z1 := d.X*sa + d.Z*ca
	y1 := d.Y
	st, ct := math.Sin(c.pitch), math.Cos(c.pitch)
	y2 := y1*ct + z1*st
	z2 := -y1*st + z1*ct
	return cv{x1, y2, z2}
}

const near = 0.12

// clipNear clips a convex polygon (CCW) against z >= near.
func clipNear(in []cv) []cv {
	var out []cv
	n := len(in)
	for i := 0; i < n; i++ {
		a := in[i]
		b := in[(i+1)%n]
		ain := a.z >= near
		bin := b.z >= near
		if ain {
			out = append(out, a)
		}
		if ain != bin {
			t := (near - a.z) / (b.z - a.z)
			out = append(out, cv{a.x + (b.x-a.x)*t, a.y + (b.y-a.y)*t, near})
		}
	}
	return out
}

func (r *Renderer) project(c cv) (sx, sy, iz float64) {
	iz = 1 / c.z
	sx = float64(r.W)/2 + r.focal*c.x*iz
	sy = float64(r.H)/2 - r.focal*c.y*iz
	return
}

func (r *Renderer) fog(z float64) float64 {
	// Scale fog with the playfield so you can see across it but the far corners
	// still fade for depth.
	f0, f1 := arenaSize*0.5, arenaSize*2.4
	if z <= f0 {
		return 0
	}
	if z >= f1 {
		return 1
	}
	return (z - f0) / (f1 - f0)
}

func edge(ax, ay, bx, by, px, py float64) float64 {
	return (px-ax)*(by-ay) - (py-ay)*(bx-ax)
}

// fillTri rasterizes one projected triangle with a z-buffer and per-pixel fog.
func (r *Renderer) fillTri(sx, sy, iz [3]float64, col [3]float64) {
	area := edge(sx[0], sy[0], sx[1], sy[1], sx[2], sy[2])
	if math.Abs(area) < 1e-7 {
		return
	}
	minx := int(math.Floor(min3(sx[0], sx[1], sx[2])))
	maxx := int(math.Ceil(max3(sx[0], sx[1], sx[2])))
	miny := int(math.Floor(min3(sy[0], sy[1], sy[2])))
	maxy := int(math.Ceil(max3(sy[0], sy[1], sy[2])))
	if minx < 0 {
		minx = 0
	}
	if miny < 0 {
		miny = 0
	}
	if maxx >= r.W {
		maxx = r.W - 1
	}
	if maxy >= r.H {
		maxy = r.H - 1
	}
	inv := 1 / area
	for py := miny; py <= maxy; py++ {
		fy := float64(py) + 0.5
		for px := minx; px <= maxx; px++ {
			fx := float64(px) + 0.5
			w0 := edge(sx[1], sy[1], sx[2], sy[2], fx, fy) * inv
			w1 := edge(sx[2], sy[2], sx[0], sy[0], fx, fy) * inv
			w2 := edge(sx[0], sy[0], sx[1], sy[1], fx, fy) * inv
			if w0 < 0 || w1 < 0 || w2 < 0 {
				continue
			}
			z := w0*iz[0] + w1*iz[1] + w2*iz[2]
			idx := py*r.W + px
			if z <= r.zb[idx] {
				continue
			}
			r.zb[idx] = z
			fg := r.fog(1 / z)
			o := idx * 3
			r.fb[o] = clampB((col[0]*(1-fg) + r.fogCol[0]*fg) * 255)
			r.fb[o+1] = clampB((col[1]*(1-fg) + r.fogCol[1]*fg) * 255)
			r.fb[o+2] = clampB((col[2]*(1-fg) + r.fogCol[2]*fg) * 255)
		}
	}
}

// drawEdge plots a depth-tested line for the wireframe silhouette.
func (r *Renderer) drawEdge(ax, ay, aiz, bx, by, biz float64, col [3]float64) {
	steps := int(math.Max(math.Abs(bx-ax), math.Abs(by-ay))) + 1
	for s := 0; s <= steps; s++ {
		t := float64(s) / float64(steps)
		x := int(ax + (bx-ax)*t)
		y := int(ay + (by-ay)*t)
		if x < 0 || y < 0 || x >= r.W || y >= r.H {
			continue
		}
		z := aiz + (biz-aiz)*t
		idx := y*r.W + x
		if z*1.012 < r.zb[idx] {
			continue
		}
		fg := r.fog(1 / z)
		o := idx * 3
		r.fb[o] = clampB((col[0]*(1-fg) + r.fogCol[0]*fg) * 255)
		r.fb[o+1] = clampB((col[1]*(1-fg) + r.fogCol[1]*fg) * 255)
		r.fb[o+2] = clampB((col[2]*(1-fg) + r.fogCol[2]*fg) * 255)
	}
}

// drawTris rasterizes a triangle list into the framebuffer. It does NOT clear
// (the caller clears once per frame), so it can be called repeatedly to layer
// the static arena and the dynamic entities. yawAnim tris spin about the world
// origin using t.
func (r *Renderer) drawTris(cam Cam, tris []Tri, t float64) {
	sy, cy := math.Sin(t*0.8), math.Cos(t*0.8) // obelisk spin (yawAnim tris)
	for _, tri := range tris {
		v := tri.v
		if tri.yawAnim {
			for i := range v {
				v[i] = V3{v[i].X*cy - v[i].Z*sy, v[i].Y, v[i].X*sy + v[i].Z*cy}
			}
		}
		// flat shading from the world-space normal (double-sided: no black faces)
		nrm := v[1].Sub(v[0]).Cross(v[2].Sub(v[0])).Norm()
		l := 0.30 + 0.70*math.Abs(nrm.Dot(lightDir))
		col := [3]float64{tri.col[0] * l, tri.col[1] * l, tri.col[2] * l}

		poly := clipNear([]cv{cam.toCam(v[0]), cam.toCam(v[1]), cam.toCam(v[2])})
		if len(poly) < 3 {
			continue
		}
		px := make([]float64, len(poly))
		py := make([]float64, len(poly))
		pz := make([]float64, len(poly))
		for i, c := range poly {
			px[i], py[i], pz[i] = r.project(c)
		}
		// fan-triangulate the (possibly clipped) polygon
		for i := 1; i+1 < len(poly); i++ {
			r.fillTri([3]float64{px[0], px[i], px[i+1]},
				[3]float64{py[0], py[i], py[i+1]},
				[3]float64{pz[0], pz[i], pz[i+1]}, col)
		}
		if tri.wire {
			ec := [3]float64{math.Min(col[0]*1.6+0.12, 1), math.Min(col[1]*1.6+0.12, 1), math.Min(col[2]*1.6+0.12, 1)}
			for i := 0; i < len(poly); i++ {
				j := (i + 1) % len(poly)
				r.drawEdge(px[i], py[i], pz[i], px[j], py[j], pz[j], ec)
			}
		}
	}
}

func min3(a, b, c float64) float64 { return math.Min(a, math.Min(b, c)) }
func max3(a, b, c float64) float64 { return math.Max(a, math.Max(b, c)) }

// ---------------------------------------------------------------------------
// half-block delta encoder (truecolor; cp437 0xDF upper-half-block)
// ---------------------------------------------------------------------------

func encode(r *Renderer, cols, rows3d int, prev []byte) ([]byte, []byte) {
	var b strings.Builder
	cur := make([]byte, rows3d*cols*6)
	if prev == nil {
		b.WriteString("\x1b[2J\x1b[H")
	}
	lastSGR := ""
	curX := -1
	curY := -1
	for y := 0; y < rows3d; y++ {
		for x := 0; x < cols; x++ {
			tp := ((2*y)*cols + x) * 3
			bp := ((2*y+1)*cols + x) * 3
			ci := (y*cols + x) * 6
			copy(cur[ci:ci+3], r.fb[tp:tp+3])
			copy(cur[ci+3:ci+6], r.fb[bp:bp+3])
			if prev != nil && string(prev[ci:ci+6]) == string(cur[ci:ci+6]) {
				continue
			}
			if y == rows3d-1 && x == cols-1 {
				continue // never write the true bottom-right cell: with autowrap on
				// (some fTelnet builds ignore ESC[?7l) it scrolls and floods the screen
			}
			if curY != y || curX != x {
				fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
			}
			sgr := fmt.Sprintf("38;2;%d;%d;%d;48;2;%d;%d;%d",
				cur[ci], cur[ci+1], cur[ci+2], cur[ci+3], cur[ci+4], cur[ci+5])
			if sgr != lastSGR {
				b.WriteString("\x1b[")
				b.WriteString(sgr)
				b.WriteByte('m')
				lastSGR = sgr
			}
			b.WriteByte(0xDF)
			curY, curX = y, x+1
		}
	}
	return []byte(b.String()), cur
}

// ---------------------------------------------------------------------------
// input (shared, time-stamped so auto-repeat reads as "held")
// ---------------------------------------------------------------------------

// menuKey is a discrete navigation event for menu/UI screens (distinct from the
// held-action model the game loop uses).
type menuKey int

const (
	mkUp menuKey = iota
	mkDown
	mkLeft
	mkRight
	mkEnter
	mkTab
)

type input struct {
	mu     sync.Mutex
	last   [aCount]time.Time // indexed by action
	any    time.Time         // last time any byte arrived (for "press any key")
	quitCh chan struct{}
	events chan menuKey // discrete nav events for menus (buffered; drops if full)
}

func (in *input) pushKey(k menuKey) {
	select {
	case in.events <- k:
	default: // menu not listening / buffer full: discrete events are best-effort
	}
}

func (in *input) markAny() { in.mu.Lock(); in.any = time.Now(); in.mu.Unlock() }
func (in *input) anySince(t time.Time) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.any.After(t)
}

const (
	aThrottle = iota // drive forward along hull heading
	aReverse         // drive backward
	aHullL           // rotate hull left
	aHullR           // rotate hull right
	aTurretL         // aim turret left (relative to hull)
	aTurretR         // aim turret right
	aFire            // fire main gun (gated by cooldown)
	aJump            // jump (ENTER)
	aRecenter        // snap turret to hull-forward + level (C)
	aAimUp           // elevate the gun (up arrow)
	aAimDown         // depress the gun (down arrow)
	aCount
)

func (in *input) hit(a int) { in.mu.Lock(); in.last[a] = time.Now(); in.mu.Unlock() }
func (in *input) held(a int) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return time.Since(in.last[a]) < 200*time.Millisecond
}

// snapshot translates the current held-key state into one tick of game input.
func (in *input) snapshot() gm.Input {
	return gm.Input{
		Throttle: in.held(aThrottle),
		Reverse:  in.held(aReverse),
		HullL:    in.held(aHullL),
		HullR:    in.held(aHullR),
		TurretL:  in.held(aTurretL),
		TurretR:  in.held(aTurretR),
		Fire:     in.held(aFire),
		Jump:     in.held(aJump),
		Recenter: in.held(aRecenter),
		AimUp:    in.held(aAimUp),
		AimDown:  in.held(aAimDown),
		Vote:     -1, // overridden by the loop during the lobby
	}
}

func (in *input) reader(t Term) {
	buf := make([]byte, 16)
	esc, csi, iac := false, false, 0
	for {
		n, err := t.Read(buf)
		if err != nil || n == 0 {
			logf("reader exit (EOF/err): n=%d err=%v", n, err)
			close(in.quitCh)
			return
		}
		in.markAny()
		for i := 0; i < n; i++ {
			c := buf[i]
			switch {
			case iac > 0:
				iac--
			case c == 0xFF: // telnet IAC: skip the next two bytes
				iac = 2
			case csi:
				switch c {
				case 'A': // up arrow -> elevate gun (aim up)
					in.hit(aAimUp)
					in.pushKey(mkUp)
				case 'B': // down arrow -> depress gun (aim down)
					in.hit(aAimDown)
					in.pushKey(mkDown)
				case 'C': // right arrow -> aim turret right
					in.hit(aTurretR)
					in.pushKey(mkRight)
				case 'D': // left arrow -> aim turret left
					in.hit(aTurretL)
					in.pushKey(mkLeft)
				}
				csi = false
			case esc:
				esc = false
				if c == '[' {
					csi = true // CSI sequence (arrows); next byte is the final
				} else {
					// Lone ESC or an unsupported intro (e.g. SS3 'ESC O'): do NOT
					// swallow this byte. Reprocess it as a normal key so Q / Ctrl-C
					// still register even after a stray or fragmented ESC.
					i--
				}
			case c == 27:
				esc = true
			case c == 'q' || c == 'Q' || c == 3:
				logf("reader exit: quit key %d", c)
				close(in.quitCh)
				return
			case c == 'w' || c == 'W':
				in.hit(aThrottle)
				in.pushKey(mkUp)
			case c == 's' || c == 'S':
				in.hit(aReverse)
				in.pushKey(mkDown)
			case c == 'a' || c == 'A':
				in.hit(aHullL)
				in.pushKey(mkLeft)
			case c == 'd' || c == 'D':
				in.hit(aHullR)
				in.pushKey(mkRight)
			case c == 'c' || c == 'C': // recenter turret to hull-forward
				in.hit(aRecenter)
			case c == ',' || c == '<': // aim turret left
				in.hit(aTurretL)
			case c == '.' || c == '>': // aim turret right
				in.hit(aTurretR)
			case c == ' ': // fire
				in.hit(aFire)
			case c == '\r' || c == '\n': // ENTER: jump / menu confirm
				in.hit(aJump)
				in.pushKey(mkEnter)
			case c == '\t': // TAB: toggle top-down view
				in.pushKey(mkTab)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// tty / door plumbing
// ---------------------------------------------------------------------------

// drawLeaderboard overlays a live, color-coded standings list down the left
// edge: a swatch in each tank's color + kills-deaths, sorted by kills, with your
// row marked. Colors ARE the identity (same scheme as the tanks/radar).
func drawLeaderboard(w *bufio.Writer, cols, rows int, v viewState) {
	ranked := append([]gm.TankSnap(nil), v.tanks...)
	sort.Slice(ranked, func(a, b int) bool {
		if ranked[a].Kills != ranked[b].Kills {
			return ranked[a].Kills > ranked[b].Kills
		}
		return ranked[a].Deaths < ranked[b].Deaths
	})
	maxRows := rows - 4
	if maxRows > 10 {
		maxRows = 10
	}
	for i, t := range ranked {
		if i >= maxRows {
			break
		}
		mark := "\x1b[0;90m " // dim space
		if t.ID == v.me {
			mark = "\x1b[1;37m>" // your row
		}
		fmt.Fprintf(w, "\x1b[%d;1H%s\x1b[38;2;%d;%d;%dm\xdb\xdb\x1b[0;37m %2d-%d\x1b[0m   ",
			3+i, mark,
			int(clampB(t.Color[0]*255)), int(clampB(t.Color[1]*255)), int(clampB(t.Color[2]*255)),
			t.Kills, t.Deaths)
	}
}

// drawHPBar floats the HP bar in the top-left sky (row 1): bright blocks for
// remaining health over a dim track. Power-ups read off the same bar: SHIELD
// extends it with cyan overshield blocks, CLOAK turns the fill purple and adds
// a "CLOAKED" word, RAPID adds an amber tag. Fixed format, redrawn each frame;
// buff toggles change length, so overlaySig forces a repaint to clear trails.
func drawHPBar(w *bufio.Writer, p *gm.TankSnap) {
	hp := p.HP
	if hp < 0 {
		hp = 0
	}
	const barLen = 18
	fill := hp * barLen / 100
	if fill > barLen {
		fill = barLen
	}
	fillCol := "\x1b[38;2;70;200;90m" // green
	switch {
	case p.Cloak:
		fillCol = "\x1b[38;2;180;110;220m" // purple while cloaked
	case hp <= 30:
		fillCol = "\x1b[38;2;210;70;70m" // red
	case hp <= 60:
		fillCol = "\x1b[38;2;220;200;60m" // yellow
	}
	var b strings.Builder
	b.WriteString("\x1b[1;2H") // row 1, col 2 (top-left, in the sky)
	b.WriteString(fillCol)
	for i := 0; i < fill; i++ {
		b.WriteByte(0xDB) // full block
	}
	b.WriteString("\x1b[38;2;70;70;70m")
	for i := fill; i < barLen; i++ {
		b.WriteByte(0xB1) // medium-shade track
	}
	if p.Shield { // overshield: extend the bar with cyan blocks
		b.WriteString("\x1b[38;2;90;185;230m")
		for i := 0; i < 4; i++ {
			b.WriteByte(0xDB)
		}
	}
	if p.Cloak {
		b.WriteString("\x1b[38;2;180;110;220m CLOAKED")
	}
	if p.Rapid {
		b.WriteString("\x1b[38;2;230;170;40m RAPID")
	}
	b.WriteString("\x1b[0m")
	w.WriteString(b.String())
}

// drawStatus draws the mode + match clock and any mode-specific readout on row 2
// (just below the HP bar, above the leaderboard) — the content that used to live
// in the now-removed bottom status bar.
func drawStatus(w *bufio.Writer, cols int, v viewState, p *gm.TankSnap) {
	var b strings.Builder
	b.WriteString("\x1b[2;1H\x1b[0;37m")
	r := gm.RulesetFor(v.mode)
	b.WriteString(r.Name)
	if r.TimeLimit > 0 { // endless modes (survival) have no match clock
		t := int(v.timer)
		if t < 0 {
			t = 0
		}
		fmt.Fprintf(&b, "  %d:%02d", t/60, t%60)
	}
	switch r.Objective {
	case gm.ObjNeutralFlags:
		fmt.Fprintf(&b, "  FLAGS %d/%d", v.flagsTotal-v.flagsLeft, v.flagsTotal)
	case gm.ObjTeamFlags:
		mark0, mark1 := " ", " "
		if v.myTeam == 0 {
			mark0 = ">"
		}
		if v.myTeam == 1 {
			mark1 = ">"
		}
		fmt.Fprintf(&b, "  \x1b[91m%sRED %d\x1b[0;37m \x1b[94m%sBLU %d\x1b[0;37m",
			mark0, v.teamScore[0], mark1, v.teamScore[1])
		if p.Carrying {
			b.WriteString("  \x1b[93m*FLAG*\x1b[0;37m")
		}
	}
	if r.Bots == gm.BotWaves {
		fmt.Fprintf(&b, "  WAVE %d", v.wave)
	}
	if r.Lives > 0 { // lives-based modes (survival, elimination)
		lv := p.Lives
		if lv < 0 {
			lv = 0
		}
		fmt.Fprintf(&b, "  LIVES %d", lv)
	}
	b.WriteString("\x1b[0m")
	w.WriteString(b.String())
}

// drawDeathBanner overlays a big red TheDraw "DESTROYED" with a respawn count,
// centered. Auto-fits the font to the screen width.
func drawDeathBanner(w *bufio.Writer, cols, rows int, respawnIn float64) {
	const word = "DESTROYED"
	top := rows/2 - 2
	if f, ok := tdf.Fit(word, cols-2, "untx", "union", "block"); ok {
		top = rows/2 - f.Height/2 - 1
		if top < 1 {
			top = 1
		}
		w.WriteString(f.RenderCentered(word, cols, top, tdf.RenderOpts{Recolor: true, FG: 4, Transparent: true})) // CGA red (blood), blended
		top += f.Height
	} else {
		t := "** DESTROYED **"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;31m%s\x1b[0m", top, (cols-len(t))/2+1, t)
	}
	msg := fmt.Sprintf("RESPAWNING IN %.0f", respawnIn+0.99)
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;97m%s\x1b[0m", top+1, (cols-len(msg))/2+1, msg)
}

// overlaySig is a small key identifying the current overlay/HUD state; when it
// changes the loop forces a full repaint so transient overlays and variable-width
// HUD bits (buff tags, the CTF carry flag) clear without leaving trails.
func overlaySig(v viewState, p *gm.TankSnap) string {
	switch v.phase {
	case gm.PhaseCountdown:
		return fmt.Sprintf("c%d", int(math.Ceil(v.timer)))
	case gm.PhaseEnded:
		return "end"
	case gm.PhaseLobby:
		return "lobby"
	default:
		if p.Dead {
			return "dead"
		}
		// buff/carry toggles change HUD width -> force a repaint to clear trails
		return fmt.Sprintf("live%t%t%t%t", p.Shield, p.Rapid, p.Cloak, p.Carrying)
	}
}

// menuChoice is what the menu returns: quit, join the online arena, or a
// single-player mode.
type menuChoice struct {
	quit   bool
	online bool
	mode   gm.Mode
}

// runMenu shows the TDF-titled menu: single-player modes plus ONLINE ARENA
// (enabled only when the sysop configured a server). note (if any) is shown,
// e.g. a failed-connect message from a previous attempt.
func runMenu(w *bufio.Writer, cols, rows int, ip *input, note string) menuChoice {
	type entry struct {
		name, blurb string
		online      bool
		mode        gm.Mode
		ready       bool
	}
	haveArena := arenaConfigured()
	onlineBlurb := "Battle other BBS callers in the live arena."
	if !haveArena {
		onlineBlurb = "No arena server configured (ask the sysop)."
	}
	// Single-player items are built straight from the ruleset table, so a new mode
	// appears here just by being added to gm.Rulesets (Phase B: modes are data).
	var items []entry
	for m := 0; m < len(gm.Rulesets); m++ {
		rs := gm.Rulesets[m]
		items = append(items, entry{name: rs.Name, blurb: rs.Desc, mode: gm.Mode(m), ready: true})
	}
	items = append(items, entry{name: "ONLINE ARENA", blurb: onlineBlurb, online: true, ready: haveArena})
	sel := 0
	titleF, _ := tdf.Fit("SPEKDER", cols-2, "block", "union", "untx")
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		top := 1
		if titleF != nil {
			w.WriteString(titleF.RenderCentered("SPEKDER", cols, top, tdf.RenderOpts{Recolor: true, FG: 11}))
			top += titleF.Height + 1
		}
		listTop := top + 1
		for i, it := range items {
			label := it.name
			if !it.ready {
				label += "  (soon)"
			}
			var style string
			switch {
			case i == sel:
				style = "\x1b[1;30;46m" // selected: black on cyan
			case !it.ready:
				style = "\x1b[0;90m" // unavailable: dim
			case it.online:
				style = "\x1b[0;32m" // online available: green
			default:
				style = "\x1b[0;36m" // single-player: cyan
			}
			fmt.Fprintf(w, "\x1b[%d;%dH%s  %s  \x1b[0m", listTop+i, (cols-len(label)-4)/2+1, style, label)
		}
		blurb := items[sel].blurb
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", listTop+len(items)+1, (cols-len(blurb))/2+1, blurb)
		if note != "" {
			fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;1;31m%s\x1b[0m", listTop+len(items)+3, (cols-len(note))/2+1, note)
		}
		foot := "up/down  select       ENTER  start       Q  quit"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows-1, (cols-len(foot))/2+1, foot)
		w.Flush()
	}
	draw()
	for {
		select {
		case <-ip.quitCh:
			return menuChoice{quit: true}
		case k := <-ip.events:
			switch k {
			case mkUp:
				sel = (sel - 1 + len(items)) % len(items)
				note = ""
				draw()
			case mkDown:
				sel = (sel + 1) % len(items)
				note = ""
				draw()
			case mkEnter:
				it := items[sel]
				if !it.ready {
					if it.online {
						note = "No arena server configured."
					} else {
						note = it.name + " is coming soon."
					}
					draw()
					continue
				}
				return menuChoice{online: it.online, mode: it.mode}
			}
		}
	}
}

// runVehicleMenu lets the player pick a vehicle class (with stats), returning
// the index, or quit=true if they bailed.
func runVehicleMenu(w *bufio.Writer, cols, rows int, ip *input) (int, bool) {
	sel := 1 // HUNTER default
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		hdr := "SELECT  VEHICLE"
		fmt.Fprintf(w, "\x1b[2;%dH\x1b[1;96m%s\x1b[0m", (cols-len(hdr))/2+1, hdr)
		listTop := 5
		for i, v := range gm.Vehicles {
			row := listTop + i*3
			style := "\x1b[0;36m"
			mark := "  "
			if i == sel {
				style = "\x1b[1;30;46m"
				mark = "> "
			}
			name := fmt.Sprintf("%s%-7s", mark, v.Name)
			fmt.Fprintf(w, "\x1b[%d;%dH%s %s \x1b[0m", row, (cols-12)/2+1, style, name)
			stat := fmt.Sprintf("HP %3d   SPEED %.1f   TURN %.1f   FIRE %.2fs", v.MaxHP, v.Speed, v.HullTurn, v.FireDelay)
			fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", row+1, (cols-len(stat))/2+1, stat)
		}
		foot := "up/down  select       ENTER  go       Q  quit"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows-1, (cols-len(foot))/2+1, foot)
		w.Flush()
	}
	draw()
	for {
		select {
		case <-ip.quitCh:
			return 0, true
		case k := <-ip.events:
			switch k {
			case mkUp:
				sel = (sel - 1 + len(gm.Vehicles)) % len(gm.Vehicles)
				draw()
			case mkDown:
				sel = (sel + 1) % len(gm.Vehicles)
				draw()
			case mkEnter:
				return sel, false
			}
		}
	}
}

// drawCountdown overlays the big count-in number (or GO) plus the mode name.
func drawCountdown(w *bufio.Writer, cols, rows int, v viewState) {
	n := int(math.Ceil(v.timer))
	word, fg := fmt.Sprintf("%d", n), 14 // yellow
	if n <= 0 {
		word, fg = "GO", 10 // green
	}
	if f, ok := tdf.Fit(word, cols-2, "block", "union", "untx"); ok {
		top := rows/2 - f.Height/2
		if top < 2 {
			top = 2
		}
		w.WriteString(f.RenderCentered(word, cols, top, tdf.RenderOpts{Recolor: true, FG: fg, Transparent: true}))
	}
	m := v.mode.String()
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;36m%s\x1b[0m", rows/2-5, (cols-len(m))/2+1, m)
}

// cycleVote moves a mode vote among the votable modes (every ruleset).
func cycleVote(cur, dir int) int {
	modes := gm.VotableModes()
	if len(modes) == 0 {
		return cur
	}
	if cur < 0 {
		return int(modes[0])
	}
	pos := 0
	for i, m := range modes {
		if int(m) == cur {
			pos = i
		}
	}
	return int(modes[(pos+dir+len(modes))%len(modes)])
}

// drawLobby overlays the between-match vote lobby: mode options with live vote
// tallies, your pick, the roster, and a countdown to the next match.
func drawLobby(w *bufio.Writer, cols, rows int, v viewState, voteMode int) {
	if f, ok := tdf.Fit("LOBBY", cols-2, "union", "untx", "block"); ok {
		w.WriteString(f.RenderCentered("LOBBY", cols, 1, tdf.RenderOpts{Recolor: true, FG: 11, Transparent: true}))
	}
	votesOf := func(idx int) int {
		if idx >= 0 && idx < len(v.votes) {
			return v.votes[idx]
		}
		return 0
	}
	modes := gm.VotableModes()
	lead, leadN := -1, 0
	for _, m := range modes {
		if n := votesOf(int(m)); n > leadN {
			leadN, lead = n, int(m)
		}
	}
	row := rows/2 - len(modes)/2 - 1
	hdr := "VOTE THE NEXT MODE"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;1;37m%s\x1b[0m", row, (cols-len(hdr))/2+1, hdr)
	row += 2
	for _, m := range modes {
		idx := int(m)
		marker, style := "  ", "\x1b[0;36m"
		if idx == voteMode {
			marker, style = "> ", "\x1b[1;33m"
		}
		if idx == lead && leadN > 0 {
			style = "\x1b[1;30;46m" // leading: black on cyan
		}
		line := fmt.Sprintf("%s%-16s %d votes", marker, gm.RulesetFor(m).Name, votesOf(idx))
		fmt.Fprintf(w, "\x1b[%d;%dH%s%s\x1b[0m", row, (cols-len(line))/2+1, style, line)
		row++
	}
	row += 2
	players := 0
	for i := range v.tanks {
		if !v.tanks[i].Bot {
			players++
		}
	}
	ros := fmt.Sprintf("PLAYERS ONLINE: %d", players)
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", row, (cols-len(ros))/2+1, ros)
	row++
	nm := fmt.Sprintf("NEXT MATCH IN %d", int(math.Ceil(v.timer)))
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;1;32m%s\x1b[0m", row, (cols-len(nm))/2+1, nm)
	foot := "</> or arrows to vote"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows-1, (cols-len(foot))/2+1, foot)
}

// drawScoreboard overlays VICTORY/GAME OVER and a ranked frag list.
func drawScoreboard(w *bufio.Writer, cols, rows int, v viewState) {
	won := v.winnerID == v.me
	title, fg := "GAME OVER", 4 // CGA red
	if v.mode == gm.ModeCTF {
		switch {
		case v.winnerTeam == v.myTeam:
			title, fg = "VICTORY", 10 // green
		case v.winnerTeam < 0:
			title, fg = "DRAW", 14 // yellow
		default:
			title, fg = "DEFEAT", 4 // red
		}
	} else if won {
		title, fg = "VICTORY", 10 // green
	} else if v.mode == gm.ModeFlagRun {
		title = "TIME UP"
	}
	if f, ok := tdf.Fit(title, cols-2, "union", "untx", "block"); ok {
		w.WriteString(f.RenderCentered(title, cols, 1, tdf.RenderOpts{Recolor: true, FG: fg, Transparent: true}))
	}
	if v.mode == gm.ModeFlagRun {
		line := fmt.Sprintf("collected %d of %d flags", v.flagsTotal-v.flagsLeft, v.flagsTotal)
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;97m%s\x1b[0m", rows/2, (cols-len(line))/2+1, line)
		return
	}
	if v.mode == gm.ModeCTF {
		red := fmt.Sprintf("RED %d", v.teamScore[0])
		blu := fmt.Sprintf("BLU %d", v.teamScore[1])
		plain := red + "   -   " + blu
		colStart := (cols-len(plain))/2 + 1
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;91m%s\x1b[0;37m   -   \x1b[1;94m%s\x1b[0m",
			rows/2, colStart, red, blu)
		return
	}
	listTop := rows / 2
	if v.mode == gm.ModeSurvival {
		line := fmt.Sprintf("you survived to WAVE %d", v.wave)
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;97m%s\x1b[0m", rows/2-1, (cols-len(line))/2+1, line)
		listTop = rows/2 + 1
	}
	ranked := append([]gm.TankSnap(nil), v.tanks...)
	sort.Slice(ranked, func(a, b int) bool { return ranked[a].Kills > ranked[b].Kills })
	for i, t := range ranked {
		if i >= 8 {
			break
		}
		name := "BOT"
		if !t.Bot {
			name = "PLAYER"
		}
		style := "\x1b[0;37m"
		if t.ID == v.me {
			name, style = "YOU", "\x1b[1;97m"
		}
		body := fmt.Sprintf("%-7s  %2d frags   %2d deaths", name, t.Kills, t.Deaths)
		// color swatch in the tank's color, then the body
		col := (cols-(len(body)+3))/2 + 1
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[38;2;%d;%d;%dm\xdb\xdb\x1b[0m %s%s\x1b[0m",
			listTop+i, col,
			int(clampB(t.Color[0]*255)), int(clampB(t.Color[1]*255)), int(clampB(t.Color[2]*255)),
			style, body)
	}
}

// drawRadarCorners overlays the four CP437 corner glyphs marking the radar's
// bounds. They sit at fixed cells (so no trails) and float over the sky; the
// blips themselves live in the framebuffer (drawRadar).
func drawRadarCorners(w *bufio.Writer, cols int) {
	c0, c1, r0, r1 := radarRect(cols)
	const dim = "\x1b[0;36m" // dim cyan
	corner := func(row, col int, ch byte) {
		fmt.Fprintf(w, "\x1b[%d;%dH%s", row, col, dim)
		w.WriteByte(ch)
	}
	corner(r0, c0, 0xDA) // top-left
	corner(r0, c1, 0xBF) // top-right
	corner(r1, c0, 0xC0) // bottom-left
	corner(r1, c1, 0xD9) // bottom-right
	w.WriteString("\x1b[0m")
}

// splash shows the TheDraw title banner until ~1.6s pass or any key is pressed.
// The font is auto-selected to fit the caller's screen width (the library has
// fonts of every size; we embed a few spanning 47..114 cols and pick the
// largest that fits), falling back to plain text on a very narrow terminal.
func splash(w *bufio.Writer, cols, rows int, ip *input) {
	w.WriteString("\x1b[2J\x1b[H")
	bannerRows := 1
	if f, ok := tdf.Fit("SPEKDER", cols-2, "block", "union", "untx"); ok {
		top := rows/2 - f.Height/2 - 1
		if top < 1 {
			top = 1
		}
		w.WriteString(f.RenderCentered("SPEKDER", cols, top, tdf.RenderOpts{Recolor: true, FG: 11})) // bright cyan
		bannerRows = top + f.Height
	} else { // too narrow for any big font: plain bold title
		title := "S P E C T R E"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;96m%s\x1b[0m", rows/2-1, (cols-len(title))/2+1, title)
		bannerRows = rows/2 - 1
	}
	sub := "T A N K   A R E N A"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;1;36m%s\x1b[0m", bannerRows+1, (cols-len(sub))/2+1, sub)
	ctl := "W/S drive  A/D turn  ,/. aim  up/down elevate  C recenter  SPACE fire  ENTER jump  TAB map  Q quit"
	if len(ctl) > cols {
		ctl = "WASD move  arrows aim/elevate  C recenter  SPACE fire  Q quit"
	}
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", bannerRows+3, (cols-len(ctl))/2+1, ctl)
	hint := "press any key"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", bannerRows+5, (cols-len(hint))/2+1, hint)
	w.Flush()

	start := time.Now()
	for time.Since(start) < 1600*time.Millisecond {
		select {
		case <-ip.quitCh:
			return
		default:
		}
		if ip.anySince(start) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func main() {
	if exe, err := os.Executable(); err == nil {
		if lf, e := os.OpenFile(filepath.Join(filepath.Dir(exe), "spekder.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); e == nil {
			dbg = log.New(lf, "", log.LstdFlags|log.Lmicroseconds)
		}
	}
	logf("=== spekder %s start: args=%v pid=%d ===", version, os.Args, os.Getpid())

	// Parse args: an optional "-dropfile <path>" (or SPEKDER_DROPFILE env) names
	// the BBS dropfile; remaining positional args are "<cols> <rows>" overrides.
	// Default dropfile is DOOR32.SYS in the working directory (the convention).
	dropfile := os.Getenv("SPEKDER_DROPFILE")
	if dropfile == "" {
		dropfile = "DOOR32.SYS"
	}
	var pos []string
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		switch {
		case a == "-dropfile" && i+1 < len(os.Args):
			i++
			dropfile = os.Args[i]
		case strings.HasPrefix(a, "-dropfile="):
			dropfile = a[len("-dropfile="):]
		default:
			pos = append(pos, a)
		}
	}

	// Establish the I/O channel first (the size probe needs it): the inherited
	// telnet/socket handle from the dropfile in door mode, else stdin/stdout.
	// openTerm is platform-specific (raw tty on unix, console VT / Winsock on
	// Windows) but presents one Term to the rest of the program.
	term, restore, err := openTerm(dropfile)
	if err != nil {
		logf("openTerm failed: %v", err)
		fmt.Fprintln(os.Stderr, "spekder: cannot open terminal:", err)
		os.Exit(1)
	}
	defer term.Close()

	// Terminal size, best source first: the OS (ioctl / console API for a local
	// terminal) -> ANSI probe over the channel (the door/socket path) -> 80x25.
	// The probe MUST run before the reader goroutine starts so it can read the
	// ESC[..R reply. Explicit "<cols> <rows>" args override everything.
	cols, rows := 80, 25
	if c, r, ok := localTermSize(); ok {
		cols, rows = c, r
		logf("term size from OS: %dx%d", c, r)
	} else if c, r, ok := probeSize(term, 600*time.Millisecond); ok {
		cols, rows = c, r
		logf("term size from ANSI probe: %dx%d", c, r)
	} else {
		logf("term size unknown; default 80x25")
	}
	if len(pos) >= 2 {
		if c, e := strconv.Atoi(pos[0]); e == nil {
			cols = c
		}
		if r, e := strconv.Atoi(pos[1]); e == nil {
			rows = r
		}
		logf("term size overridden by args: %dx%d", cols, rows)
	}
	if cols < 20 {
		cols = 20
	}
	if rows < 8 {
		rows = 8
	}
	rows3d := rows // 3D scene now owns every row (HUD is drawn over the top-left sky)

	rnd := newRenderer(cols, 2*rows3d)
	var cam Cam // arena geometry is (re)built in the loop when the map changes

	prevHP := -1
	flash := 0.0

	// Raw mode is set inside openTerm. The input reader blocks in its own
	// goroutine; Term.Read tolerates the non-blocking inherited door socket.
	fmt.Fprint(term, "\x1b[?25l\x1b[?7l") // hide cursor, disable auto-wrap
	ip := &input{quitCh: make(chan struct{}), events: make(chan menuKey, 32)}
	go ip.reader(term)
	logf("setup done: grid %dx%d (rows3d=%d), entering loop", cols, rows, rows3d)

	cleanup := func() {
		restore()
		fmt.Fprint(term, "\x1b[?7h\x1b[0m\x1b[?25h\x1b[2J\x1b[H")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, shutdownSignals...)

	w := bufio.NewWriterSize(term, 1<<16)
	splash(w, cols, rows, ip)

	// The menu drives everything: pick a single-player mode, or join the arena.
	var sess session
	note := ""
	for {
		choice := runMenu(w, cols, rows, ip, note)
		if choice.quit {
			cleanup()
			return
		}
		vehicle, vquit := runVehicleMenu(w, cols, rows, ip)
		if vquit {
			cleanup()
			return
		}
		if !choice.online {
			sess = newOfflineSession(offlineBots, choice.mode, vehicle)
			break
		}
		ns, err := connectArena(vehicle)
		if err != nil {
			note = "Could not reach the arena: " + err.Error()
			continue
		}
		sess = ns
		break
	}
	defer sess.close()

	const targetFPS = 30.0
	frameBudget := time.Second / time.Duration(targetFPS)
	var prev []byte
	lastSig := ""
	start := time.Now()
	last := start
	fpsT := start
	frames := 0
	fps := 0
	voteMode := -1   // our lobby vote (mode index), cycled with </>
	curMapSig := "?" // signature of the currently-built map; rebuild on change
	topdown := false
	lastPhase := gm.PhaseActive

loop:
	for {
		select {
		case <-ip.quitCh:
			logf("loop exit: input channel closed")
			break loop
		case <-sig:
			logf("loop exit: signal")
			break loop
		default:
		}
		now := time.Now()
		dt := now.Sub(last).Seconds()
		if dt > 0.1 {
			dt = 0.1 // clamp after a stall so nothing teleports
		}
		last = now

		// Drain discrete nav events; in the lobby they cycle our mode vote.
	drainEvents:
		for {
			select {
			case k := <-ip.events:
				switch {
				case k == mkTab:
					topdown = !topdown
					prev = nil // view changed: full repaint
				case lastPhase == gm.PhaseLobby && k == mkLeft:
					voteMode = cycleVote(voteMode, -1)
				case lastPhase == gm.PhaseLobby && k == mkRight:
					voteMode = cycleVote(voteMode, +1)
				}
			default:
				break drainEvents
			}
		}
		gin := ip.snapshot()
		gin.Vote = voteMode

		v := sess.step(dt, gin)
		lastPhase = v.phase
		if !v.ready { // no view yet (awaiting first STATE from the server)
			fmt.Fprint(w, "\x1b[2J\x1b[H\x1b[0;1;37m  Connecting to arena...\x1b[0m")
			w.Flush()
			prev = nil // force a full repaint on the first real frame
			if d := frameBudget - time.Since(now); d > 0 {
				time.Sleep(d)
			}
			continue
		}
		p := v.self

		// Rebuild the static geometry when the active map changes (and full repaint).
		if sig := fmt.Sprintf("%s/%.0f/%d", v.gmap.Name, v.gmap.Size, len(v.gmap.Obstacles)); sig != curMapSig {
			buildArena(v.gmap)
			curMapSig = sig
			prev = nil
		}

		// Force a full repaint whenever the overlay changes (phase, countdown
		// tick, or death toggle) so transient overlays clear cleanly instead of
		// leaving stale cells where the scene is static.
		if sig := overlaySig(v, &p); sig != lastSig {
			prev = nil
			lastSig = sig
		}

		// Client-side damage flash: watch our own HP drop between ticks.
		if prevHP >= 0 && p.HP < prevHP {
			flash = hitFlashTime
		}
		prevHP = p.HP
		if flash > 0 {
			if flash -= dt; flash < 0 {
				flash = 0
			}
		}

		if topdown {
			// Overhead tactical view: camera high over arena center, looking
			// straight down. Height fits the arena in the (wider-than-tall) frame.
			a := arenaSize + 2
			camY := a
			if r := float64(rnd.W) / float64(rnd.H); r > 1 {
				camY = a * r
			}
			cam.pos = V3{0, camY, 0}
			cam.yaw, cam.pitch = 0, math.Pi/2
		} else {
			// First-person: ride our tank (predicted in net mode), look along the turret.
			cam.pos = v.camPos.Add(V3{0, gm.EyeHeight, 0})
			cam.yaw = v.camYaw
			cam.pitch = -v.viewPitch // gun up (+pitch) tilts the view up (cam.pitch<0 looks up)
		}

		// Crosshair color doubles as the reload gauge: red while reloading, amber
		// when rapid-fire is ready, green otherwise (gray when dead).
		reticle := [3]byte{90, 200, 110}
		switch {
		case p.Dead:
			reticle = [3]byte{120, 120, 120}
		case p.Reload > 0.02:
			reticle = [3]byte{205, 70, 70}
		case p.Rapid:
			reticle = [3]byte{230, 170, 40}
		}
		rnd.renderWorld(cam, now.Sub(start).Seconds(), v.tanks, v.shots, v.flags, v.pickups, v.gmap.Entities, v.ents, v.me, flash, topdown, reticle, v.viewTurret)
		frame, cur := encode(rnd, cols, rows3d, prev)
		prev = cur
		w.Write(frame)

		// HUD overlays (drawn over the top-left sky; no bottom bar anymore)
		frames++
		if now.Sub(fpsT) >= time.Second {
			fps = frames
			frames = 0
			fpsT = now
			logf("fps=%d phase=%d t=%.0f hp=%d kills=%d deaths=%d shots=%d", fps, v.phase, v.timer, p.HP, p.Kills, p.Deaths, len(v.shots))
		}
		switch v.phase {
		case gm.PhaseCountdown:
			drawCountdown(w, cols, rows, v)
		case gm.PhaseEnded:
			drawScoreboard(w, cols, rows, v)
		case gm.PhaseLobby:
			drawLobby(w, cols, rows, v, voteMode)
		default:
			if !topdown {
				drawRadarCorners(w, cols)
			}
			drawLeaderboard(w, cols, rows, v)
			drawStatus(w, cols, v, &p) // mode + clock + mode-specifics, row 2 (below HP bar)
			if p.Dead {
				drawDeathBanner(w, cols, rows, p.RespawnIn)
			} else {
				drawHPBar(w, &p)
			}
		}
		w.Flush()

		if d := frameBudget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
	cleanup()
}
