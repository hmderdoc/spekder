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
	_ "embed"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gm "spekder/internal/game"
	"spekder/internal/proto"
	"spekder/internal/tdf"
)

// version is stamped at build time by the release workflow (-X main.version).
var version = "dev"

// sigCh delivers OS shutdown signals; playMatch watches it to end a match.
var sigCh chan os.Signal

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
var hideArenaWalls bool // demo/attract mode renders the arena without its border walls

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
	builtObstacles, builtRamps = m.Obstacles, m.Ramps // for render-side ground queries
	radarRange = A * 1.9                              // radar shows the whole map
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
	// Four walls, normals facing inward. (Skipped in demo/attract mode, where a
	// roaming chase camera would otherwise get blocked by them.)
	if !hideArenaWalls {
		const wh = 3.0
		steel := [3]float64{0.30, 0.38, 0.55}
		addQuad(V3{-A, 0, A}, V3{A, 0, A}, V3{A, wh, A}, V3{-A, wh, A}, steel, true)     // +z
		addQuad(V3{A, 0, -A}, V3{-A, 0, -A}, V3{-A, wh, -A}, V3{A, wh, -A}, steel, true) // -z
		addQuad(V3{A, 0, A}, V3{A, 0, -A}, V3{A, wh, -A}, V3{A, wh, A}, steel, true)     // +x
		addQuad(V3{-A, 0, -A}, V3{-A, 0, A}, V3{-A, wh, A}, V3{-A, wh, -A}, steel, true) // -x
	}

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

// mapTris builds a map's static geometry into a fresh slice and returns it with
// the map's half-extent, WITHOUT disturbing the live arena globals (buildArena
// mutates package state the active match relies on). Used for the lobby/map
// previews, which render a different map than the one being played.
func mapTris(m gm.Map) (tris []Tri, size float64) {
	sa, ss := arena, arenaSize
	so, sr, srad := builtObstacles, builtRamps, radarRange
	sh := hideArenaWalls
	arena, hideArenaWalls = nil, false
	buildArena(m)
	tris, size = arena, arenaSize
	arena, arenaSize = sa, ss
	builtObstacles, builtRamps, radarRange = so, sr, srad
	hideArenaWalls = sh
	return
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
	noFog  bool // top-down tactical view: the whole floor sits in the fog band,
	// which muds the map - fatally so once the palette modes quantize it
}

func newRenderer(w, h int) *Renderer {
	return &Renderer{W: w, H: h, fb: make([]byte, w*h*3), zb: make([]float64, w*h),
		focal: float64(w) / 2, fogCol: [3]float64{0.04, 0.05, 0.09}}
}

// Resize re-fits the renderer's buffers to new pixel dimensions, in place so every
// holder of the *Renderer sees the change. Cheap: two slice allocations on resize.
func (r *Renderer) Resize(w, h int) {
	if w == r.W && h == r.H {
		return
	}
	r.W, r.H = w, h
	r.fb = make([]byte, w*h*3)
	r.zb = make([]float64, w*h)
	r.focal = float64(w) / 2
}

// pollResize keeps a render loop's dimensions in step with the live terminal. It
// asks the terminal for its size every ~1.5s (ESC[18t, written into w so it's
// serialized with the loop's own output - works over telnet, not just a TTY) and
// applies any reported change to *cols/*rows/*rows3d + the renderer. Returns true
// when the size changed this call, so the caller can force a full repaint.
func pollResize(w *bufio.Writer, ip *input, rnd *Renderer, cols, rows, rows3d *int, lastPoll *time.Time, now time.Time) bool {
	if now.Sub(*lastPoll) >= 1500*time.Millisecond {
		w.WriteString("\x1b[18t")
		*lastPoll = now
	}
	select {
	case sz := <-ip.resizeCh:
		if c, r := sz[0], sz[1]; c >= 20 && r >= 8 && (c != *cols || r != *rows) {
			*cols, *rows, *rows3d = c, r, r
			rnd.Resize(c, 2*r)
			return true
		}
	default:
	}
	return false
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

// clearBlack fills the framebuffer with black (vehicle-preview backdrop).
func (r *Renderer) clearBlack() {
	for i := range r.fb {
		r.fb[i] = 0
	}
	for i := range r.zb {
		r.zb[i] = 0
	}
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
	if r.noFog {
		return 0
	}
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
// half-block delta encoder (cp437 0xDF upper-half-block; colorMode picks the
// palette: truecolor SGRs, xterm-256, or classic dithered 16-color)
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
	mode := colorMode
	for y := 0; y < rows3d; y++ {
		for x := 0; x < cols; x++ {
			tp := ((2*y)*cols + x) * 3
			bp := ((2*y+1)*cols + x) * 3
			ci := (y*cols + x) * 6
			quantCell(mode, cur[ci:ci+6], r.fb, tp, bp, x, y)
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
			sgr, glyph := cellSGR(mode, cur[ci:ci+6])
			if sgr != lastSGR {
				b.WriteString("\x1b[")
				b.WriteString(sgr)
				b.WriteByte('m')
				lastSGR = sgr
			}
			b.WriteByte(glyph)
			curY, curX = y, x+1
		}
	}
	return []byte(b.String()), cur
}

// txBytes counts every byte that reaches the terminal socket, via the
// countWriter wrapped under the session's bufio.Writer. The info panel
// derives its TX rate from deltas of this.
var txBytes atomic.Int64

type countWriter struct{ w io.Writer }

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	txBytes.Add(int64(n))
	return n, err
}

// linkPace adapts frame output to what the terminal link can actually carry.
// Frames go out through blocking writes, so when the connection degrades
// (packet loss, saturated uplink) Flush stalls on TCP backpressure and the
// loop freezes inside the write - including the input check at the top, which
// is why a lagged demo also ignores "press any key". note() times each
// write+flush; on an overrun the next frames are skipped in proportion (the
// sim keeps running and input stays responsive) before probing the link with
// another frame. stalled() reports a sustained stall, for callers that would
// rather give up than limp along (the attract demo).
type linkPace struct {
	budget time.Duration // the loop's per-frame time slice
	skip   int           // frames left to skip before the next probe write
	skips  int           // lifetime frames skipped (info panel reads deltas)
	lastOK time.Time     // last write that completed at a watchable pace
}

// skipFrame reports whether the caller should skip rendering this frame.
func (lp *linkPace) skipFrame() bool {
	if lp.skip > 0 {
		lp.skip--
		lp.skips++
		return true
	}
	return false
}

// note records how long a frame's write+flush took. Anything within 2x budget
// counts as keeping up (a touch of jitter shouldn't throttle the render);
// past that, upcoming frames are skipped in proportion to the overrun, capped
// at ~2s so a long stall doesn't park the renderer once the link recovers.
func (lp *linkPace) note(d time.Duration) {
	if d <= lp.watchable() {
		// Skip-pacing is keeping the demo watchable, whatever the link is
		// doing underneath - that's stalled()'s healthy signal.
		lp.lastOK = time.Now()
	}
	if d <= lp.budget*2 {
		return
	}
	lp.skip = int(d / lp.budget)
	if lim := int(2 * time.Second / lp.budget); lp.skip > lim {
		lp.skip = lim
	}
}

// watchable is the write time under which a delivered frame still reads as a
// live picture (several budgets of lateness is fine - the skipper hides it).
func (lp *linkPace) watchable() time.Duration {
	if w := lp.budget * 6; w > 500*time.Millisecond {
		return w
	}
	return 500 * time.Millisecond
}

// stalled reports that the link is UNWATCHABLY behind: not one frame has gone
// out at a watchable pace for a sustained stretch. Mere throttling (skipped
// frames, late-but-delivered probes) never trips this - frame-skipping is the
// degradation strategy, and a limping-but-live demo is better than bailing.
// (The old cumulative-overrun counter ratcheted on slow probe frames and
// bailed demos that looked fine from the user's chair.)
func (lp *linkPace) stalled() bool {
	if lp.lastOK.IsZero() {
		lp.lastOK = time.Now() // arm on first check, not at construction time
		return false
	}
	return time.Since(lp.lastOK) > 25*time.Second
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
	mkBack // Backspace: exit a screen back to the previous menu (no program quit)
	mkEsc  // lone Esc while escMenu mode is on (in-game): opens the exit confirm
	mkChatToggle
	mkCmdToggle // '/': open the slash-command compose line
	mkTranscriptToggle
	mkChangeChar // 'v': swap your character (online, while dead or in the lobby)
	// Cruise-control keys (uppercase W/A/S/D, Q/E): latch auto-movement.
	mkCruiseF
	mkCruiseB
	mkCruiseL
	mkCruiseR
	mkCruiseSL
	mkCruiseSR
)

type input struct {
	mu       sync.Mutex
	last     [aCount]time.Time // indexed by action
	binds    map[byte]int      // remappable byte -> game action (see controls.go)
	any      time.Time         // last time any byte arrived (for "press any key")
	quitCh   chan struct{}
	quitOnce sync.Once      // guards close(quitCh): reader EOF and the idle watchdog can both fire
	events   chan menuKey   // discrete nav events for menus (buffered; drops if full)
	runes    chan rune      // printable chars (for editor text entry; buffered, best-effort)
	cpr      chan time.Time // cursor-position-report arrivals (the info panel's ping pong)
	resizeCh chan [2]int    // {cols,rows} from a terminal window report (ESC[8;h;w;t)
	winCols  int            // latest reported terminal size (guarded by mu); 0 = unknown
	winRows  int
	escMenu  bool // lone Esc emits mkEsc instead of quitting (set during matches)
}

// setEscMenu switches what a lone Esc does: false (default) quits the door,
// true emits an mkEsc event so the game can run an exit confirmation.
func (in *input) setEscMenu(v bool) {
	in.mu.Lock()
	in.escMenu = v
	in.mu.Unlock()
}

func (in *input) escAsKey() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.escMenu
}

// cprPong records a terminal CPR reply (ESC[row;colR) for the info panel's
// round-trip clock. Non-blocking: an unwatched pong is just dropped.
func (in *input) cprPong() {
	select {
	case in.cpr <- time.Now():
	default:
	}
}

// onWindowReport parses an xterm window size report (params of ESC[8;rows;cols;t)
// and hands the new dimensions to the render loop for a live resize. Anything
// that doesn't look like an "8" (size) report is ignored.
func (in *input) onWindowReport(params []byte) {
	f := strings.Split(string(params), ";")
	if len(f) != 3 || f[0] != "8" {
		return
	}
	rows, err1 := strconv.Atoi(f[1])
	cols, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil || cols < 20 || rows < 8 || cols > 1000 || rows > 1000 {
		return
	}
	in.mu.Lock()
	in.winCols, in.winRows = cols, rows
	in.mu.Unlock()
	// Replace any stale pending report with the latest, non-blocking.
	select {
	case <-in.resizeCh:
	default:
	}
	select {
	case in.resizeCh <- [2]int{cols, rows}:
	default:
	}
}

// winSize returns the latest terminal size from a window report (0,0 if none yet).
func (in *input) winSize() (int, int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.winCols, in.winRows
}

// signalQuit closes quitCh exactly once. Both the reader goroutine (on EOF/Esc/
// Ctrl-C) and the idle watchdog can request a quit; without the Once a second
// caller would panic on close-of-closed-channel.
func (in *input) signalQuit() { in.quitOnce.Do(func() { close(in.quitCh) }) }

// idleWatchdog disconnects a session that has gone silent for `limit`. This is the
// door's backstop against a half-open client: if the user vanished without a clean
// close, TCP keepalive (set in openTerm) usually trips the reader first, but if the
// reader is parked in a blocking Read that never returns, this fires, signals quit,
// and closes the term to unblock it - so the door exits and Synchronet frees the
// node. limit <= 0 disables the watchdog.
func (in *input) idleWatchdog(t Term, limit time.Duration) {
	if limit <= 0 {
		return
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-in.quitCh:
			return
		case <-tick.C:
			in.mu.Lock()
			idle := time.Since(in.any)
			in.mu.Unlock()
			if idle >= limit {
				logf("idle timeout: %s with no input; disconnecting", idle.Round(time.Second))
				in.signalQuit()
				t.Close() // unblock the reader's blocking Read on a dead/silent socket
				return
			}
		}
	}
}

// setBinds swaps the live key->action map (startup and CONTROLS edits).
func (in *input) setBinds(binds map[byte]int) {
	in.mu.Lock()
	in.binds = binds
	in.mu.Unlock()
}

func (in *input) bindLookup(c byte) (int, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	a, ok := in.binds[c]
	return a, ok
}

func (in *input) pushKey(k menuKey) {
	select {
	case in.events <- k:
	default: // menu not listening / buffer full: discrete events are best-effort
	}
}

func (in *input) pushRune(r rune) {
	select {
	case in.runes <- r:
	default: // not in a text field / buffer full: best-effort
	}
}

// drainRunes discards buffered typed chars (called when opening a text field so
// keystrokes from before it opened don't leak into the field).
func (in *input) drainRunes() {
	for {
		select {
		case <-in.runes:
		default:
			return
		}
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
	aFire2           // fire secondary weapon (B)
	aJump            // jump (ENTER)
	aRecenter        // snap turret to hull-forward + level (C)
	aAimUp           // elevate the gun (up arrow)
	aAimDown         // depress the gun (down arrow)
	aEdUp            // editor: fly camera up (R)
	aEdDown          // editor: fly camera down (F)
	aEdMenu          // editor: open the file menu (M)
	aEdSelect        // editor: enter select/edit mode (E)
	aEdDelete        // editor: delete the selected object (X)
	aEdGrid          // editor: cycle grid-snap size (G)
	aCruiseF         // latch cruise: forward
	aCruiseB         // latch cruise: back
	aCruiseL         // latch cruise: turn left
	aCruiseR         // latch cruise: turn right
	// New actions append here (action ids must stay stable: saved binds key off
	// these and the editor reuses the movement ones).
	aStrafeL  // sidestep left (Q)
	aStrafeR  // sidestep right (E)
	aCruiseSL // latch cruise: strafe left
	aCruiseSR // latch cruise: strafe right
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
		StrafeL:  in.held(aStrafeL),
		StrafeR:  in.held(aStrafeR),
		HullL:    in.held(aHullL),
		HullR:    in.held(aHullR),
		TurretL:  in.held(aTurretL),
		TurretR:  in.held(aTurretR),
		Fire:     in.held(aFire),
		Fire2:    in.held(aFire2),
		Jump:     in.held(aJump),
		Recenter: in.held(aRecenter),
		AimUp:    in.held(aAimUp),
		AimDown:  in.held(aAimDown),
		Vote:     -1, // overridden by the loop during the lobby
	}
}

// escTimeout: how long to wait for a byte after a lone Esc before deciding it's
// a quit (rather than the start of an arrow sequence whose bytes were split).
const escTimeout = 60 * time.Millisecond

// escNext returns the byte following an Esc / CSI intro: the next in-buffer byte
// if present (advancing i), else a brief timed read so a true lone Esc resolves
// to quit without waiting for the next keystroke.
func (in *input) escNext(t Term, buf []byte, n int, i *int) (byte, bool) {
	if *i+1 < n {
		*i++
		return buf[*i], true
	}
	var pb [1]byte
	if m, _ := t.ReadTimeout(pb[:], escTimeout); m > 0 {
		return pb[0], true
	}
	return 0, false
}

// csiFinal maps an arrow sequence's final byte to its aim action + menu event.
func (in *input) csiFinal(c byte) {
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
	case 'M': // SS3 numpad Enter (ESC O M) -> treat as Return, not a stray Esc
		in.pushKey(mkEnter)
	}
}

func (in *input) reader(t Term) {
	in.markAny() // seed the idle clock so the watchdog measures from connect, not zero-time
	buf := make([]byte, 16)
	iac := 0
	for {
		n, err := t.Read(buf)
		if err != nil || n == 0 {
			logf("reader exit (EOF/err): n=%d err=%v", n, err)
			in.signalQuit()
			return
		}
		in.markAny()
		for i := 0; i < n; i++ {
			c := buf[i]
			// Escape/control-sequence handling first; each consumes the byte.
			if iac > 0 {
				iac--
				continue
			}
			if c == 0xFF { // telnet IAC: skip the next two bytes
				iac = 2
				continue
			}
			if c == 27 { // Esc: an arrow/SS3 sequence (ESC [ x / ESC O x), or a lone Esc = quit
				nb, have := in.escNext(t, buf, n, &i)
				if have && (nb == '[' || nb == 'O') {
					fin, ok := in.escNext(t, buf, n, &i)
					var params []byte
					if nb == '[' { // collect CSI parameter bytes (e.g. a CPR ESC[24;80R or a window report ESC[8;h;w;t)
						for ok && fin >= '0' && fin <= ';' {
							params = append(params, fin)
							fin, ok = in.escNext(t, buf, n, &i)
						}
					}
					if ok {
						switch fin {
						case 'R': // cursor-position report: the info panel's ping reply
							in.cprPong()
						case 't': // xterm window report ESC[8;rows;cols;t -> live resize
							in.onWindowReport(params)
						default:
							in.csiFinal(fin)
						}
					}
					continue
				}
				if in.escAsKey() { // in a match: Esc opens the exit confirm instead
					in.pushKey(mkEsc)
					continue
				}
				logf("reader exit: Esc")
				in.signalQuit()
				return
			}
			// Normal key. Expose printable chars for editor text entry (map naming).
			if c >= 0x20 && c < 0x7f {
				in.pushRune(rune(c))
			}
			// Fixed menu navigation / cruise / special keys (not remappable). WASD
			// double as menu nav + cruise (uppercase); ENTER/Backspace/Tab/`/~ drive
			// menus. The game ACTION a key triggers comes from the bind map below.
			switch c {
			case 3: // Ctrl-C: hard abort (Esc is the normal quit)
				logf("reader exit: Ctrl-C")
				in.signalQuit()
				return
			case 'w': // wasd = fixed menu navigation (movement is via the bind map)
				in.pushKey(mkUp)
			case 's':
				in.pushKey(mkDown)
			case 'a':
				in.pushKey(mkLeft)
			case 'd':
				in.pushKey(mkRight)
			case '\r', '\n': // ENTER: menu confirm (and jump, via the bind map)
				in.pushKey(mkEnter)
			case '\t': // TAB: toggle top-down view
				in.pushKey(mkTab)
			case '`':
				in.pushKey(mkChatToggle)
			case '/':
				in.pushKey(mkCmdToggle)
			case '~':
				in.pushKey(mkTranscriptToggle)
			case 'v', 'V': // change character (online: while dead or in the lobby)
				in.pushKey(mkChangeChar)
			case 0x7f, 0x08: // Backspace: back / exit a screen
				in.pushKey(mkBack)
			}
			if a, ok := in.bindLookup(c); ok { // remappable in-game action
				if ck, isCruise := cruiseEvent[a]; isCruise {
					in.pushKey(ck) // cruise controls latch via a menu event
				} else {
					in.hit(a)
				}
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
func drawLeaderboard(w *bufio.Writer, cols, rows int, v viewState, recentKill map[int]float64) {
	koth := gm.RulesetFor(v.mode).Objective == gm.ObjZone && v.myTeam < 0 // FFA KotH scores by hold
	ranked := append([]gm.TankSnap(nil), v.tanks...)
	sort.Slice(ranked, func(a, b int) bool {
		if koth && ranked[a].HoldScore != ranked[b].HoldScore {
			return ranked[a].HoldScore > ranked[b].HoldScore
		}
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
		name := t.Name
		if name == "" {
			name = "BOT"
		}
		if len(name) > 12 {
			name = name[:12]
		}
		plus := "   "
		if recentKill[t.ID] > 0 { // just scored: a brief light-green +1
			plus = "\x1b[1;92m+1\x1b[0m "
		}
		ac := accentColor(t.Color) // accent swatch; the name carries the primary color
		stat := fmt.Sprintf("\x1b[0;37m%2d-%d", t.Kills, t.Deaths)
		if koth { // hold-points are the operative score
			stat = fmt.Sprintf("\x1b[1;93m%2dh\x1b[0;90m%d-%d", t.HoldScore, t.Kills, t.Deaths)
		}
		fmt.Fprintf(w, "\x1b[%d;1H%s%s\xdb\xdb %s \x1b[%sm%-12s\x1b[0m %s",
			3+i, mark,
			fgEsc(clampB(ac[0]*255), clampB(ac[1]*255), clampB(ac[2]*255)),
			stat, tankSGR(t.Color), name, plus)
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
	fillCol := fgEsc(70, 200, 90) // green
	switch {
	case p.Cloak:
		fillCol = fgEsc(180, 110, 220) // purple while cloaked
	case hp <= 30:
		fillCol = fgEsc(210, 70, 70) // red
	case hp <= 60:
		fillCol = fgEsc(220, 200, 60) // yellow
	}
	var b strings.Builder
	b.WriteString("\x1b[1;2H") // row 1, col 2 (top-left, in the sky)
	b.WriteString(fillCol)
	for i := 0; i < fill; i++ {
		b.WriteByte(0xDB) // full block
	}
	b.WriteString(fgEsc(70, 70, 70))
	for i := fill; i < barLen; i++ {
		b.WriteByte(0xB1) // medium-shade track
	}
	if p.Shield { // overshield: extend the bar with cyan blocks
		b.WriteString(fgEsc(90, 185, 230))
		for i := 0; i < 4; i++ {
			b.WriteByte(0xDB)
		}
	}
	if p.Cloak {
		b.WriteString(fgEsc(180, 110, 220) + " CLOAKED")
	}
	if p.Rapid {
		b.WriteString(fgEsc(230, 170, 40) + " RAPID")
	}
	if p.Shell {
		b.WriteString(fgEsc(90, 230, 190) + " SHELL")
	}
	if p.Poisoned {
		b.WriteString(fgEsc(120, 220, 60) + " POISON")
	}
	if p.Burning {
		b.WriteString(fgEsc(240, 130, 40) + " BURN")
	}
	if p.Bleeding {
		b.WriteString(fgEsc(235, 60, 60) + " BLEED")
	}
	// Ammo gauge on the same row, after the HP bar: drains as you fire, refills via
	// the vehicle's recharge.
	const ammoLen = 8
	af := int(p.Ammo*ammoLen + 0.5)
	if af > ammoLen {
		af = ammoLen
	}
	b.WriteString(fgEsc(220, 180, 60) + " \xb3") // separator
	for i := 0; i < af; i++ {
		b.WriteByte(0xDB)
	}
	b.WriteString(fgEsc(70, 70, 70))
	for i := af; i < ammoLen; i++ {
		b.WriteByte(0xB1)
	}
	// Minotaur barrier gauge: its own cyan bar after the ammo gauge - bright while
	// braced, dim while lowered/recovering, and flagged BROKEN on the cooldown.
	if p.Body == gm.BodyMinotaur {
		const shLen = 8
		sf := int(p.ShieldFrac*float64(shLen) + 0.5)
		if sf > shLen {
			sf = shLen
		}
		b.WriteString(fgEsc(120, 200, 230) + " \xb3")
		switch {
		case p.ShieldUp:
			b.WriteString(fgEsc(90, 230, 255) + "SHLD")
		case p.ShieldFrac <= 0:
			b.WriteString(fgEsc(225, 80, 80) + "BRKN")
		default:
			b.WriteString(fgEsc(80, 150, 185) + "shld")
		}
		barCol := fgEsc(70, 150, 190)
		if p.ShieldUp {
			barCol = fgEsc(90, 210, 255)
		}
		b.WriteString(barCol)
		for i := 0; i < sf; i++ {
			b.WriteByte(0xDB)
		}
		b.WriteString(fgEsc(70, 70, 70))
		for i := sf; i < shLen; i++ {
			b.WriteByte(0xB1)
		}
	}
	if p.Body == gm.BodyElephant { // passive regenerating shield buffer (ARMOR)
		const shLen = 8
		sf := int(p.ShieldFrac*float64(shLen) + 0.5)
		if sf > shLen {
			sf = shLen
		}
		b.WriteString(fgEsc(120, 200, 230) + " \xb3" + fgEsc(120, 200, 235) + "ARMR")
		b.WriteString(fgEsc(90, 185, 230))
		for i := 0; i < sf; i++ {
			b.WriteByte(0xDB)
		}
		b.WriteString(fgEsc(70, 70, 70))
		for i := sf; i < shLen; i++ {
			b.WriteByte(0xB1)
		}
	}
	// Secondary (B-key) gauge: the turtle SHELL and minotaur SHIELD already have
	// their own dedicated readouts above, so only draw this for palette-weapon
	// secondaries. Charge weapons (crab CLAW) show pips - one per stocked charge;
	// cooldown weapons show a short recharge bar (full = ready).
	if p.Body != gm.BodyTurtle && p.Body != gm.BodyMinotaur {
		name := gm.SecondaryWeaponName(p.Body)
		b.WriteString(fgEsc(170, 150, 220) + " \xb3" + name + " ")
		if p.MaxCharges > 0 { // charge-stock: pips, filled = available
			for i := 0; i < p.MaxCharges; i++ {
				if i < p.Charges {
					b.WriteString(fgEsc(180, 160, 235))
					b.WriteByte(0xDB) // full block = a ready charge
				} else {
					b.WriteString(fgEsc(70, 70, 70))
					b.WriteByte(0xB1) // shaded = spent/recharging
				}
			}
		} else { // cooldown: a short bar, full when ready (Reload2 0 = ready, 1 = just fired)
			const secLen = 6
			ready := int((1-p.Reload2)*secLen + 0.5)
			if ready > secLen {
				ready = secLen
			} else if ready < 0 {
				ready = 0
			}
			b.WriteString(fgEsc(150, 130, 215))
			for i := 0; i < ready; i++ {
				b.WriteByte(0xDB)
			}
			b.WriteString(fgEsc(70, 70, 70))
			for i := ready; i < secLen; i++ {
				b.WriteByte(0xB1)
			}
		}
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
	if r.Objective == gm.ObjNeutralFlags {
		fmt.Fprintf(&b, "  FLAGS %d/%d", v.flagsTotal-v.flagsLeft, v.flagsTotal)
	}
	if r.Teams == 2 { // team modes (CTF, Team KotH): RED vs BLU score
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
	if r.Objective == gm.ObjZone && r.Teams != 2 { // FFA King of the Hill: your hold-score
		fmt.Fprintf(&b, "  SCORE %d", p.HoldScore)
	}
	if r.Bots == gm.BotWaves {
		fmt.Fprintf(&b, "  WAVE %d", v.wave)
	}
	// Lives-based play: the base ruleset (survival, elimination) or a per-map /
	// campaign override (base Flag Run has no lives, so r.Lives misses those -
	// but the player snapshot only carries lives when the system is active).
	if r.Lives > 0 || p.Lives > 0 {
		lv := p.Lives
		if lv < 0 {
			lv = 0
		}
		fmt.Fprintf(&b, "  LIVES %d", lv)
	}
	b.WriteString("\x1b[0m")
	w.WriteString(b.String())
}

// drawDeathBanner is a COMPACT death header: a few centered lines pinned to the
// top so the death-cam replay behind it stays visible. (It used to stamp a full
// TheDraw block that swallowed most of the view.) The change-character hint sits
// on the last row, well clear of the replay.
func drawDeathBanner(w *bufio.Writer, cols, rows int, respawnIn float64, deathBy, hint string) {
	title := "*** DESTROYED ***"
	fmt.Fprintf(w, "\x1b[1;%dH\x1b[1;91m%s\x1b[0m", (cols-len(title))/2+1, title)
	row := 2
	if deathBy != "" { // who got you (kill feed)
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;91m%s\x1b[0m", row, (cols-len(clip(deathBy, cols-2)))/2+1, clip(deathBy, cols-2))
		row++
	}
	msg := fmt.Sprintf("respawning in %.0f", respawnIn+0.99)
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", row, (cols-len(msg))/2+1, msg)
	if hint != "" {
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;96m%s\x1b[0m", rows-1, (cols-len(hint))/2+1, hint)
	}
}

// drawKillBanner shows a transient "KILLED X" under the HP bar (top-center).
func drawKillBanner(w *bufio.Writer, cols int, text string) {
	fmt.Fprintf(w, "\x1b[3;%dH\x1b[1;92m%s\x1b[0m", (cols-len(text))/2+1, clip(text, cols-2))
}

// drawEscConfirm is the small in-match exit prompt (Esc): one line, top center
// on row 5 (under the kill banner and info strip), auto-dismissed after a few
// seconds if unanswered.
func drawEscConfirm(w *bufio.Writer, cols int) {
	const t = " LEAVE MATCH?  Y yes / N no "
	fmt.Fprintf(w, "\x1b[5;%dH\x1b[1;30;43m%s\x1b[0m", (cols-len(t))/2+1, t)
}

// infoPanel toggles the I-key diagnostics strip; package-level so the choice
// survives across matches within a session (not persisted).
var infoPanel bool

// drawInfoPanel is the diagnostics strip: one fixed-width dim line at row 4,
// centered - under the kill banner (row 3), clear of the leaderboard (left)
// and radar (right). Fields: rendered fps, terminal-bound bytes/sec, avg
// write+flush ms (link backpressure), frames skipped/sec (linkPace), and the
// terminal round-trip in ms via a cursor-position-report ping ("-" until a
// reply arrives; some terminals never answer).
func drawInfoPanel(w *bufio.Writer, cols, fps int, tx int64, wr time.Duration, sk int, rtt time.Duration) {
	txs := fmt.Sprintf("%dk", tx/1024)
	if tx >= 1<<20 {
		txs = fmt.Sprintf("%.1fm", float64(tx)/(1<<20))
	}
	rts := "   -"
	if rtt >= 0 {
		ms := rtt.Milliseconds()
		if ms > 9999 {
			ms = 9999
		}
		rts = fmt.Sprintf("%4d", ms)
	}
	wrMs := wr.Milliseconds()
	if wrMs > 999 {
		wrMs = 999
	}
	if fps > 99 {
		fps = 99
	}
	if sk > 99 {
		sk = 99
	}
	line := fmt.Sprintf("%2dfps tx%-5s wr%-3d sk%-2d rtt%sms", fps, txs, wrMs, sk, rts)
	fmt.Fprintf(w, "\x1b[4;%dH\x1b[0;90;40m%s\x1b[0m", (cols-len(line))/2+1, line)
}

// drawToast shows a transient author message (event system) centered near the top,
// bright yellow on black so it reads over the world.
func drawToast(w *bufio.Writer, cols int, text string) {
	text = clip(text, cols-4)
	fmt.Fprintf(w, "\x1b[2;%dH\x1b[1;33;40m %s \x1b[0m", (cols-len(text)-2)/2+1, text)
}

// tankName resolves a tank id to its display name (fallback for left/unknown).
func tankName(v viewState, id int) string {
	for i := range v.tanks {
		if v.tanks[i].ID == id {
			if v.tanks[i].Name != "" {
				return v.tanks[i].Name
			}
			break
		}
	}
	return "a tank"
}

// replayFrame is one recorded tick of the renderable world, for the death-cam replay.
type replayFrame struct {
	tanks   []gm.TankSnap
	shots   []gm.ShotSnap
	flags   []gm.FlagSnap
	pickups []gm.PickupSnap
	ents    []gm.EntitySnap
}

// posInSnap returns a tank's position within a snapshot by id (ok=false if absent).
func posInSnap(tanks []gm.TankSnap, id int) (gm.V3, bool) {
	if id < 0 {
		return gm.V3{}, false
	}
	for i := range tanks {
		if tanks[i].ID == id {
			return tanks[i].Pos, true
		}
	}
	return gm.V3{}, false
}

// deathText phrases who/what killed you for the death banner.
func deathText(v viewState, k gm.KillEvent) string {
	if k.Killer >= 0 {
		return tankName(v, k.Killer) + " killed you with " + k.Cause.Word()
	}
	return "Killed by " + k.Cause.Word()
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
		return fmt.Sprintf("live%t%t%t%t%t%t%t", p.Shield, p.Rapid, p.Cloak, p.Carrying, p.Shell, p.Poisoned, p.Burning)
	}
}

func phaseText(p gm.Phase) string {
	switch p {
	case gm.PhaseCountdown:
		return "countdown"
	case gm.PhaseActive:
		return "active"
	case gm.PhaseEnded:
		return "scoreboard"
	case gm.PhaseLobby:
		return "lobby"
	default:
		return "arena"
	}
}

func arenaMenuText(haveArena bool, dropfile string) (string, string, proto.ArenaStatus, bool) {
	onlineName := "ONLINE (?)"
	onlineBlurb := "Arena status unavailable."
	if !haveArena {
		return "ONLINE", "No arena server configured (ask the sysop).", proto.ArenaStatus{}, false
	}
	if st, err := arenaStatus(); err == nil {
		total := presenceCount(st.Presence)
		onlineName = fmt.Sprintf("ONLINE (%d)", total)
		arenaNoun := "players"
		if st.Humans == 1 {
			arenaNoun = "player"
		}
		others := presenceSummary(st.Presence, presenceSession(dropfile))
		onlineBlurb = fmt.Sprintf("%d %s in arena.%s %s on %s.", st.Humans, arenaNoun, others, st.Mode.String(), st.Map)
		if st.Phase != gm.PhaseActive {
			onlineBlurb = fmt.Sprintf("%d %s in arena.%s %s %s.", st.Humans, arenaNoun, others, st.Mode.String(), phaseText(st.Phase))
		}
		return onlineName, onlineBlurb, st, true
	} else {
		logf("arena status unavailable: %v", err)
		onlineBlurb = "Arena status unavailable: " + err.Error()
	}
	return onlineName, onlineBlurb, proto.ArenaStatus{}, false
}

func presenceCount(pres []proto.Presence) int {
	n := 0
	for _, rec := range pres {
		if rec.State != "offline" {
			n++
		}
	}
	return n
}

func presenceSummary(pres []proto.Presence, self string) string {
	var names []string
	for _, rec := range pres {
		if rec.Session == self || rec.State == "online arena" || rec.State == "offline" {
			continue
		}
		label := rec.Handle + " " + rec.State
		if rec.Detail != "" {
			label += " (" + rec.Detail + ")"
		}
		names = append(names, label)
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) > 2 {
		return fmt.Sprintf(" %d elsewhere in Spekder.", len(names))
	}
	return " " + strings.Join(names, "; ") + "."
}

// menuChoice is what the menu returns: quit, join the online arena, or a
// single-player mode.
type menuChoice struct {
	quit       bool
	relayout   bool // terminal resized: caller re-syncs size and re-enters the menu
	autoJoin   bool // a party-mate entered the arena: join it (skip the vehicle picker)
	online     bool
	party      bool
	campaign   bool
	options    bool
	help       bool
	highScores bool
	card       bool
	update     bool
	mode       gm.Mode
}

// runMenu shows the TDF-titled menu: single-player modes plus ONLINE ARENA
// (enabled only when the sysop configured a server). note (if any) is shown,
// e.g. a failed-connect message from a previous attempt.
// splashBin is the 80x25 CP437 title screen (Synchronet .bin: char/attr pairs,
// trailed by a SAUCE record we ignore) used as the main-menu backdrop.
//
//go:embed spekder.bin
var splashBin []byte

// cgaRGB is the 16-color CGA/EGA palette the .bin's attribute bytes index into.
var cgaRGB = [16][3]byte{
	{0, 0, 0}, {0, 0, 170}, {0, 170, 0}, {0, 170, 170},
	{170, 0, 0}, {170, 0, 170}, {170, 85, 0}, {170, 170, 170},
	{85, 85, 85}, {85, 85, 255}, {85, 255, 85}, {85, 255, 255},
	{255, 85, 85}, {255, 85, 255}, {255, 255, 85}, {255, 255, 255},
}

// drawBin paints an 80x25 CP437 screen (char/attr pairs) at (col0,row0), 1-based.
// Auto-wrap is disabled around it so writing the last column can't scroll.
func drawBin(w *bufio.Writer, data []byte, col0, row0 int) {
	const cw, chh = 80, 25
	w.WriteString("\x1b[?7l") // autowrap off
	lastSGR := ""
	for r := 0; r < chh; r++ {
		fmt.Fprintf(w, "\x1b[%d;%dH", row0+r+1, col0+1)
		for c := 0; c < cw; c++ {
			o := (r*cw + c) * 2
			if o+1 >= len(data) {
				break
			}
			glyph, attr := data[o], data[o+1]
			var sgr string
			if colorMode == colorTrue {
				fg, bg := cgaRGB[attr&0x0F], cgaRGB[(attr>>4)&0x07]
				sgr = fmt.Sprintf("38;2;%d;%d;%d;48;2;%d;%d;%d", fg[0], fg[1], fg[2], bg[0], bg[1], bg[2])
			} else {
				// The .bin is natively 16-color: emit its attributes as classic
				// SGRs - lossless, and works on any terminal.
				sgr = sgr16(attr&0x0F, (attr>>4)&0x07)
			}
			if sgr != lastSGR {
				w.WriteString("\x1b[")
				w.WriteString(sgr)
				w.WriteByte('m')
				lastSGR = sgr
			}
			if glyph == 0 {
				glyph = 0x20 // NUL renders as a space
			}
			w.WriteByte(glyph)
		}
	}
	w.WriteString("\x1b[0m\x1b[?7h") // reset + autowrap on
}

// runInfoScreen shows a titled, centered block of text until the player backs out
// (ENTER or Backspace; ESC quits the program as everywhere).
func runInfoScreen(w *bufio.Writer, cols, rows int, ip *input, title string, lines []string) {
	w.WriteString("\x1b[2J\x1b[H")
	fmt.Fprintf(w, "\x1b[1;%dH\x1b[1;96m%s\x1b[0m", (cols-len(title))/2+1, title)
	for i, l := range lines {
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", 3+i, (cols-len(l))/2+1, l)
	}
	foot := "ENTER / Bksp  back     ESC  quit"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows, (cols-len(foot))/2+1, foot)
	w.Flush()
	for {
		select {
		case <-ip.quitCh:
			return
		case k := <-ip.events:
			if k == mkEnter || k == mkBack {
				return
			}
		}
	}
}

// runHelp shows the multi-section help: controls, the roster, weapons, modes,
// solo/online play, custom maps and an about page. Left/right pages the
// sections; up/down scrolls a long one. See help.go for the copy.
func runHelp(w *bufio.Writer, cols, rows int, ip *input) {
	runPagedDoc(w, cols, rows, ip, "HELP", titleBlue, helpPages(cols), helpPages)
}

// runVersionInfo is the view-only VERSION INFO screen (from OPTIONS, or the
// menu's update alert): this build, the arena's version when known, and where
// to get a newer client if one was detected.
func runVersionInfo(w *bufio.Writer, cols, rows int, ip *input, dropfile string) {
	arena, latest, source, url := updateSnapshot()
	lines := []string{
		"This client:  spekder " + version,
		fmt.Sprintf("Protocol:     %d", proto.ProtocolVersion),
		"",
	}
	if arena != "" {
		lines = append(lines, "Arena server: spekder "+arena)
	} else if arenaConfigured() {
		lines = append(lines, "Arena server: configured (version unknown)")
	} else {
		lines = append(lines, "Arena server: none configured")
	}
	if avail, _ := updateAvailable(); avail {
		lines = append(lines,
			"",
			"** A newer client is available: spekder "+latest+" **",
			"   (reported by "+source+")",
			"   Get it: "+url,
			"",
			"Tell your sysop to update the door, then relaunch.")
	} else if version == "dev" {
		lines = append(lines, "", "Development build - update check disabled.")
	} else if latest != "" {
		lines = append(lines, "", "You are up to date (latest known: "+latest+").")
	} else {
		lines = append(lines, "", "No newer version detected (no network, or none published).")
	}
	runInfoScreen(w, cols, rows, ip, "VERSION INFO", lines)
}

// runParty is the PARTY screen: see who's online and which party they're in,
// START your own party (named after your handle) or LEAVE it, and JOIN another
// caller's party so a team mode drops you on the same side. Party identity rides
// the presence heartbeat, so a change propagates to everyone within a few
// seconds. One game per server means at most two parties (one per team).
func runParty(w *bufio.Writer, cols, rows int, ip *input, dropfile string) {
	_, myHandle := door32Identity(dropfile)
	mySession := presenceSession(dropfile)
	var pres []proto.Presence
	note := ""
	sel := 0 // 0 = the START/LEAVE action row; 1..len(pres) = a listed caller
	cmding := false
	cmdInput := ""

	refresh := func() {
		st, err := arenaStatus()
		if err != nil {
			return
		}
		syncPartyFromStatus(st.Presence, mySession) // reflect a kick the server applied
		pres = pres[:0]
		for _, p := range st.Presence {
			if p.Session == mySession || p.State == "offline" {
				continue
			}
			pres = append(pres, p)
		}
		// sel is clamped to the live action count in draw().
	}
	// announce sends our current party to the arena right away (don't wait for the
	// menu's 5s heartbeat) so others see the change promptly.
	announce := func() { updatePresence(dropfile, "party", "") }

	// totalParties counts the distinct parties in play (mine + everyone else's);
	// the arena caps the game at two. partyLists returns the parties you could
	// join - everyone else's, deduped, capped at two.
	totalParties := func() int {
		seen := map[string]bool{}
		if m := getParty(); m != "" {
			seen[m] = true
		}
		for _, p := range pres {
			if p.Party != "" {
				seen[p.Party] = true
			}
		}
		return len(seen)
	}
	partyLists := func() (joinable []string) {
		mine := getParty()
		seen := map[string]bool{}
		for _, p := range pres {
			if p.Party != "" && p.Party != mine && !seen[p.Party] {
				seen[p.Party] = true
				joinable = append(joinable, p.Party)
			}
		}
		sort.Strings(joinable)
		if len(joinable) > 2 {
			joinable = joinable[:2]
		}
		return
	}

	const partyTitle = "PARTY"
	titleCol := (cols-len(partyTitle))/2 + 1
	phase := 0
	// paintTitle repaints just the shimmering title row (the shimmer ticker).
	paintTitle := func() {
		fmt.Fprintf(w, "\x1b[1;%dH%s", titleCol, shimmerTitle(partyTitle, phase, titleMagenta))
		w.Flush()
	}

	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		fmt.Fprintf(w, "\x1b[1;%dH%s", titleCol, shimmerTitle(partyTitle, phase, titleMagenta))

		// What a party does, in practice.
		for i, s := range []string{
			"Team up across boards. In team modes the arena drops everyone in your",
			"party on the SAME side. Start a party (named after you) and have a friend",
			"join it - or join theirs. One game means at most two parties, one per team.",
		} {
			fmt.Fprintf(w, "\x1b[%d;4H\x1b[0;37m%s\x1b[0m", 3+i, s)
		}

		mine := getParty()
		yours := "(none - solo)"
		if mine != "" {
			yours = mine
			if mine == myHandle {
				yours += "  (you own it)"
			}
		}
		fmt.Fprintf(w, "\x1b[7;4H\x1b[0;37mYour party: \x1b[1;93m%s\x1b[0m", yours)

		// Action lightbar: START/LEAVE, then a JOIN entry per existing party. START
		// is disabled once two parties are already in play (you can only join then).
		joinable := partyLists()
		type opt struct {
			label   string
			enabled bool
		}
		var opts []opt
		switch {
		case mine != "":
			opts = append(opts, opt{"LEAVE PARTY", true})
		case totalParties() < 2:
			opts = append(opts, opt{"START A PARTY (named " + myHandle + ")", true})
		default:
			opts = append(opts, opt{"START A PARTY  -  two parties already, join one", false})
		}
		for _, pn := range joinable {
			opts = append(opts, opt{"JOIN " + pn + "'S PARTY", true})
		}
		if sel >= len(opts) {
			sel = len(opts) - 1
		}
		if sel < 0 {
			sel = 0
		}
		const actionTop = 9 // one blank row under "Your party"
		for i, o := range opts {
			style := "\x1b[0;36m"
			if !o.enabled {
				style = "\x1b[0;90m"
			}
			if i == sel {
				style = "\x1b[1;30;46m"
				if !o.enabled {
					style = "\x1b[0;1;30;47m"
				}
			}
			fmt.Fprintf(w, "\x1b[%d;4H%s  %-46s \x1b[0m", actionTop+i, style, o.label)
		}
		nextRow := actionTop + len(opts) + 1
		if mine != "" && mine == myHandle {
			fmt.Fprintf(w, "\x1b[%d;4H\x1b[0;90mOwner: boot a member with  \x1b[0;37m/kick <player>\x1b[0m", nextRow)
			nextRow++
		}

		// Online callers - view only (joining is driven by the JOIN action above).
		onlineTop := nextRow + 1
		fmt.Fprintf(w, "\x1b[%d;4H\x1b[1;95mONLINE NOW\x1b[0m", onlineTop)
		if len(pres) == 0 {
			fmt.Fprintf(w, "\x1b[%d;6H\x1b[0;90mNo other callers online right now.\x1b[0m", onlineTop+1)
		}
		for i, p := range pres {
			tag := ""
			if p.Party != "" {
				tag = "  [" + p.Party + "]"
			}
			fmt.Fprintf(w, "\x1b[%d;6H\x1b[0;37m%s@%s  \x1b[0;90m%s%s\x1b[0m", onlineTop+1+i, p.Handle, p.BBSID, p.State, tag)
		}
		if cmding { // slash-command line (same green prompt as elsewhere)
			fmt.Fprintf(w, "\x1b[%d;4H\x1b[1;30;42m /%-40s\x1b[0m", rows-1, cmdInput+"_")
		} else if note != "" {
			fmt.Fprintf(w, "\x1b[%d;4H\x1b[1;93m%s\x1b[0m", rows-1, note)
		}
		foot := "up/dn select   ENTER start/leave or join   / command   < back"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows, (cols-len(foot))/2+1, foot)
		w.Flush()
	}

	refresh()
	draw()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	shimmer := time.NewTicker(130 * time.Millisecond)
	defer shimmer.Stop()
	resizeTick := time.NewTicker(1500 * time.Millisecond)
	defer resizeTick.Stop()
	for {
		select {
		case <-ip.quitCh:
			return
		case <-shimmer.C:
			phase++
			if phase > len(partyTitle)+10 {
				phase = 0
			}
			paintTitle()
		case <-resizeTick.C:
			w.WriteString("\x1b[18t") // request a window report (telnet-safe live resize)
			w.Flush()
			if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
				cols, rows = c, r
				titleCol = (cols-len(partyTitle))/2 + 1
				draw()
			}
		case <-tick.C:
			refresh()
			draw()
		case r := <-ip.runes:
			if cmding {
				if r >= 0x20 && r < 0x7f && !(cmdInput == "" && r == '/') && len(cmdInput) < 80 {
					cmdInput += string(r)
					draw()
				}
			}
		case k := <-ip.events:
			joinable := partyLists()
			n := 1 + len(joinable) // START/LEAVE + one JOIN per existing party
			if cmding {            // command compose owns the keys until ENTER/back
				switch k {
				case mkEnter:
					note = dispatchCommand(cmdInput, dropfile)
					cmding, cmdInput = false, ""
					refresh()
					draw()
				case mkBack:
					if cmdInput == "" {
						cmding = false
					} else {
						cmdInput = cmdInput[:len(cmdInput)-1]
					}
					draw()
				}
				continue
			}
			switch k {
			case mkCmdToggle:
				cmding, cmdInput, note = true, "", ""
				draw()
			case mkUp:
				sel = (sel - 1 + n) % n
				draw()
			case mkDown:
				sel = (sel + 1) % n
				draw()
			case mkLeft, mkBack:
				return
			case mkEnter:
				note = ""
				if sel == 0 { // START / LEAVE
					if getParty() != "" {
						setParty("")
						announce()
					} else if totalParties() >= 2 {
						note = "Two parties already - join one of them instead."
					} else {
						setParty(myHandle)
						announce()
					}
				} else if sel-1 < len(joinable) { // JOIN the chosen party
					setParty(joinable[sel-1])
					announce()
				}
				refresh()
				draw()
			}
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runPlayerCard shows the caller's cumulative record (local stats; see
// SCORING.md): a styled card grouped by record / combat / career / bests /
// favorites, then a per-mode table. Zero-valued stats are omitted so the card
// only shows what the player has actually done.
func runPlayerCard(w *bufio.Writer, cols, rows int, ip *input, dropfile string) {
	st := loadStats(dropfile)
	if st.Games == 0 {
		runPagedDoc(w, cols, rows, ip, "PLAYER CARD", titleGreen, []docPage{{body: proseLines([]string{
			"No games played yet.",
			"",
			"Finish a match and your record shows up here - wins and losses, kills",
			"and accuracy, your best runs and your favorite characters.",
		}, cols-8, docTheme)}}, nil)
		return
	}
	ratio := func(a, b int) string {
		if b == 0 {
			return fmt.Sprintf("%.2f", float64(a))
		}
		return fmt.Sprintf("%.2f", float64(a)/float64(b))
	}
	secs := int(st.TimePlayed)

	var L []string
	L = append(L, "# RECORD",
		fmt.Sprintf("  `Games` **%d**     `Wins` **%d**     `Losses` **%d**     `W/L` **%s**",
			st.Games, st.Wins, st.Losses, ratio(st.Wins, st.Losses)))

	combat := fmt.Sprintf("  `Kills` **%d**     `Deaths` **%d**     `K/D` **%s**", st.Kills, st.Deaths, ratio(st.Kills, st.Deaths))
	if st.ShotsFired > 0 {
		combat += fmt.Sprintf("     `Accuracy` **%d%%**", st.ShotsHit*100/st.ShotsFired)
	}
	L = append(L, "", "# COMBAT", combat)

	career := fmt.Sprintf("  `Pickups` **%d**     `Time` **%dh %02dm**", st.Pickups, secs/3600, (secs%3600)/60)
	if st.DmgDealt > 0 {
		career += fmt.Sprintf("     `Damage` **%d**", st.DmgDealt)
	}
	if st.HealDone > 0 {
		career += fmt.Sprintf("     `Healed` **%d**", st.HealDone)
	}
	L = append(L, "", "# CAREER", career)

	bests := fmt.Sprintf("  `Best score` **%d**     `Total` **%d**", st.BestScore, st.TotalScore)
	if st.BestWave > 0 {
		bests += fmt.Sprintf("     `Best wave` **%d**", st.BestWave)
	}
	L = append(L, "", "# BESTS", bests)

	L = append(L, "", "# FAVORITES",
		fmt.Sprintf("  `Mode` **%s**     `Vehicle` **%s**", orDash(favoriteMode(&st)), orDash(favoriteVehicle(&st))))

	type mk struct {
		name string
		ms   *modeStat
	}
	var order []mk
	for k, v := range st.PerMode {
		order = append(order, mk{k, v})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].ms.Games > order[j].ms.Games })
	L = append(L, "", "# BY MODE",
		"`"+fmt.Sprintf("  %-12s %6s %6s %7s", "MODE", "GAMES", "WINS", "BEST")+"`")
	for _, e := range order {
		L = append(L, fmt.Sprintf("  %-12s %6d %6d %7d", clip(e.name, 12), e.ms.Games, e.ms.Wins, e.ms.Best))
	}
	runPagedDoc(w, cols, rows, ip, "PLAYER CARD", titleGreen, []docPage{{body: tableLines(L, docTheme)}}, nil)
}

// hsDateShort formats a leaderboard timestamp as "2 Jan 2006" (empty if unset).
func hsDateShort(when int64) string {
	if when <= 0 {
		return ""
	}
	return time.Unix(when, 0).Format("2 Jan 2006")
}

// hsRowColor tints a leaderboard row by placement: gold for the top run, bright
// white for the podium, plain white below.
func hsRowColor(rank int) string {
	switch {
	case rank == 0:
		return "\x1b[1;93m" // gold
	case rank < 3:
		return "\x1b[1;97m" // bright white
	default:
		return "\x1b[0;37m" // white
	}
}

// hsWho renders a leaderboard identity as handle@bbs, clipped to fit the wide
// PLAYER column (the full 80-col layout gives it room to breathe).
func hsWho(name, bbs string) string {
	if bbs == "" {
		return clip(name, 26)
	}
	return clip(name, 14) + "@" + clip(bbs, 11)
}

// Minimum activity before a player qualifies for the rate-based boards, so one
// lucky game can't top the K/D or accuracy charts.
const (
	minSkillGames = 5
	minSkillShots = 50
)

// careerBoardPages builds the global aggregate boards from per-player career rows:
// winningest (cumulative), K/D and accuracy (rate-based, activity-gated), and the
// deepest Survival wave. A board with no qualifying players is omitted.
func careerBoardPages(players []proto.PlayerRow) []docPage {
	const head = "\x1b[0;90m"
	kd := func(p proto.PlayerRow) float64 {
		if p.Deaths == 0 {
			return float64(p.Kills)
		}
		return float64(p.Kills) / float64(p.Deaths)
	}
	acc := func(p proto.PlayerRow) int {
		if p.ShotsFired == 0 {
			return 0
		}
		return p.ShotsHit * 100 / p.ShotsFired
	}
	// board assembles one page: filter to the eligible players, sort, render top 10.
	board := func(name, header string, ok func(proto.PlayerRow) bool, less func(a, b proto.PlayerRow) bool, row func(rank int, p proto.PlayerRow) string) (docPage, bool) {
		var elig []proto.PlayerRow
		for _, p := range players {
			if ok(p) {
				elig = append(elig, p)
			}
		}
		if len(elig) == 0 {
			return docPage{}, false
		}
		sort.SliceStable(elig, func(i, j int) bool { return less(elig[i], elig[j]) })
		body := []rline{styledLine(head, header)}
		for j, p := range elig {
			if j >= 10 {
				break
			}
			body = append(body, styledLine(hsRowColor(j), row(j, p)))
		}
		return docPage{name: name, body: body}, true
	}

	var pages []docPage
	if pg, ok := board("WINNINGEST",
		fmt.Sprintf("   %5s %6s %8s   %s", "WINS", "GAMES", "SCORE", "PLAYER"),
		func(p proto.PlayerRow) bool { return p.Games > 0 },
		func(a, b proto.PlayerRow) bool {
			if a.Wins != b.Wins {
				return a.Wins > b.Wins
			}
			return a.TotalScore > b.TotalScore
		},
		func(rank int, p proto.PlayerRow) string {
			return fmt.Sprintf(" %2d.%5d %6d %8d   %s", rank+1, p.Wins, p.Games, p.TotalScore, hsWho(p.Name, p.BBS))
		}); ok {
		pages = append(pages, pg)
	}
	if pg, ok := board("K/D",
		fmt.Sprintf("   %5s %7s %7s   %s", "K/D", "KILLS", "DEATHS", "PLAYER"),
		func(p proto.PlayerRow) bool { return p.Games >= minSkillGames },
		func(a, b proto.PlayerRow) bool { return kd(a) > kd(b) },
		func(rank int, p proto.PlayerRow) string {
			return fmt.Sprintf(" %2d. %5.2f %7d %7d   %s", rank+1, kd(p), p.Kills, p.Deaths, hsWho(p.Name, p.BBS))
		}); ok {
		pages = append(pages, pg)
	}
	if pg, ok := board("ACCURACY",
		fmt.Sprintf("   %5s %7s %7s   %s", "ACC", "HITS", "SHOTS", "PLAYER"),
		func(p proto.PlayerRow) bool { return p.ShotsFired >= minSkillShots },
		func(a, b proto.PlayerRow) bool { return acc(a) > acc(b) },
		func(rank int, p proto.PlayerRow) string {
			return fmt.Sprintf(" %2d.  %3d%% %7d %7d   %s", rank+1, acc(p), p.ShotsHit, p.ShotsFired, hsWho(p.Name, p.BBS))
		}); ok {
		pages = append(pages, pg)
	}
	if pg, ok := board("SURVIVAL WAVES",
		fmt.Sprintf("   %5s %6s   %s", "WAVE", "GAMES", "PLAYER"),
		func(p proto.PlayerRow) bool { return p.BestWave > 0 },
		func(a, b proto.PlayerRow) bool {
			if a.BestWave != b.BestWave {
				return a.BestWave > b.BestWave
			}
			return a.Games > b.Games
		},
		func(rank int, p proto.PlayerRow) string {
			return fmt.Sprintf(" %2d.%5d %6d   %s", rank+1, p.BestWave, p.Games, hsWho(p.Name, p.BBS))
		}); ok {
		pages = append(pages, pg)
	}
	return pages
}

// runHighScores shows one board per screen, paged left/right. With an arena it
// leads with the global career boards (winningest / K-D / accuracy / waves) then
// the per-mode single-match boards; offline it falls back to the caller's local
// per-mode tables (which carry the difficulty tier each run was set on).
func runHighScores(w *bufio.Writer, cols, rows int, ip *input, dropfile string) {
	const head = "\x1b[0;90m"
	title := "HIGH SCORES  (local)"
	var pages []docPage

	if arenaConfigured() { // prefer the global boards
		var careerPages, modePages []docPage
		if prows, err := queryGlobalPlayers(); err == nil {
			careerPages = careerBoardPages(prows)
		}
		if grows, err := queryGlobalScores(); err == nil && len(grows) > 0 {
			byMode := map[string][]proto.ScoreRow{}
			for _, r := range grows {
				byMode[r.Mode] = append(byMode[r.Mode], r)
			}
			for i := range gm.Rulesets {
				name := gm.Mode(i).String()
				list := byMode[name]
				if len(list) == 0 {
					continue
				}
				sort.SliceStable(list, func(a, b int) bool { return list[a].Score > list[b].Score })
				body := []rline{styledLine(head, fmt.Sprintf("    %6s  %-18s %-26s %s", "SCORE", "MAP", "PLAYER", "WHEN"))}
				for j, e := range list {
					if j >= 10 {
						break
					}
					body = append(body, styledLine(hsRowColor(j),
						fmt.Sprintf(" %2d.%6d  %-18s %-26s %s", j+1, e.Score, clip(e.Map, 18), hsWho(e.Name, e.BBS), hsDateShort(int64(e.When)))))
				}
				modePages = append(modePages, docPage{name: name + " best", body: body})
			}
		}
		pages = append(careerPages, modePages...)
		if len(pages) > 0 {
			title = "HIGH SCORES  (global)"
		}
	}

	if len(pages) == 0 { // no arena / unreachable / empty: the caller's local tables
		st := loadStats(dropfile)
		for i := range gm.Rulesets {
			name := gm.Mode(i).String()
			list := st.High[name]
			if len(list) == 0 {
				continue
			}
			body := []rline{styledLine(head, fmt.Sprintf("    %6s  %-18s %-10s %s", "SCORE", "MAP", "TIER", "WHEN"))}
			for j, e := range list {
				if j >= 10 {
					break
				}
				body = append(body, styledLine(hsRowColor(j),
					fmt.Sprintf(" %2d.%6d  %-18s %-10s %s", j+1, e.Score, clip(e.Map, 18), gm.Difficulty(e.Diff).String(), hsDateShort(e.When))))
			}
			pages = append(pages, docPage{name: name, body: body})
		}
	}

	if len(pages) == 0 {
		pages = []docPage{{body: proseLines([]string{
			"No high scores yet.",
			"",
			"Finish a match to set one - each mode keeps its own board, and harder",
			"difficulties are worth more.",
		}, cols-8, docTheme)}}
	}
	runPagedDoc(w, cols, rows, ip, title, titleRed, pages, nil)
}

// pickAliveBot returns a random living tank index, or -1 if all are down.
func pickAliveBot(tanks []gm.TankSnap) int {
	var alive []int
	for i := range tanks {
		if !tanks[i].Dead {
			alive = append(alive, i)
		}
	}
	if len(alive) == 0 {
		return -1
	}
	return alive[rand.Intn(len(alive))]
}

// tankSGR is a bright SGR body for a tank's color, in the current color mode.
func tankSGR(c [3]float64) string {
	r, g, b := clampB(c[0]*255), clampB(c[1]*255), clampB(c[2]*255)
	if colorMode == colorTrue {
		return fmt.Sprintf("1;38;2;%d;%d;%d", r, g, b)
	}
	return fgSGR(r, g, b)
}

// drawDemoScoreboard overlays a compact live standings panel (top-left) for the
// attract loop: mode + map, team scores, and the top fighters by frags - each row
// in the player's color, with brief +1 (green) / -1 (red) markers after a kill/death.
func drawDemoScoreboard(w *bufio.Writer, tanks []gm.TankSnap, m gm.MatchSnap, mapName string, killT, deathT map[int]float64) {
	pad := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s + strings.Repeat(" ", n-len(s))
	}
	rs := gm.RulesetFor(m.Mode)
	koth := rs.Objective == gm.ObjZone && rs.Teams != 2 // FFA King of the Hill: hold-points win
	row := 2
	fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;96m%s\x1b[0m", row, pad(rs.Name+" - "+mapName, 24))
	row++
	// Objective + clock line: show HOW the mode scores (so the demo teaches it).
	clock := ""
	if rs.TimeLimit > 0 {
		t := int(m.Timer)
		if t < 0 {
			t = 0
		}
		clock = fmt.Sprintf("%d:%02d", t/60, t%60)
	}
	switch {
	case rs.Teams == 2:
		fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;91mRED %-2d\x1b[0;37m \x1b[1;94mBLU %-2d\x1b[0;90m %s\x1b[0m     ", row, m.TeamScore[0], m.TeamScore[1], clock)
		row++
	case koth:
		fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;93mHILL \x1b[0;37mhold to %d\x1b[0;90m %s\x1b[0m   ", row, winTarget(rs), clock)
		row++
	case rs.Objective == gm.ObjNeutralFlags:
		fmt.Fprintf(w, "\x1b[%d;2H\x1b[0;37mFLAGS %d/%d\x1b[0;90m %s\x1b[0m       ", row, m.FlagsTotal-m.FlagsLeft, m.FlagsTotal, clock)
		row++
	case clock != "":
		fmt.Fprintf(w, "\x1b[%d;2H\x1b[0;37mFRAGS to %d\x1b[0;90m %s\x1b[0m        ", row, winTarget(rs), clock)
		row++
	}
	ranked := append([]gm.TankSnap(nil), tanks...)
	sort.Slice(ranked, func(a, b int) bool {
		if koth { // rank by the operative metric, not kills
			return ranked[a].HoldScore > ranked[b].HoldScore
		}
		return ranked[a].Kills > ranked[b].Kills
	})
	for i, t := range ranked {
		if i >= 5 {
			break
		}
		name := t.Name
		if name == "" {
			name = "BOT"
		}
		ac := accentColor(t.Color) // accent swatch; the name carries the primary color
		fmt.Fprintf(w, "\x1b[%d;2H\x1b[0;90m%d.\x1b[0m", row, i+1)
		fmt.Fprintf(w, "\x1b[%d;4H%s\xdb\xdb\x1b[0m", row,
			fgEsc(clampB(ac[0]*255), clampB(ac[1]*255), clampB(ac[2]*255)))
		fmt.Fprintf(w, "\x1b[%d;7H\x1b[%sm%-8.8s\x1b[0m", row, tankSGR(t.Color), name)
		if koth { // hold-points (operative) then small frags
			fmt.Fprintf(w, "\x1b[%d;16H\x1b[1;93m%2d\x1b[0;90mh\x1b[0;37m %d-%-2d\x1b[0m", row, t.HoldScore, t.Kills, t.Deaths)
		} else {
			fmt.Fprintf(w, "\x1b[%d;16H\x1b[0;37m%2d-%-2d   \x1b[0m", row, t.Kills, t.Deaths)
		}
		km, dm := "  ", "  " // fixed-width slots so a marker clears itself when it expires
		if killT[t.ID] > 0 {
			km = "+1"
		}
		if deathT[t.ID] > 0 {
			dm = "-1"
		}
		fmt.Fprintf(w, "\x1b[%d;25H\x1b[1;92m%s\x1b[0m \x1b[1;91m%s\x1b[0m", row, km, dm)
		row++
	}
}

// winTarget is the operative win count for a mode (frags/captures/hold-points).
func winTarget(rs gm.Ruleset) int {
	if len(rs.Win) > 0 {
		return rs.Win[0].Count
	}
	return 0
}

// botCountFor sizes the bot field by mode: team modes run ~4 a side (a healer +
// fighters per squad) - fewer than free-for-all, which reads better with squads.
// allBots=true means no human in the count (demo); false leaves a slot for you.
func botCountFor(mode gm.Mode, allBots bool) int {
	if gm.RulesetFor(mode).Teams == 2 {
		if allBots {
			return 8 // 4v4
		}
		return 7 // 4v4 including you
	}
	if allBots {
		return 6
	}
	return offlineBots
}

// runDemo is the attract loop: a local all-bot match rendered from a roaming chase
// camera, shown when the menu goes idle. It returns on the first keypress. No server
// needed - it works offline too; with an arena configured it also polls for chat
// (drawn as toasts over the action) and heartbeats presence so demo watchers stay
// on the server's who list. Returns true if it gave up because the link couldn't
// carry the frames (so the caller can hold off re-entering for a while), false on
// a keypress/quit.
func runDemo(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, chat *chatUI, dropfile string) bool {
	var modes []gm.Mode // non-Survival modes (Survival needs a player to end)
	for m := range gm.Rulesets {
		if gm.Rulesets[m].Bots != gm.BotWaves {
			modes = append(modes, gm.Mode(m))
		}
	}
	if len(modes) == 0 {
		modes = []gm.Mode{0}
	}
	// Each demo world is a fresh map + mode with freshly-rolled bot characters,
	// so the attract loop cycles through levels, modes, and the roster.
	// FLAG RUN is special: it simulates the campaign - a real numbered level,
	// one player stand-in collecting the flags against the level's authored,
	// non-respawning roster, camera locked on the protagonist. Clearing a level
	// advances the demo to the next one.
	campLevel := 0 // demo campaign progression
	campIdx := -1  // campaign level temporarily appended to gm.Maps (-1 = none)
	clearCamp := func() {
		if campIdx >= 0 && campIdx == len(gm.Maps)-1 {
			gm.Maps = gm.Maps[:campIdx]
		}
		campIdx = -1
	}
	defer clearCamp()
	randMode := func() gm.Mode { return modes[rand.Intn(len(modes))] }
	newWorld := func(mode gm.Mode) *gm.World {
		clearCamp()
		if mode == gm.ModeFlagRun && len(gm.CampaignMaps) > 0 {
			m := gm.CampaignMaps[campLevel%len(gm.CampaignMaps)]
			campIdx = len(gm.Maps) // the campaign runner's append-play-truncate pattern
			gm.Maps = append(gm.Maps, m)
			enemies := 3
			if m.Rules != nil && m.Rules.Bots >= 0 {
				enemies = m.Rules.Bots
			}
			wd := gm.NewWorld(enemies+1, mode)
			wd.PinMap(campIdx)
			wd.SetDemoHero(enemies) // the extra bot is the protagonist
			wd.SetDifficulty(gm.DiffNormal)
			wd.SetAimAssist(true)
			wd.SkipCountdown()
			return wd
		}
		wd := gm.NewWorld(botCountFor(mode, true), mode)
		wd.SetDifficulty(gm.DiffNormal)
		wd.SetAimAssist(true)
		wd.SkipCountdown() // open on action: a frozen count-in grid is a lousy attract
		return wd
	}
	// How long to linger on a map: deathmatch resolves fast, objective modes
	// (CTF/KOTH) need 60-90s to develop, and a campaign level sim gets room
	// to actually be beaten.
	capFor := func(mode gm.Mode) time.Duration {
		switch {
		case mode == gm.ModeFlagRun:
			return 120 * time.Second
		case gm.RulesetFor(mode).Objective == gm.ObjNone:
			return 30 * time.Second
		}
		return time.Duration(60+rand.Intn(31)) * time.Second
	}
	hideArenaWalls = true // a roaming chase cam shouldn't get blocked by the border
	defer func() { hideArenaWalls = false }()
	world := newWorld(randMode())
	noInput := map[int]gm.Input{}
	var endedAt, heroDeadAt time.Time // victory/defeat linger clocks

	var cam Cam
	var prev []byte
	curMapSig := "?"
	focus, segT, topdown := -1, 0.0, false
	// Per-tank (by ID) kill/death tracking, so the scoreboard can flash +1/-1.
	prevK, prevD := map[int]int{}, map[int]int{}
	killT, deathT := map[int]float64{}, map[int]float64{}
	const markerDur = 3.0
	start := time.Now()
	last := start
	worldStart := start
	worldCap := capFor(world.Match().Mode)
	budget := time.Second / 20
	pace := linkPace{budget: budget}
	// Arena poll: chat + presence, off-loop so a slow dial can't hitch the render.
	// The presence write doubles as a heartbeat - the server prunes sessions that
	// go quiet for 45s, and a demo watcher sends no other traffic.
	type pollResult struct {
		st proto.ArenaStatus
		ok bool
	}
	haveArena := arenaConfigured()
	pollCh := make(chan pollResult, 1)
	pollBusy := false
	var lastPoll time.Time
	chatSig := ""
	lastSizePoll := time.Time{} // live terminal-resize polling
	for {
		select { // any input wakes from the demo
		case <-ip.quitCh:
			return false
		case <-ip.events:
			return false
		case <-ip.runes:
			return false
		default:
		}
		if haveArena && !pollBusy && time.Since(lastPoll) >= 5*time.Second {
			pollBusy = true
			lastPoll = time.Now()
			go func() {
				updatePresence(dropfile, "demo", "")
				st, err := arenaStatus()
				pollCh <- pollResult{st, err == nil}
			}()
		}
		select {
		case p := <-pollCh:
			pollBusy = false
			if p.ok {
				chat.ingest(p.st.Chat)
				chat.setWho(p.st.Presence)
			}
		default:
		}
		chat.prune()
		if sig := fmt.Sprintf("%d:%d", len(chat.toasts), chat.seenSeq); sig != chatSig {
			chatSig, prev = sig, nil // toast set changed: repaint so stale text clears
		}
		now := time.Now()
		if pollResize(w, ip, rnd, &cols, &rows, &rows3d, &lastSizePoll, now) {
			prev = nil      // full re-encode at the new size
			curMapSig = "?" // rebuild the arena geometry buffer
		}
		dt := now.Sub(last).Seconds()
		last = now
		if dt > 0.1 {
			dt = 0.1
		}
		world.Update(dt, noInput)
		// Cycle to a new scene when the match ends (after a short beat so the
		// outcome reads), the protagonist falls, or the cap expires. A cleared
		// campaign level advances the demo to the next level.
		ms := world.Match()
		if ms.Phase == gm.PhaseEnded && endedAt.IsZero() {
			endedAt = now
		}
		heroWon := world.DemoHero() >= 0 && ms.Phase == gm.PhaseEnded && ms.FlagsTotal > 0 && ms.FlagsLeft == 0
		if (!endedAt.IsZero() && now.Sub(endedAt) > 3*time.Second) ||
			(!heroDeadAt.IsZero() && now.Sub(heroDeadAt) > 3*time.Second) ||
			now.Sub(worldStart) > worldCap {
			next := randMode()
			if heroWon {
				campLevel++ // the stand-in cleared it: straight to the next level
				next = gm.ModeFlagRun
			}
			world = newWorld(next)
			worldStart, worldCap = now, capFor(world.Match().Mode)
			endedAt, heroDeadAt = time.Time{}, time.Time{}
			focus, segT, prev = -1, 0, nil
			prevK, prevD = map[int]int{}, map[int]int{}
			killT, deathT = map[int]float64{}, map[int]float64{}
			continue
		}
		if pace.skipFrame() { // link saturated: keep the match running, skip drawing
			if d := budget - time.Since(now); d > 0 {
				time.Sleep(d)
			}
			continue
		}
		tanks, shots, flags, pickups := world.Snapshot()
		for i := range tanks { // flash +1/-1 when a tank's kills/deaths tick up
			id := tanks[i].ID
			if pk, ok := prevK[id]; ok && tanks[i].Kills > pk {
				killT[id] = markerDur
			}
			if pd, ok := prevD[id]; ok && tanks[i].Deaths > pd {
				deathT[id] = markerDur
			}
			prevK[id], prevD[id] = tanks[i].Kills, tanks[i].Deaths
		}
		for id := range killT {
			killT[id] -= dt
		}
		for id := range deathT {
			deathT[id] -= dt
		}
		ents, zones := world.Entities(), world.Zones()
		gmap := world.ActiveMap()
		if sig := fmt.Sprintf("%s/%.0f/%d", gmap.Name, gmap.Size, len(gmap.Obstacles)); sig != curMapSig {
			buildArena(gmap)
			curMapSig, prev = sig, nil
		}
		// Camera: a campaign sim locks onto the protagonist (this is "watching
		// someone play the level"); other modes pick a fresh shot when the
		// segment timer runs out or the followed tank dies. Top-down at most
		// ~20% of roaming segments; the rest 3D chase.
		heroIdx := -1
		if h := world.DemoHero(); h >= 0 {
			for i := range tanks {
				if tanks[i].ID == h {
					heroIdx = i
					break
				}
			}
		}
		if heroIdx >= 0 && tanks[heroIdx].Dead && heroDeadAt.IsZero() {
			heroDeadAt = now // the stand-in is down: linger a beat, then move on
		}
		segDead := focus >= 0 && (focus >= len(tanks) || tanks[focus].Dead)
		segT -= dt
		switch {
		case heroIdx >= 0:
			focus, topdown = heroIdx, false
		case segT <= 0 || segDead:
			topdown = rand.Float64() < 0.2
			focus, segT = pickAliveBot(tanks), 5+rand.Float64()*3
			prev = nil // mode switch: force a clean repaint
		}
		switch {
		case topdown: // overhead tactical view, looking straight down
			a := arenaSize + 2
			camY := a
			if r := float64(rnd.W) / float64(rnd.H); r > 1 {
				camY = a * r
			}
			cam.pos = V3{0, camY, 0}
			cam.yaw, cam.pitch = 0, math.Pi/2
		case focus >= 0: // chase the focused fighter from behind + above
			ft := tanks[focus]
			sy, cy := math.Sin(ft.HullYaw), math.Cos(ft.HullYaw)
			cam.pos = V3{ft.Pos.X - sy*6.5, ft.Pos.Y + 3.4, ft.Pos.Z - cy*6.5}
			cam.yaw, cam.pitch = ft.HullYaw, 0.32
		default: // everyone down between matches: slow overhead orbit
			cam.pos = V3{0, arenaSize, 0}
			cam.yaw, cam.pitch = now.Sub(start).Seconds()*0.3, 1.2
		}
		rnd.renderWorld(cam, now.Sub(start).Seconds(), tanks, shots, flags, pickups, gmap.Entities, ents, zones, -1, 0, topdown, false, [3]byte{}, 0)
		frame, cur := encode(rnd, cols, rows3d, prev)
		prev = cur
		wstart := time.Now()
		w.Write(frame)
		t := "*  D E M O  *"
		fmt.Fprintf(w, "\x1b[1;%dH\x1b[1;33;40m %s \x1b[0m", (cols-len(t))/2, t)
		drawDemoScoreboard(w, tanks, world.Match(), gmap.Name, killT, deathT)
		if world.DemoHero() >= 0 { // campaign sim: announce the run's outcome
			switch {
			case heroWon:
				t := ">>  LEVEL CLEAR  <<"
				fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;30;42m %s \x1b[0m", rows/2, (cols-len(t)-2)/2+1, t)
			case !heroDeadAt.IsZero():
				t := ">>  MISSION FAILED  <<"
				fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;37;41m %s \x1b[0m", rows/2, (cols-len(t)-2)/2+1, t)
			}
		}
		h := "press any key"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90;40m %s \x1b[0m", rows, (cols-len(h))/2, h)
		drawChat(w, cols, rows, chat, nil) // arena chat rides over the demo as toasts
		w.Flush()
		pace.note(time.Since(wstart))
		if pace.stalled() { // no point burning a saturated link on a screensaver
			logf("demo: link can't keep up with the attract loop; back to the menu")
			return true
		}
		if d := budget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
}

// spModeColors returns (base, arrow) SGR params for a single-player mode: a
// distinct bright color per mode for the shimmering name, plus an accent for
// the < > cycle arrows that complements it without blending in.
func spModeColors(mode int) (string, string) {
	pairs := [][2]string{
		{"1;36", "1;35"}, // cyan name, lightmagenta arrows
		{"1;32", "1;33"}, // green, yellow
		{"1;31", "1;36"}, // red, cyan
		{"1;33", "1;35"}, // yellow, lightmagenta
		{"1;35", "1;32"}, // magenta, green
		{"1;34", "1;31"}, // blue, red
	}
	p := pairs[mode%len(pairs)]
	return p[0], p[1]
}

// shimmerText renders s in baseSGR with a 2-char bright-white window sweeping
// left to right (position from phase), pausing briefly between sweeps.
func shimmerText(s, baseSGR string, phase int) string {
	var b strings.Builder
	p := phase % (len(s) + 6) // +6: a beat of rest between sweeps
	cur := ""
	for i := 0; i < len(s); i++ {
		sgr := baseSGR
		if i == p || i == p-1 {
			sgr = "1;37" // the gleam
		}
		if sgr != cur {
			b.WriteString("\x1b[" + sgr + "m")
			cur = sgr
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// drawSPMode overdraws the single-player blurb's "< NAME >" prefix in its
// per-mode colors with the shimmer sweep. Same characters as the plain text
// it covers, so the layout (and wrapText's line break) is unchanged.
func drawSPMode(w *bufio.Writer, x, y int, name string, mode, phase int) {
	base, arrow := spModeColors(mode)
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[%s;40m<\x1b[0m ", y, x, arrow)
	w.WriteString(shimmerText(name, base, phase))
	fmt.Fprintf(w, "\x1b[0m \x1b[%s;40m>\x1b[0m", arrow)
}

func runMenu(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, note, dropfile string) menuChoice {
	type entry struct {
		name, blurb                                                          string
		single, online, party, options, help, highScores, card, quit, update bool
		ready                                                                bool
	}
	haveArena := arenaConfigured()
	updatePresence(dropfile, "menu", "")
	chat := &chatUI{}
	onlineName, onlineBlurb, st, ok := arenaMenuText(haveArena, dropfile)
	if ok {
		chat.ingest(st.Chat)
		chat.setWho(st.Presence)
		noteArenaVersion(st.ServerVersion, st.LatestClient)
		syncPartyFromStatus(st.Presence, presenceSession(dropfile))
		// Party already in the arena when we land on the menu: follow them in.
		if mate := partyMateInArena(st.Presence, presenceSession(dropfile)); !mate {
			autoJoinArmed = true
		} else if autoJoinArmed {
			return menuChoice{autoJoin: true}
		}
	}
	// Single-player collapses the whole ruleset table into one entry; left/right
	// cycle the mode (spMode), so adding a mode to gm.Rulesets still just appears.
	// In the single-player cycler, FLAG RUN is the campaign (numbered levels,
	// life pool) - the original game's flag mode, not a one-off quick match.
	campBlurb := "Numbered levels, 3 lives, +1 per clear. Enemies stay dead."
	if best := loadUserSettings(dropfile).campaignBest; best > 0 {
		campBlurb = fmt.Sprintf("Numbered levels, 3 lives, +1 per clear. Best: level %d.", best)
	}
	items := []entry{
		{name: "SINGLE PLAYER", single: true, ready: true},
		{name: onlineName, blurb: onlineBlurb, online: true, ready: haveArena},
		{name: "PARTY", blurb: "Team up with other callers so a team mode puts you on the same side.", party: true, ready: haveArena},
		{name: "HIGH SCORES", blurb: "Top runs by mode (local; global when an arena is set).", highScores: true, ready: true},
		{name: "PLAYER CARD", blurb: "Your record: W/L, K/D, favorites, time played.", card: true, ready: true},
		{name: "OPTIONS", blurb: "Difficulty, map editor, and controls.", options: true, ready: true},
		{name: "HELP", blurb: "Controls and how to play.", help: true, ready: true},
		{name: "EXIT/QUIT", blurb: "Leave the game and return to the BBS.", quit: true, ready: true},
	}
	// A newer client release: a bright alert row at the top of the menu (the
	// GitHub probe / arena report feed updateAvailable; it shows up the moment
	// it's detected, on this or the next menu entry).
	if avail, latest := updateAvailable(); avail {
		items = append([]entry{{
			name:   "UPDATE AVAILABLE",
			blurb:  "spekder " + latest + " is out - ENTER for where to get it.",
			update: true, ready: true,
		}}, items...)
	}
	onlineIdx, spMode := 0, int(gm.ModeFlagRun) // FLAG RUN (the campaign) is the default mode
	sel := 0
	for i := range items { // the ONLINE row (refreshOnline rewrites its name/blurb)
		if items[i].online {
			onlineIdx = i
		}
		if items[i].single { // default the cursor to SINGLE PLAYER, not the alert row
			sel = i
		}
	}
	// The 80x25 .bin art is the backdrop; the menu rides on the right over it. If
	// the terminal is wider, center the art (top-aligned); taller, push the blurb
	// and footer into the space below the art.
	artCol0 := 0
	if cols > 80 {
		artCol0 = (cols - 80) / 2
	}
	const menuW, descW = 16, 34
	menuCol := artCol0 + 80 - menuW - 1 // right edge of the art, slim margin
	menuTop := 5
	descCol := artCol0 + 80 - descW
	blurbRow := menuTop + len(items) + 1
	shimmerPhase := 0 // animation clock for the single-player mode selector
	drawOptions := func() {
		for i, it := range items {
			label := it.name
			if !it.ready {
				label += " *"
			}
			var style string
			switch {
			case i == sel:
				style = "\x1b[1;30;46m" // selected: black on cyan
			case it.update:
				style = "\x1b[1;93;41m" // update alert: bright yellow on red
			case !it.ready:
				style = "\x1b[0;90;40m" // unavailable: dim on black
			case it.online:
				style = "\x1b[1;32;40m" // online available: green on black
			default:
				style = "\x1b[0;37;40m" // single-player: white on black
			}
			fmt.Fprintf(w, "\x1b[%d;%dH%s %-*s\x1b[0m", menuTop+i, menuCol, style, menuW-1, label)
		}
		// Blurb (2 lines) + note, right-aligned on a black strip so they read over art.
		blurb := items[sel].blurb
		if items[sel].single { // show + cycle the chosen single-player mode
			rs := gm.Rulesets[spMode]
			desc := rs.Desc
			if gm.Mode(spMode) == gm.ModeFlagRun {
				desc = campBlurb // FLAG RUN is the campaign
			}
			blurb = "< " + rs.Name + " >  " + desc
		}
		bl := wrapText(blurb, descW)
		for j := 0; j < 2; j++ {
			line := ""
			if j < len(bl) {
				line = bl[j]
			}
			fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37;40m%-*s\x1b[0m", blurbRow+j, descCol, descW, line)
		}
		if items[sel].single { // recolor the mode selector over the plain line
			drawSPMode(w, descCol, blurbRow, gm.Rulesets[spMode].Name, spMode, shimmerPhase)
		}
		noteTxt := ""
		if note != "" {
			noteTxt = note
		}
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;31;40m%-*s\x1b[0m", blurbRow+3, descCol, descW, noteTxt)
		drawChat(w, cols, rows, chat, nil)
		w.Flush()
	}
	draw := func() { // full repaint: backdrop + menu (used on entry / after submenus)
		w.WriteString("\x1b[2J\x1b[H")
		drawBin(w, splashBin, artCol0, 0)
		drawOptions()
	}
	mySession := presenceSession(dropfile)
	followParty := false // set when a party-mate is in the arena and we should follow
	refreshOnline := func() {
		updatePresence(dropfile, "menu", "")
		name, blurb, st, ok := arenaMenuText(haveArena, dropfile)
		if ok {
			chat.ingest(st.Chat)
			chat.setWho(st.Presence)
			noteArenaVersion(st.ServerVersion, st.LatestClient)
			syncPartyFromStatus(st.Presence, mySession)
			// Follow the party into the arena: re-arm once they're not in it, and
			// fire when armed + a mate is in (so leaving doesn't yank you back).
			if mate := partyMateInArena(st.Presence, mySession); !mate {
				autoJoinArmed = true
			} else if autoJoinArmed {
				followParty = true
			}
		}
		if items[onlineIdx].name == name && items[onlineIdx].blurb == blurb {
			drawOptions()
			return
		}
		items[onlineIdx].name = name
		items[onlineIdx].blurb = blurb
		drawOptions()
	}
	draw()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	chatTick := time.NewTicker(1 * time.Second)
	defer chatTick.Stop()
	animTick := time.NewTicker(120 * time.Millisecond) // mode-selector shimmer
	defer animTick.Stop()
	resizeTick := time.NewTicker(1500 * time.Millisecond) // live terminal-resize polling
	defer resizeTick.Stop()
	const demoAfter = 30 * time.Second
	demoDelay := demoAfter
	lastActive := time.Now()
	for {
		select {
		case <-ip.quitCh:
			return menuChoice{quit: true}
		case <-tick.C:
			if !chat.active && !chat.transcript && time.Since(lastActive) > demoDelay {
				// Attract mode; returns on a keypress. If it bailed because the
				// link couldn't carry the frames, hold off a good while before
				// trying again instead of re-saturating the connection every 30s.
				if runDemo(w, cols, rows, rows3d, rnd, ip, chat, dropfile) {
					demoDelay = 5 * time.Minute
				} else {
					demoDelay = demoAfter
				}
				lastActive = time.Now()
				draw()
				continue
			}
			refreshOnline()
			if followParty {
				return menuChoice{autoJoin: true}
			}
		case <-chatTick.C:
			if chat.prune() {
				draw()
			}
		case <-resizeTick.C:
			w.WriteString("\x1b[18t") // ask the terminal for its size (telnet-safe)
			w.Flush()
			if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
				return menuChoice{relayout: true} // re-enter the menu at the new size
			}
		case <-animTick.C:
			// Animate just the selector line (a few dozen bytes); skip while the
			// transcript box overlays the center of the screen.
			if items[sel].single && !chat.transcript {
				shimmerPhase++
				drawSPMode(w, descCol, blurbRow, gm.Rulesets[spMode].Name, spMode, shimmerPhase)
				w.Flush()
			}
		case k := <-ip.events:
			lastActive = time.Now()
			if chat.active {
				switch k {
				case mkChatToggle:
					chat.active, chat.cmd = false, false
					chat.input = ""
					draw()
				case mkTranscriptToggle:
					chat.transcript = !chat.transcript
					draw()
				case mkEnter:
					if msg := chat.submit(dropfile); msg != "" {
						note = msg
					}
					refreshOnline()
					draw()
				case mkBack:
					chat.backspace()
					drawOptions()
				}
				continue
			}
			if chat.transcript {
				switch k {
				case mkTranscriptToggle:
					chat.transcript = false
					draw()
				case mkChatToggle:
					chat.active = true
					draw()
				}
				continue
			}
			switch k {
			case mkChatToggle:
				chat.active = !chat.active
				chat.cmd = false
				if !chat.active {
					chat.input = ""
				}
				draw()
			case mkCmdToggle:
				chat.active, chat.cmd, chat.input = true, true, ""
				draw()
			case mkTranscriptToggle:
				chat.transcript = !chat.transcript
				draw()
			case mkUp:
				sel = (sel - 1 + len(items)) % len(items)
				note = ""
				drawOptions() // overlay only; backdrop stays
			case mkDown:
				sel = (sel + 1) % len(items)
				note = ""
				drawOptions()
			case mkLeft:
				if items[sel].single {
					spMode = (spMode - 1 + len(gm.Rulesets)) % len(gm.Rulesets)
					drawOptions()
				}
			case mkRight:
				if items[sel].single {
					spMode = (spMode + 1) % len(gm.Rulesets)
					drawOptions()
				}
			case mkEnter:
				it := items[sel]
				if !it.ready {
					if it.online {
						note = "No arena server configured."
					} else {
						note = it.name + " is coming soon."
					}
					drawOptions()
					continue
				}
				if it.quit { // same exit path as ESC, for anyone who misses the footer
					return menuChoice{quit: true}
				}
				if it.update { // open the version/download info screen
					return menuChoice{update: true}
				}
				mc := menuChoice{
					online: it.online, party: it.party, options: it.options, help: it.help,
					highScores: it.highScores, card: it.card,
				}
				if it.single {
					if gm.Mode(spMode) == gm.ModeFlagRun {
						mc.campaign = true // FLAG RUN is the campaign
					} else {
						mc.mode = gm.Mode(spMode)
					}
				}
				return mc
			}
		case r := <-ip.runes:
			lastActive = time.Now()
			if chat.active {
				chat.appendRune(r)
				drawOptions()
			}
		}
	}
}

// drawListMenu renders a centered title + vertical item list with a selection
// marker and a blurb line, shared by the OPTIONS sub-menus.
func drawListMenu(w *bufio.Writer, cols, rows int, title string, names, blurbs []string, dim []bool, sel int) {
	w.WriteString("\x1b[2J\x1b[H")
	if f, ok := tdf.Fit(title, cols-2, "union", "untx", "block"); ok {
		w.WriteString(f.RenderCentered(title, cols, 1, tdf.RenderOpts{Recolor: true, FG: 11, Transparent: true}))
	}
	row := rows/2 - len(names)/2 - 1
	for i, nm := range names {
		marker, style := "  ", "\x1b[0;36m"
		if dim != nil && dim[i] {
			style = "\x1b[0;90m"
			nm += "  (soon)"
		}
		if i == sel {
			style = "\x1b[1;30;46m"
		}
		line := marker + nm
		fmt.Fprintf(w, "\x1b[%d;%dH%s  %s  \x1b[0m", row+i, (cols-len(line)-4)/2+1, style, line)
	}
	if sel >= 0 && sel < len(blurbs) {
		b := blurbs[sel]
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", row+len(names)+2, (cols-len(b))/2+1, b)
	}
	foot := "up/down  select     ENTER  choose     </ back"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows-1, (cols-len(foot))/2+1, foot)
	w.Flush()
}

// runOptions is the OPTIONS sub-menu: Difficulty + Aim Assist (live), Map Editor
// + Controls (coming soon). Mutates and persists the user's settings in place.
func runOptions(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, dropfile string, s *userSettings) {
	const (
		iDiff = iota
		iAssist
		iColor
		iEditor
		iControls
		iVersion
		iBack
	)
	names := []string{"DIFFICULTY", "AIM ASSIST", "COLOR", "MAP EDITOR", "CONTROLS", "VERSION INFO", "BACK"}
	blurbs := []string{"", "", "", "Build and edit arenas.", "Remap your key bindings.", "Your version, the arena's, and any update.", "Return to the main menu."}
	dim := []bool{false, false, false, false, false, false, false}
	onOff := func(b bool) string {
		if b {
			return "ON"
		}
		return "OFF"
	}
	sel := 0
	draw := func() {
		names[iDiff] = "DIFFICULTY: " + s.difficulty.String()
		names[iAssist] = "AIM ASSIST: " + onOff(s.aimAssist)
		names[iColor] = "COLOR: " + colorModeName(s.colorMode)
		blurbs[iDiff] = "Bot skill level."
		blurbs[iAssist] = "Sticky aim: ease onto a target your reticle is near."
		blurbs[iColor] = "TRUECOLOR looks best; 256 is lighter on slow links; 16 suits classic terminals."
		drawListMenu(w, cols, rows, "OPTIONS", names, blurbs, dim, sel)
	}
	draw()
	resizeTick := time.NewTicker(1500 * time.Millisecond)
	defer resizeTick.Stop()
	for {
		select {
		case <-ip.quitCh:
			return // propagate quit; the main menu will exit
		case <-resizeTick.C:
			w.WriteString("\x1b[18t")
			w.Flush()
			if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
				cols, rows, rows3d = c, r, r
				rnd.Resize(c, 2*r) // editor / preview pickers launched from here use rnd
				draw()
			}
		case k := <-ip.events:
			switch k {
			case mkUp:
				sel = (sel - 1 + len(names)) % len(names)
				draw()
			case mkDown:
				sel = (sel + 1) % len(names)
				draw()
			case mkLeft:
				return // back
			case mkEnter:
				switch sel {
				case iDiff:
					s.difficulty = runDifficulty(w, cols, rows, ip, s.difficulty)
					saveUserSettings(dropfile, *s)
					draw()
				case iAssist:
					s.aimAssist = !s.aimAssist
					saveUserSettings(dropfile, *s)
					draw()
				case iColor:
					s.colorMode = (s.colorMode + 1) % colorModes
					colorMode = s.colorMode // applies immediately (next encode)
					saveUserSettings(dropfile, *s)
					draw()
				case iEditor:
					_, pn := door32Identity(dropfile)
					updatePresence(dropfile, "map editor", "")
					runEditor(w, cols, rows, rows3d, rnd, ip, s, pn, dropfile)
					draw() // repaint the options menu on return
				case iControls:
					runControls(w, cols, rows, ip, dropfile, s)
					draw()
				case iVersion:
					runVersionInfo(w, cols, rows, ip, dropfile)
					draw()
				case iBack:
					return
				default:
					draw() // soon: no-op
				}
			}
		}
	}
}

// runDifficulty is the tier picker; ENTER saves the choice for this caller.
func runDifficulty(w *bufio.Writer, cols, rows int, ip *input, cur gm.Difficulty) gm.Difficulty {
	tiers := gm.Difficulties()
	names := make([]string, len(tiers))
	blurbs := make([]string, len(tiers))
	for i, d := range tiers {
		names[i] = gm.ProfileFor(d).Name
		blurbs[i] = "ENTER to play at this skill level."
	}
	sel := 0
	for i, d := range tiers {
		if d == cur {
			sel = i
		}
	}
	draw := func() {
		blurbs[sel] = "current: " + cur.String() + "  -  ENTER to set"
		drawListMenu(w, cols, rows, "DIFFICULTY", names, blurbs, nil, sel)
	}
	draw()
	resizeTick := time.NewTicker(1500 * time.Millisecond)
	defer resizeTick.Stop()
	for {
		select {
		case <-ip.quitCh:
			return cur
		case <-resizeTick.C:
			w.WriteString("\x1b[18t")
			w.Flush()
			if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
				cols, rows = c, r
				draw()
			}
		case k := <-ip.events:
			switch k {
			case mkUp:
				sel = (sel - 1 + len(tiers)) % len(tiers)
				draw()
			case mkDown:
				sel = (sel + 1) % len(tiers)
				draw()
			case mkLeft:
				return cur // back without changing
			case mkEnter:
				return tiers[sel] // caller persists
			}
		}
	}
}

// runVehicleMenu lets the player pick a vehicle class (with stats), returning
// the index, or quit=true if they bailed.
// vehEntry is one selectable row: a body style (each character owns its stats).
type vehEntry struct {
	name string
	body int    // gm.BodyTank for the tank; a creature body otherwise
	desc string // blurb (creatures); the tank uses the builtin's own Desc
}

// playerBodies are the creatures a player can pilot. Each character owns its
// stat row (gm.VehBody), so the player roster and bot rolls never disagree.
var playerBodies = []vehEntry{
	{name: "HUMANOID", body: gm.BodyHumanoid, desc: "Upright fighter, balanced frame. Fires from the hand."},
	{name: "GORILLA", body: gm.BodyGorilla, desc: "Knuckle-walking bruiser; bounds high, pounds (knockback)."},
	{name: "MANTIS", body: gm.BodyMantis, desc: "Raptorial forearms; agile and aggressive, leaps far."},
	{name: "T-REX", body: gm.BodyTrex, desc: "Towering biped, heavy jaws. Slow but devastating."},
	{name: "SCORPION", body: gm.BodyScorpion, desc: "Glass sniper: a hitscan laser from the arched tail, but frail and slow."},
	{name: "SERPENT", body: gm.BodySerpent, desc: "Cobra: raised hood, fast; venom spit poisons its prey."},
	{name: "INSECT", body: gm.BodyInsect, desc: "Six-legged skitterer: scales walls (drive into one to climb), fast, deep magazine."},
	{name: "CRAB", body: gm.BodyCrab, desc: "Wide armored shell; lobs heavy shots from a claw."},
	{name: "OCTOPOD", body: gm.BodyOctopod, desc: "Bulbous body, eight tentacles; ink slows foes."},
	{name: "TIGER", body: gm.BodyQuad, desc: "Feline skirmisher: pounce in, scratch for a bleeding wound, lick wounds to recover."},
	{name: "TURTLE", body: gm.BodyTurtle, desc: "Bunker: snaps up close; B tucks into the shell (invulnerable)."},
	{name: "BUTTERFLY", body: gm.BodyButterfly, desc: "Flying healer. Hold JUMP to hover; heal beam mends allies."},
	{name: "ELEPHANT", body: gm.BodyElephant, desc: "Anchor: trunk-hook a foe in then gore with tusks; regenerating shield buffer; B sprays ally shields."},
	{name: "FALCON", body: gm.BodyFalcon, desc: "Flying striker. Hold JUMP to climb; fast talon bolts, gust blast."},
	{name: "STAG", body: gm.BodyStag, desc: "Pack healer: radial aura mends allies; antler charge, swift bolt."},
	{name: "MINOTAUR", body: gm.BodyMinotaur, desc: "Bruiser: heavy hammer; tap B to raise/lower a frontal barrier (it breaks, then recharges)."},
}

// selectableTanks curates which built-in chassis appear in the picker. The roster
// is creatures-first now: TANK is the one true tank (balanced, true-to-scale).
// The retired chassis (SCOUT/HEAVY/RANGER/ARTILLERY) stay in gm.Vehicles so wire
// ids and saved builds don't shift, and they live on as the stat profiles the
// creatures ride on - the playstyles survive, the tank sprawl doesn't.
var selectableTanks = []int{1} // TANK

// vehicleEntries is the full selector list: builtins, then creatures.
func vehicleEntries() []vehEntry {
	e := make([]vehEntry, 0, len(selectableTanks)+len(playerBodies))
	for _, i := range selectableTanks {
		_ = i // the curated builtin is always TANK (gm.Vehicles[1])
		e = append(e, vehEntry{name: gm.Vehicles[1].Name, body: gm.BodyTank, desc: gm.Vehicles[1].Desc})
	}
	e = append(e, playerBodies...)
	return e
}

// charName is the display name of a character body (the selector entry whose
// .body matches), used for stats. Falls back to "TANK" for an unknown body.
func charName(body int) string {
	for _, e := range vehicleEntries() {
		if e.body == body {
			return e.name
		}
	}
	return "TANK"
}

// runVehicleMenu: pick a vehicle. Wide terminals get a two-pane screen (lightbar
// list on the left, a rotating 3D preview on the right); narrow ones fall back to
// a simple centered list. Returns the chosen (chassis, body, color).
func runVehicleMenu(w *bufio.Writer, cols, rows int, ip *input, s *userSettings, dropfile string) (body int, color [3]float64, back, quit bool) {
	entries := vehicleEntries()
	N := len(entries)
	sel := 0 // TANK (the one tank) leads the list
	colorIdx := 0
	sortIdx := 0
	sortEntries(entries, s, charSortKeys[sortIdx].statIdx)

	// Two-pane layout, recomputed on resize: a lightbar list under a color strip on
	// the left, a rotating 3D preview filling the right. relayout returns false when
	// the terminal is too narrow for two panes (caller falls back to the simple list).
	// The preview claims the whole right pane (top row to just above the footer);
	// overlays ride the corners (name/blurb top-right, loadout bottom-left, stat bars
	// bottom-right). The color strip is 3 rows + a spacer at the top-left.
	var leftW, panelCol0, panelW, panelRow0, previewRows, colorStripRow, listTop, listH, descW int
	var pr *Renderer
	relayout := func() bool {
		leftW = cols / 4
		if leftW < 22 {
			leftW = 22
		} else if leftW > 28 {
			leftW = 28
		}
		panelCol0 = leftW + 1
		panelW = cols - panelCol0
		if cols < 70 || rows < 16 || panelW < 10 {
			return false
		}
		panelRow0 = 1
		previewRows = rows - panelRow0 - 1
		if pr == nil {
			pr = newRenderer(panelW, 2*previewRows)
			pr.fogCol = [3]float64{0, 0, 0} // distance fades to black, not navy
		} else {
			pr.Resize(panelW, 2*previewRows)
		}
		colorStripRow = 1
		listTop = colorStripRow + 4
		listH = rows - 1 - listTop
		descW = 28
		if descW > panelW-2 {
			descW = panelW - 2
		}
		return true
	}
	if !relayout() {
		return runVehicleMenuSimple(w, cols, rows, ip, s, dropfile)
	}

	drawFooter := func() {
		fmt.Fprintf(w, "\x1b[%d;2H\x1b[0;90mup/dn pick  </> color  \x1b[0;37mS\x1b[0;90m sort:\x1b[0;37m%-9s\x1b[0;90m  ENTER go  BKSP back  ESC quit\x1b[0m", rows-1, charSortKeys[sortIdx].label)
	}
	header := func() {
		w.WriteString("\x1b[2J\x1b[H")
		drawFooter()
	}
	header()
	scroll := 0
	listDirty := true
	var panelPrev []byte
	descLines := wrapText(entryBlurb(entries[sel], s), descW)
	angle := 0.0
	start := time.Now()
	last := start
	budget := time.Second / 12
	lastSizePoll := time.Now()
	for {
		select {
		case <-ip.quitCh:
			return 0, [3]float64{}, false, true
		default:
		}
		nc := len(gm.SelectColors)
	drain:
		for {
			select {
			case k := <-ip.events:
				switch k {
				case mkUp:
					sel, listDirty = (sel-1+N)%N, true
				case mkDown:
					sel, listDirty = (sel+1)%N, true
				case mkLeft:
					colorIdx, listDirty = (colorIdx-1+nc)%nc, true
				case mkRight:
					colorIdx, listDirty = (colorIdx+1)%nc, true
				case mkEnter:
					e := entries[sel]
					return e.body, gm.SelectColors[colorIdx], false, false
				case mkBack:
					return 0, [3]float64{}, true, false // back to the main menu
				}
			case r := <-ip.runes:
				// Shift+S cycles the sort criterion (lowercase s stays wasd-down).
				if r == 'S' {
					sortIdx = (sortIdx + 1) % len(charSortKeys)
					keep := entries[sel].name
					sortEntries(entries, s, charSortKeys[sortIdx].statIdx)
					sel, listDirty = indexOfEntryName(entries, keep), true
				}
			default:
				break drain
			}
		}
		now := time.Now()
		if now.Sub(lastSizePoll) >= 1500*time.Millisecond {
			w.WriteString("\x1b[18t") // request a window report (telnet-safe live resize)
			lastSizePoll = now
		}
		if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
			cols, rows = c, r
			if !relayout() { // shrank below the two-pane threshold: hand off to the simple list
				return runVehicleMenuSimple(w, cols, rows, ip, s, dropfile)
			}
			header()
			panelPrev, listDirty = nil, true
			descLines = wrapText(entryBlurb(entries[sel], s), descW)
		}
		dt := now.Sub(last).Seconds()
		last = now
		angle += 0.7 * dt
		e := entries[sel]
		body := e.body
		tank := gm.TankSnap{Body: body, Color: gm.SelectColors[colorIdx]}
		tris := appendTank(nil, &tank, now.Sub(start).Seconds())
		pr.renderModel(fitCam(tris, pr.W, pr.H, angle, previewPad(body)), tris)
		panelPrev = blitPanel(w, pr, panelPrev, panelCol0, panelRow0)
		// Overlays drawn over the panel every frame (the model animates behind):
		// name+blurb top-right, weapon loadout bottom-left, stat bars bottom-right.
		drawPreviewDesc(w, cols, panelRow0, entries[sel].name, descLines)
		drawWeaponsBlock(w, panelCol0, panelRow0+previewRows-1, entries[sel], s)
		drawStatBars(w, cols, panelRow0+previewRows-1, entries[sel], s, charSortKeys[sortIdx].statIdx)
		if listDirty {
			if sel < scroll { // keep the selection inside the scrolling window
				scroll = sel
			} else if sel >= scroll+listH {
				scroll = sel - listH + 1
			}
			drawColorStrip(w, 2, colorStripRow, colorIdx)
			drawVehicleList(w, leftW, listTop, listH, entries, sel, scroll)
			drawFooter() // reflects the current sort criterion
			descLines = wrapText(entryBlurb(entries[sel], s), descW)
			panelPrev = nil // force a full preview repaint so a stale overlay is cleared
			listDirty = false
		}
		w.Flush()
		if d := budget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
}

// drawVehicleList renders the left-pane lightbar as a scrolling window of listH rows
// starting at startRow, showing entries[scroll:]. Every entry reads the same
// magenta; only the selection inverts to the cyan highlight bar.
func drawVehicleList(w *bufio.Writer, leftW, startRow, listH int, entries []vehEntry, sel, scroll int) {
	pad := func(str string) string {
		if len(str) < leftW {
			return str + strings.Repeat(" ", leftW-len(str))
		}
		return str[:leftW]
	}
	for r := 0; r < listH; r++ {
		i := scroll + r
		if i >= len(entries) {
			fmt.Fprintf(w, "\x1b[%d;2H%s\x1b[0m", startRow+r, pad("")) // clear unused rows
			continue
		}
		e := entries[i]
		style, label := "\x1b[0;35m", "  "+e.name // uniform magenta for the whole roster
		if i == sel {
			style, label = "\x1b[1;30;46m", "> "+e.name
		}
		fmt.Fprintf(w, "\x1b[%d;2H%s%s\x1b[0m", startRow+r, style, pad(label))
	}
}

// entryBlurb is the selected entry's one-line summary (no chassis/"frame" tag:
// the chassis is an internal stat profile, not a class the player picks).
func entryBlurb(e vehEntry, s *userSettings) string {
	return e.desc
}

// drawPreviewDesc overlays the selected entry's name + blurb at the top-right of
// the preview (drawn after the model each frame, so the animation runs behind
// it). The name is bright red, the blurb CGA yellow; lines are right-aligned.
func drawPreviewDesc(w *bufio.Writer, cols, row0 int, title string, lines []string) {
	if title != "" {
		col := cols - len(title)
		if col < 1 {
			col = 1
		}
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;31;40m%s\x1b[0m", row0, col, title)
		row0++
	}
	for i, l := range lines {
		col := cols - len(l)
		if col < 1 {
			col = 1
		}
		// 1;33;40 = bold yellow on black: pin the background so a stale bg left by the
		// preview blit (e.g. a teal cell) can't bleed in as a cyan flash.
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;33;40m%s\x1b[0m", row0+i, col, l)
	}
}

// drawColorStrip renders the pickable color swatches with a caret under the pick.
func drawColorStrip(w *bufio.Writer, col, row, sel int) {
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90mCOLOR </>\x1b[0m", row, col)
	x := col
	for _, c := range gm.SelectColors {
		fmt.Fprintf(w, "\x1b[%d;%dH%s\xdb\xdb\x1b[0m", row+1, x, fgEsc(clampB(c[0]*255), clampB(c[1]*255), clampB(c[2]*255)))
		x += 2
	}
	fmt.Fprintf(w, "\x1b[%d;%dH%s", row+2, col, strings.Repeat(" ", len(gm.SelectColors)*2)) // clear stale caret
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;37m^^\x1b[0m", row+2, col+sel*2)
}

// charSortKeys is the S-key sort cycle. statIdx -1 = by name; 0..6 index the
// stat bars (HP,SPD,TURN,FIRE,AMMO,REGEN,JUMP). The active stat's bar is marked
// with a cyan pointer; this label drives the footer's "sort:" text.
var charSortKeys = []struct {
	label   string
	statIdx int
}{
	{"NAME", -1}, {"ARMOR", 0}, {"SPEED", 1}, {"TURN", 2},
	{"FIRE RATE", 3}, {"AMMO", 4}, {"REGEN", 5}, {"JUMP", 6},
}

// charVehicle resolves an entry to the Vehicle whose stats it shows.
func charVehicle(e vehEntry, s *userSettings) gm.Vehicle {
	return gm.VehBody(e.body)
}

// jumpFrac normalizes a jump impulse to 0..1 over the roster's range (~3..14).
func jumpFrac(j float64) float64 {
	f := (j - 3) / 11
	if f < 0 {
		f = 0
	} else if f > 1 {
		f = 1
	}
	return f
}

// charBarFracs returns the seven stat-bar fractions for an entry:
// HP, SPD, TURN, FIRE, AMMO, REGEN, JUMP.
func charBarFracs(e vehEntry, s *userSettings) [7]float64 {
	v := charVehicle(e, s)
	vals := [6]float64{float64(v.MaxHP), v.Speed, v.HullTurn, v.FireDelay, v.AmmoMax, v.AmmoRegen}
	var f [7]float64
	for i := 0; i < 6; i++ {
		f[i] = statFrac(i, vals[i])
	}
	f[6] = jumpFrac(gm.EffectiveJump(e.body))
	return f
}

// drawStatBars overlays the character's seven stat bars as a vertical column in
// the preview panel's bottom-right corner: HP on top down to JP on the bottom
// row (bottomRow), right-aligned to rightEdge with the same 1-col margin as the
// description. hotStat (0..6, or -1) highlights the active sort criterion.
func drawStatBars(w *bufio.Writer, rightEdge, bottomRow int, e vehEntry, s *userSettings, hotStat int) {
	fr := charBarFracs(e, s)
	words := [7]string{"HEALTH", "SPEED", "TURN", "FIRE", "AMMO", "REGEN", "JUMP"}
	const wide = 1 + statBarN // cyan marker column + the bar cells
	col := rightEdge - wide
	if col < 1 {
		col = 1
	}
	for i := 0; i < 7; i++ {
		row := bottomRow - (6 - i) // stacked tight: JUMP on the bottom row, HEALTH six rows up
		fmt.Fprintf(w, "\x1b[%d;%dH%s", row, col, statBar(words[i], fr[i], i == hotStat))
	}
}

// drawWeaponsBlock overlays the selected character's weapon loadout (primary,
// secondary, and passive HP regen when it has one) as a small left-justified
// block anchored to the preview panel's bottom-left corner - cross-corner from
// the top-right description. bottomRow is the panel's last row; the block stacks
// upward from there. Backgrounds are pinned black so the panel art can't bleed
// through, and fields are width-padded so a shorter name leaves no stale tail.
func drawWeaponsBlock(w *bufio.Writer, col0, bottomRow int, e vehEntry, s *userSettings) {
	body := e.body
	primName := gm.WeaponName(gm.PrimaryWeapon(body))
	if body == gm.BodyElephant {
		primName = "HOOK/TUSK" // FIRE auto-picks by range: hook far, gore close
	}
	secName := gm.SecondaryWeaponName(body)
	secAnn := ""
	switch secName { // the turtle/minotaur B is a mode, not a palette weapon
	case "SHELL":
		secAnn = "\x1b[1;36;40m(INVULN)"
	case "SHIELD":
		secAnn = "\x1b[1;34;40m(BARRIER)"
	default:
		secAnn = weaponAnn(gm.SecondaryWeapon(body), body)
	}
	passAnn := "\x1b[1;30;40mnone"
	if r := gm.HPRegen(body); r > 0 {
		passAnn = fmt.Sprintf("\x1b[1;35;40m+%.1f HP/s", r) // passives in light magenta
	}
	// LOADOUT header (yellow), then 1 / 2 / PASSIVE, each with a color-coded effect
	// annotation so the screen shows what a weapon DOES, not just its name.
	rows := []string{
		"\x1b[1;33;40mLOADOUT",
		fmt.Sprintf("\x1b[0;96;40m1 \x1b[1;37;40m%-9s %s", primName, weaponAnn(gm.PrimaryWeapon(body), body)),
		fmt.Sprintf("\x1b[0;96;40m2 \x1b[1;37;40m%-9s %s", secName, secAnn),
		fmt.Sprintf("\x1b[0;96;40mPASSIVE %s", passAnn),
	}
	for i, content := range rows {
		row := bottomRow - (len(rows) - 1 - i)
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;40m%-40s", row, col0, "") // clear the field (black bg, no stale tail)
		fmt.Fprintf(w, "\x1b[%d;%dH%s\x1b[0m", row, col0, content)  // draw over it
	}
}

// effectTag is the parenthesized status-effect label for a weapon's effect kind,
// pre-colored (SGR on black). "" for plain damage/heal/shield (shown as +value).
func effectTag(k gm.EffectKind) string {
	switch k {
	case gm.EffBleed:
		return "\x1b[0;31;40m(BLEED)"
	case gm.EffPoison:
		return "\x1b[0;32;40m(POISON)"
	case gm.EffDrain:
		return "\x1b[0;33;40m(BURN)"
	case gm.EffSlow:
		return "\x1b[0;36;40m(SLOW)"
	case gm.EffKnockback:
		return "\x1b[0;37;40m(KNOCK)"
	case gm.EffSlip:
		return "\x1b[1;33;40m(SLIP)"
	case gm.EffDamageDown:
		return "\x1b[0;35;40m(WEAKEN)"
	case gm.EffShieldBust:
		return "\x1b[0;35;40m(STRIP)"
	case gm.EffPull:
		return "\x1b[0;37;40m(PULL)"
	}
	return ""
}

// weaponAnn builds a color-coded effect annotation for weapon idx on a body: its
// damage or support value, an effect tag, and the recharge (or charge stock).
// DMG light-red, HEAL light-green, SHIELD light-blue, tags vary; cooldown gray.
func weaponAnn(idx, body int) string {
	if idx < 0 || idx >= len(gm.Weapons) {
		return ""
	}
	wp := gm.Weapons[idx]
	seg := []string{}
	switch wp.Effect.Kind {
	case gm.EffHeal:
		seg = append(seg, fmt.Sprintf("\x1b[1;32;40m+%.0f HEAL", wp.Effect.Mag))
	case gm.EffShield:
		seg = append(seg, "\x1b[1;34;40mSHIELD \x1b[0;34;40m(ARMOR)")
	case gm.EffSpeed:
		if wp.Damage > 0 {
			seg = append(seg, fmt.Sprintf("\x1b[1;31;40m+%d DMG", wp.Damage))
		}
		seg = append(seg, "\x1b[1;36;40m(HASTE)")
	default:
		if wp.Damage > 0 {
			seg = append(seg, fmt.Sprintf("\x1b[1;31;40m+%d DMG", wp.Damage))
		}
		if tag := effectTag(wp.Effect.Kind); tag != "" {
			seg = append(seg, tag)
		}
	}
	if wp.Charges > 0 { // charge-stock: show the stock + regen instead of a cooldown
		seg = append(seg, fmt.Sprintf("\x1b[0;90;40mx%d %.2gs", wp.Charges, wp.ChargeRegen))
	} else {
		cd := wp.Cooldown
		if cd == 0 {
			cd = gm.VehBody(body).FireDelay
		}
		if cd > 0 {
			seg = append(seg, fmt.Sprintf("\x1b[0;90;40m%.2gs", cd))
		}
	}
	return strings.Join(seg, " ")
}

// indexOfEntryName returns the index of the entry with the given name (0 if
// absent) - used to keep the selection on the same character across a re-sort.
func indexOfEntryName(entries []vehEntry, name string) int {
	for i := range entries {
		if entries[i].name == name {
			return i
		}
	}
	return 0
}

// sortEntries orders the roster by the criterion (stat fracs high-to-low so the
// strongest reads first; name ascending).
func sortEntries(entries []vehEntry, s *userSettings, statIdx int) {
	sort.SliceStable(entries, func(a, b int) bool {
		ea, eb := entries[a], entries[b]
		if statIdx >= 0 {
			if fa, fb := charBarFracs(ea, s)[statIdx], charBarFracs(eb, s)[statIdx]; fa != fb {
				return fa > fb
			}
		}
		return ea.name < eb.name
	})
}

// statBarRange is the value span each stat bar normalizes against (base..base+
// step*pbMaxLevel). Display-only: it sets where a roster stat reads as an empty vs
// a full bar; FireDelay's step is negative so a lower delay reads as a fuller bar.
const pbMaxLevel = 8

var statBarRange = [6]struct{ base, step float64 }{
	{50, 12.5},   // HP:    50..150
	{3.5, 0.6},   // SPEED: 3.5..8.3
	{1.0, 0.2},   // TURN:  1.0..2.6
	{0.9, -0.06}, // FIRE:  0.9..0.42 (lower = better)
	{5, 1.25},    // AMMO:  5..15
	{1.0, 0.2},   // REGEN: 1.0..2.6
}

// statFrac normalizes a stat value to 0..1 over its display range (FireDelay's
// range is inverted, so a lower delay reads as a fuller bar).
func statFrac(i int, val float64) float64 {
	f := (val - statBarRange[i].base) / (statBarRange[i].step * pbMaxLevel)
	if f < 0 {
		f = 0
	} else if f > 1 {
		f = 1
	}
	return f
}

// statBarN is the embedded-label bar width (cells). The stat's full word is
// centered across these cells; the left portion fills with the value color
// (red/yellow/green by fraction) behind black text, the right stays a dark
// track behind grey text - so the bar reads as a labeled progress bar.
const statBarN = 8

// statBar renders one stat as a fixed-width bar with its name embedded and
// centered: filled cells = black text on the value color, empty cells = grey
// text on a dark track. hot (the active sort stat) gets a cyan marker to its
// left. Backgrounds are fully specified per cell, so the panel art can't bleed
// through and it works in every color mode (see barCellSGR).
func statBar(word string, frac float64, hot bool) string {
	fill := int(frac*float64(statBarN) + 0.5)
	if fill < 0 {
		fill = 0
	} else if fill > statBarN {
		fill = statBarN
	}
	var vr, vg, vb byte = 90, 200, 90 // green (high)
	if frac < 0.34 {
		vr, vg, vb = 210, 80, 80 // red (low)
	} else if frac < 0.67 {
		vr, vg, vb = 220, 200, 70 // yellow (mid)
	}
	if len(word) > statBarN {
		word = word[:statBarN]
	}
	cells := []byte(strings.Repeat(" ", statBarN))
	copy(cells[(statBarN-len(word))/2:], word) // center the label
	var b strings.Builder
	if hot { // active sort criterion: cyan pointer on the left
		fmt.Fprintf(&b, "\x1b[%sm\x10", barCellSGR(90, 230, 255, 0, 0, 0))
	} else {
		fmt.Fprintf(&b, "\x1b[%sm ", barCellSGR(0, 0, 0, 0, 0, 0))
	}
	for i := 0; i < statBarN; i++ {
		if i < fill {
			fmt.Fprintf(&b, "\x1b[%sm%c", barCellSGR(0, 0, 0, vr, vg, vb), cells[i])
		} else {
			fmt.Fprintf(&b, "\x1b[%sm%c", barCellSGR(165, 165, 165, 55, 55, 55), cells[i])
		}
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// wrapText word-wraps s to width w.
func wrapText(s string, w int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		if line == "" {
			line = word
		} else if len(line)+1+len(word) <= w {
			line += " " + word
		} else {
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// blitPanel paints a renderer's half-block frame at a screen offset, diffing
// against prev so only changed cells emit (keeps the rotating preview cheap).
func blitPanel(w *bufio.Writer, r *Renderer, prev []byte, col0, row0 int) []byte {
	return blitPanelMasked(w, r, prev, col0, row0, nil)
}

// blitPanelMasked is blitPanel with an optional skip mask (panel-indexed
// y*r.W+x): cells where skip[i] is true are never written, so a static overlay
// painted over them is left untouched by the delta. cur still tracks the true
// panel color for every cell (the mask only changes when the caller does a full
// repaint, so an unmasked cell is always rewritten cleanly the same frame).
func blitPanelMasked(w *bufio.Writer, r *Renderer, prev []byte, col0, row0 int, skip []bool) []byte {
	prows := r.H / 2
	cur := make([]byte, prows*r.W*6)
	var b strings.Builder
	lastSGR := ""
	curX, curY := -1, -1
	mode := colorMode
	for y := 0; y < prows; y++ {
		for x := 0; x < r.W; x++ {
			tp := ((2*y)*r.W + x) * 3
			bp := ((2*y+1)*r.W + x) * 3
			ci := (y*r.W + x) * 6
			quantCell(mode, cur[ci:ci+6], r.fb, tp, bp, x, y)
			if prev != nil && string(prev[ci:ci+6]) == string(cur[ci:ci+6]) {
				continue
			}
			if skip != nil && skip[y*r.W+x] {
				continue // an overlay owns this cell; don't paint the map under it
			}
			if y == prows-1 && x == r.W-1 {
				continue // never write the panel's bottom-right cell: with autowrap
				// on (some clients ignore ESC[?7l) it scrolls the screen up a row
			}
			ry, rx := row0+y, col0+x
			if curY != ry || curX != rx {
				fmt.Fprintf(&b, "\x1b[%d;%dH", ry, rx)
			}
			sgr, glyph := cellSGR(mode, cur[ci:ci+6])
			if sgr != lastSGR {
				b.WriteString("\x1b[")
				b.WriteString(sgr)
				b.WriteByte('m')
				lastSGR = sgr
			}
			b.WriteByte(glyph)
			curY, curX = ry, rx+1
		}
	}
	w.WriteString(b.String())
	return cur
}

// runVehicleMenuSimple is the static centered list for narrow terminals (builtins
// and creatures).
func runVehicleMenuSimple(w *bufio.Writer, cols, rows int, ip *input, s *userSettings, dropfile string) (body int, color [3]float64, back, quit bool) {
	entries := vehicleEntries()
	N := len(entries)
	sel, colorIdx := 0, 0
	nc := len(gm.SelectColors)
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		hdr := "SELECT  VEHICLE"
		fmt.Fprintf(w, "\x1b[2;%dH\x1b[1;96m%s\x1b[0m", (cols-len(hdr))/2+1, hdr)
		for i, e := range entries {
			row := 5 + i
			style, mark := "\x1b[0;36m", "  "
			if e.body != gm.BodyTank {
				style = "\x1b[0;35m"
			}
			if i == sel {
				style, mark = "\x1b[1;30;46m", "> "
			}
			fmt.Fprintf(w, "\x1b[%d;%dH%s %s%-9s \x1b[0m", row, (cols-12)/2+1, style, mark, e.name)
		}
		drawColorStrip(w, (cols-nc*2)/2+1, rows-4, colorIdx)
		foot := "up/dn vehicle  </> color  ENTER go  BKSP back  ESC quit"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows-1, (cols-len(foot))/2+1, foot)
		w.Flush()
	}
	draw()
	resizeTick := time.NewTicker(1500 * time.Millisecond)
	defer resizeTick.Stop()
	for {
		select {
		case <-ip.quitCh:
			return 0, [3]float64{}, false, true
		case <-resizeTick.C:
			w.WriteString("\x1b[18t")
			w.Flush()
			if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
				cols, rows = c, r
				draw()
			}
		case k := <-ip.events:
			switch k {
			case mkUp:
				sel = (sel - 1 + N) % N
				draw()
			case mkDown:
				sel = (sel + 1) % N
				draw()
			case mkLeft:
				colorIdx = (colorIdx - 1 + nc) % nc
				draw()
			case mkRight:
				colorIdx = (colorIdx + 1) % nc
				draw()
			case mkEnter:
				e := entries[sel]
				return e.body, gm.SelectColors[colorIdx], false, false
			case mkBack:
				return 0, [3]float64{}, true, false // back to the main menu
			}
		}
	}
}

// runMapPicker lists the available maps (embedded + usermaps) plus RANDOM for
// an offline game. The list sorts by player capacity then name; left/right
// cycle a capacity filter (ALL, then each player tier in the inventory).
// Returns the chosen map index (-1 = random), or quit/back flags.
// mapOrbitCam frames a whole map (half-extent A) from a 3/4 overhead orbit at
// azimuth angle, sized to fit the pane. Used by the map-select preview.
func mapOrbitCam(A, angle float64, w, h int) Cam {
	const fill = 0.92     // fill the pane; the spinning map's corners stay clear for overlays
	const elev = 0.85     // radians above horizontal (~49deg): a tilted overhead
	const lookFrac = 0.45 // aim below center so the map rides higher in the pane
	radH := A * 1.414     // the map's corner radius
	D := radH / fill
	if dh := radH * float64(w) / (fill * float64(h)); dh > D {
		D = dh // taller-than-wide panes need more pull-back for the vertical
	}
	hor, hgt := math.Cos(elev)*D, math.Sin(elev)*D
	// Aim at a point below the map center: the 3/4 perspective makes the near
	// (bottom) half project larger, so a centered look leaves black at the top
	// and clips the bottom. Looking lower lifts the whole map up in the frame.
	return Cam{
		pos:   V3{math.Sin(angle) * hor, hgt, math.Cos(angle) * hor},
		yaw:   angle + math.Pi, // face back toward the map center
		pitch: math.Atan2(hgt+A*lookFrac, hor),
	}
}

// mapSizeWord describes a map's footprint for the preview metadata.
func mapSizeWord(sz float64) string {
	switch {
	case sz <= 0:
		return "standard"
	case sz < 20:
		return "small"
	case sz < 30:
		return "medium"
	default:
		return "large"
	}
}

// mapModeName is a map's intended mode ("Any" when it doesn't pin one).
func mapModeName(m gm.Map) string {
	if m.Rules != nil && m.Rules.Mode >= 0 {
		return gm.Mode(m.Rules.Mode).String()
	}
	return "Any"
}

func runMapPicker(w *bufio.Writer, cols, rows int, ip *input) (idx int, back, quit bool) {
	order := make([]int, len(gm.Maps))
	for i := range order {
		order[i] = i
	}
	nameOf := func(mi int) string {
		if n := gm.Maps[mi].Name; n != "" {
			return n
		}
		return "(unnamed)"
	}
	capOf := func(mi int) int {
		if c := gm.MapCapacity(gm.Maps[mi]); c > 0 {
			return c
		}
		return 1 << 30 // uncapped (usermaps with no rules): bottom of the list
	}
	sort.Slice(order, func(a, b int) bool {
		if ca, cb := capOf(order[a]), capOf(order[b]); ca != cb {
			return ca < cb
		}
		return nameOf(order[a]) < nameOf(order[b])
	})
	// Capacity tiers drive the filter: ALL first, then each distinct player
	// count present (order is cap-sorted, so distinct values arrive ascending).
	tiers := []int{0}
	for _, mi := range order {
		if c := gm.MapCapacity(gm.Maps[mi]); c > 0 && tiers[len(tiers)-1] != c {
			tiers = append(tiers, c)
		}
	}
	tier := 0
	var view []int // entries: -1 = RANDOM, else a gm.Maps index
	rebuild := func() {
		view = append(view[:0], -1)
		for _, mi := range order {
			if tiers[tier] == 0 || gm.MapCapacity(gm.Maps[mi]) == tiers[tier] {
				view = append(view, mi)
			}
		}
	}
	rebuild()
	sel, scroll := 0, 0
	// Wide terminals get a live 3D preview behind the picker (the map orbits like
	// the character preview); narrow ones fall back to a plain list, redrawn only
	// on change so it doesn't flicker.
	// The map render is the WHOLE screen; the list, info, header and footer are
	// just corner/edge overlays painted on top (the spinning map leaves its corners
	// clear, so they barely cover it). relayout recomputes the layout, the full-bleed
	// preview renderer, and the overlay mask on resize.
	var usePreview bool
	var listTop, listH int
	var pr *Renderer
	var mask []bool
	relayout := func() {
		usePreview = cols >= 60 && rows >= 16
		listTop = 3
		listH = rows - 1 - listTop
		if listH < 3 {
			listH = 3
		}
		if usePreview {
			if pr == nil {
				pr = newRenderer(cols, 2*rows) // full-bleed
				pr.noFog = true                // a clean, fully-lit map, no distance haze
			} else {
				pr.Resize(cols, 2*rows)
			}
		}
		mask = make([]bool, rows*cols)
	}
	relayout()
	buildFor := func() { // (re)build the arena geometry for the selected map
		if !usePreview {
			return
		}
		if mi := view[sel]; mi >= 0 {
			buildArena(gm.Maps[mi])
		} else {
			buildArena(gm.Map{Size: gm.ArenaA}) // RANDOM: a neutral empty arena
		}
	}
	filterLabel := func() string {
		if tiers[tier] > 0 {
			return fmt.Sprintf("%d PLAYER (%d)", tiers[tier], len(view)-1)
		}
		return fmt.Sprintf("ALL (%d)", len(view)-1)
	}
	// mask marks the overlay's glyph cells (screen 1-based -> panel index
	// y*cols+x) so the rotating-map delta blit skips them: the map never repaints
	// under static text, which is what keeps the overlays from flickering (the
	// same reason the character-select overlays - over the static black sky - don't).
	// (mask is (re)allocated in relayout above.)
	markRun := func(row, col, vis int) {
		for i := 0; i < vis; i++ {
			c := col + i
			if row >= 1 && row <= rows && c >= 1 && c <= cols {
				mask[(row-1)*cols+(c-1)] = true
			}
		}
	}
	// rightAt prints s (a single visible run of length vis) ending one column shy
	// of the right edge, so the bottom-right block right-aligns cleanly.
	rightAt := func(row, vis int, s string) {
		col := cols - vis
		if col < 2 {
			col = 2
		}
		fmt.Fprintf(w, "\x1b[%d;%dH%s\x1b[0m", row, col, s)
		markRun(row, col, vis)
	}
	drawOverlay := func() {
		// Glyph-only overlays: a black background sits ONLY behind the text (for
		// legibility over the map), with no width-padding - so the map shows
		// through every gap instead of being chopped by black bars. Each run is
		// recorded in mask so the next delta blit leaves it alone.
		for i := range mask {
			mask[i] = false
		}
		title := "SELECT MAP"
		fmt.Fprintf(w, "\x1b[1;%dH\x1b[1;96;40m%s\x1b[0m", (cols-len(title))/2+1, title)
		markRun(1, (cols-len(title))/2+1, len(title))
		fmt.Fprintf(w, "\x1b[2;2H\x1b[1;35;40m< %s >\x1b[0m", filterLabel())
		markRun(2, 2, len(filterLabel())+4)
		if sel < scroll {
			scroll = sel
		} else if sel >= scroll+listH {
			scroll = sel - listH + 1
		}
		for r := 0; r < listH; r++ { // top-left list; empty rows draw nothing (map shows)
			i, row := scroll+r, listTop+r
			if i >= len(view) {
				continue
			}
			label := "RANDOM"
			if view[i] >= 0 {
				label = clip(nameOf(view[i]), 15)
			}
			if i == sel { // selected: a short cyan highlight bar
				fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;30;46m> %s \x1b[0m", row, label)
				markRun(row, 2, len(label)+3)
			} else {
				fmt.Fprintf(w, "\x1b[%d;2H\x1b[0;36;40m  %s\x1b[0m", row, label)
				markRun(row, 2, len(label)+2)
			}
		}
		// Bottom-right: name + metadata (dim label / bright value) + blurb, each
		// line right-aligned, text-only (no black box behind the blank space).
		mi := view[sel]
		name, blurb := "RANDOM", "A random map each round."
		var meta [][2]string
		if mi >= 0 {
			m := gm.Maps[mi]
			name = clip(nameOf(mi), 24)
			capStr := "uncapped"
			if c := gm.MapCapacity(m); c > 0 {
				capStr = fmt.Sprintf("%d", c)
			}
			meta = [][2]string{{"MODE", mapModeName(m)}, {"PLAYERS", capStr}, {"SIZE", mapSizeWord(m.Size)}}
			parts := []string{mapModeName(m)}
			if c := gm.MapCapacity(m); c > 0 {
				parts = append(parts, fmt.Sprintf("up to %d players", c))
			}
			blurb = strings.Join(append(parts, mapSizeWord(m.Size)+" arena"), " - ")
		}
		blines := wrapText(blurb, 30)
		row := rows - 1 - (1 + len(meta) + len(blines))
		if row < listTop {
			row = listTop
		}
		rightAt(row, len(name), "\x1b[1;93;40m"+name)
		row++
		for _, kv := range meta {
			rightAt(row, len(kv[0])+1+len(kv[1]), "\x1b[0;36;40m"+kv[0]+" \x1b[1;37;40m"+kv[1])
			row++
		}
		for _, bl := range blines {
			rightAt(row, len(bl), "\x1b[0;90;40m"+bl)
			row++
		}
		foot := "up/dn select   </> size filter   ENTER play   BKSP back"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90;40m%s\x1b[0m", rows, (cols-len(foot))/2+1, foot)
		markRun(rows, (cols-len(foot))/2+1, len(foot))
	}

	// Live preview, built exactly like the character-select screen: a slow ticker
	// rotates the map; each frame the map is delta-blitted (only changed cells
	// written) and the overlays are repainted. On a selection/filter change
	// (dirty) we do a full repaint (panelPrev=nil, no mask) to wipe the old
	// overlay, redraw it, and rebuild the mask; on a steady frame the mask keeps
	// the spinning map from repainting under the static text, so nothing flickers.
	angle := 0.0
	start := time.Now()
	last := start
	budget := time.Second / 12
	var panelPrev []byte
	dirty := true
	lastSizePoll := time.Now()
	buildFor()
	if !usePreview {
		w.WriteString("\x1b[2J\x1b[H")
	}
	for {
		select {
		case <-ip.quitCh:
			return 0, false, true
		default:
		}
	drain:
		for {
			select {
			case k := <-ip.events:
				switch k {
				case mkUp:
					sel = (sel - 1 + len(view)) % len(view)
					buildFor()
					dirty = true
				case mkDown:
					sel = (sel + 1) % len(view)
					buildFor()
					dirty = true
				case mkLeft:
					tier = (tier - 1 + len(tiers)) % len(tiers)
					rebuild()
					sel = 0
					buildFor()
					dirty = true
				case mkRight:
					tier = (tier + 1) % len(tiers)
					rebuild()
					sel = 0
					buildFor()
					dirty = true
				case mkEnter:
					return view[sel], false, false // view[0] = RANDOM (-1)
				case mkBack:
					return 0, true, false
				}
			default:
				break drain
			}
		}
		now := time.Now()
		if now.Sub(lastSizePoll) >= 1500*time.Millisecond {
			w.WriteString("\x1b[18t") // request a window report (telnet-safe live resize)
			lastSizePoll = now
		}
		if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
			cols, rows = c, r
			relayout()
			buildFor() // rebuild arena geometry at the new aspect
			panelPrev, dirty = nil, true
			w.WriteString("\x1b[2J") // clear stale overlay text outside the new bounds
		}
		dt := now.Sub(last).Seconds()
		last = now
		angle += 0.5 * dt
		if usePreview {
			pr.renderModel(mapOrbitCam(arenaSize, angle, pr.W, pr.H), arena)
			if dirty {
				panelPrev = blitPanel(w, pr, nil, 1, 1) // full repaint: clears the old overlay
			} else {
				panelPrev = blitPanelMasked(w, pr, panelPrev, 1, 1, mask) // delta, overlay cells skipped
			}
		} else if dirty {
			w.WriteString("\x1b[2J\x1b[H")
		}
		if dirty {
			drawOverlay() // repaint text + rebuild the mask over the fresh frame
			dirty = false
		}
		w.Flush()
		if d := budget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
}

// drawCenterNote clears the screen and shows one centered line - a brief
// transition notice (e.g. following the party into the arena).
func drawCenterNote(w *bufio.Writer, cols, rows int, s string) {
	fmt.Fprintf(w, "\x1b[2J\x1b[%d;%dH\x1b[1;96m%s\x1b[0m", rows/2, (cols-len(s))/2+1, s)
	w.Flush()
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

// cycleVote moves a vote among n map+mode pairings (wraps).
func cycleVote(cur, dir, n int) int {
	if n == 0 {
		return cur
	}
	if cur < 0 {
		return 0
	}
	return (cur + dir + n) % n
}

// The lobby map list filters by game type with </>, mirroring the single-player
// picker. Filter 0 is ALL; filter k shows only maps whose mode is the k-1th
// distinct mode present (in pairing order). voteMode stays a real pairing index
// (== the server map index sent as Input.Vote); the filter is a view only.

// lobbyModes lists the distinct modes present, in first-seen order.
func lobbyModes(pairs []proto.LobbyEntry) []gm.Mode {
	var modes []gm.Mode
	seen := map[gm.Mode]bool{}
	for _, p := range pairs {
		if !seen[p.Mode] {
			seen[p.Mode] = true
			modes = append(modes, p.Mode)
		}
	}
	return modes
}

// lobbyView returns the pairing indices visible under the given filter.
func lobbyView(pairs []proto.LobbyEntry, filter int) []int {
	modes := lobbyModes(pairs)
	var view []int
	for i, p := range pairs {
		if filter <= 0 || (filter-1 < len(modes) && p.Mode == modes[filter-1]) {
			view = append(view, i)
		}
	}
	return view
}

// lobbyFilterLabel describes the active filter, e.g. "ALL (12)" or "CTF (3)".
func lobbyFilterLabel(pairs []proto.LobbyEntry, filter int) string {
	modes := lobbyModes(pairs)
	n := len(lobbyView(pairs, filter))
	if filter <= 0 || filter-1 >= len(modes) {
		return fmt.Sprintf("ALL (%d)", n)
	}
	return fmt.Sprintf("%s (%d)", modes[filter-1].String(), n)
}

// lobbyMoveVote slides the vote up/down within the filtered view (wraps).
func lobbyMoveVote(pairs []proto.LobbyEntry, filter, voteMode, dir int) int {
	view := lobbyView(pairs, filter)
	if len(view) == 0 {
		return voteMode
	}
	pos := 0
	for j, idx := range view {
		if idx == voteMode {
			pos = j
			break
		}
	}
	return view[(pos+dir+len(view))%len(view)]
}

// lobbyMoveFilter cycles the mode filter and snaps the vote to the first map now
// visible, so the highlight never lands on a hidden entry.
func lobbyMoveFilter(pairs []proto.LobbyEntry, filter, dir int) (newFilter, newVote int) {
	nf := 1 + len(lobbyModes(pairs))
	newFilter = (filter + dir + nf) % nf
	if view := lobbyView(pairs, newFilter); len(view) > 0 {
		return newFilter, view[0]
	}
	return newFilter, 0
}

// drawLobby overlays the between-match vote lobby: the votable map+mode pairings
// with live tallies, your pick, the roster, and a countdown to the next match.
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
	pairs := v.pairings
	hdr := "VOTE THE NEXT MAP"
	if len(pairs) == 0 {
		msg := "waiting for arena..."
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", rows/2, (cols-len(msg))/2+1, msg)
		return
	}
	lead, leadN := -1, 0
	for i := range pairs {
		if n := votesOf(i); n > leadN {
			leadN, lead = n, i
		}
	}
	// Window the list around the player's current pick (the pool can be large).
	winRows := rows - 9
	if winRows < 4 {
		winRows = 4
	}
	if winRows > len(pairs) {
		winRows = len(pairs)
	}
	top := 0
	if voteMode >= winRows {
		top = voteMode - winRows + 1
	}
	if top > len(pairs)-winRows {
		top = len(pairs) - winRows
	}
	row := rows/2 - winRows/2 - 1
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;1;37m%s\x1b[0m", row, (cols-len(hdr))/2+1, hdr)
	row += 2
	for i := top; i < top+winRows && i < len(pairs); i++ {
		marker, style := "  ", "\x1b[0;36m"
		if i == voteMode {
			marker, style = "> ", "\x1b[1;33m"
		}
		if i == lead && leadN > 0 {
			style = "\x1b[1;30;46m" // leading: black on cyan
		}
		name := pairs[i].Name
		if len(name) > 14 {
			name = name[:14]
		}
		line := fmt.Sprintf("%s%-14s %-8s %dv", marker, name, pairs[i].Mode.String(), votesOf(i))
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

// lobbyPreview carries the between-frame state for the online vote lobby's live
// map preview (the rotating-map-behind-overlays view, mirroring the SP map
// picker). The map geometry is fetched lazily (MsgMapReq) and cached by index.
type lobbyPreview struct {
	angle   float64
	prev    []byte // last blitted panel (for the masked delta)
	mask    []bool // overlay glyph cells (skipped by the delta so text never flickers)
	sig     string // overlay-content signature; a change forces a full repaint
	geomIdx int    // map index whose geometry is in tris (-1 = none built)
	tris    []Tri
	size    float64
	scroll  int
}

// splitCapTag peels the " <n>P" capacity tag EncodeLobby appends to a map name,
// returning the bare name and the capacity string ("" if no tag).
func splitCapTag(name string) (disp, capStr string) {
	if i := strings.LastIndex(name, " "); i > 0 {
		if tag := name[i+1:]; len(tag) >= 2 && tag[len(tag)-1] == 'P' {
			if _, err := strconv.Atoi(tag[:len(tag)-1]); err == nil {
				return name[:i], tag[:len(tag)-1]
			}
		}
	}
	return name, ""
}

// renderLobbyFrame draws one lobby frame: a slowly orbiting preview of the
// highlighted map behind glyph overlays (vote list, tallies, metadata, countdown)
// - the same delta+mask technique as the SP map picker, so the spinning map never
// repaints under the static text. The preview map is fetched on demand and cached;
// while a chat/confirm modal is up the rotation freezes so those overlays sit over
// a static frame (no flicker), and the modal text is drawn on top.
func renderLobbyFrame(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ns *netSession, v viewState, voteMode, voteCommit, voteFilter int, voteReady bool, dt float64, chat *chatUI, escConfirm bool, lv *lobbyPreview) {
	pairs := v.pairings
	selIdx := voteMode
	if selIdx < 0 || selIdx >= len(pairs) {
		selIdx = 0
	}
	view := lobbyView(pairs, voteFilter) // pairing indices visible under the mode filter
	filterLabel := lobbyFilterLabel(pairs, voteFilter)
	// Lazy + async: ask for the selected map, but keep showing (and spinning) the
	// last map we have until the new geometry arrives, then swap. Never blank or
	// freeze the pane - that's what read as "broken". geomIdx is what's DISPLAYED;
	// haveSel means it matches the selection (so the SIZE/metadata are accurate).
	if len(pairs) > 0 {
		ns.ensurePreview(selIdx) // disk cache first, else fire-and-forget network fetch
		if lv.geomIdx != selIdx {
			if m, ok := ns.preview(selIdx); ok {
				lv.tris, lv.size = mapTris(m)
				lv.geomIdx = selIdx
			}
		}
	}
	haveAny := lv.geomIdx >= 0      // something to render (else first-load black)
	haveSel := lv.geomIdx == selIdx // the selected map's geometry is up
	// Keep the highlighted row inside the scrolling window (list starts at row 3).
	// scroll/positions are over the FILTERED view, not raw pairing indices.
	listH := rows - 4
	if listH < 3 {
		listH = 3
	}
	selPos := 0
	for j, idx := range view {
		if idx == selIdx {
			selPos = j
			break
		}
	}
	if selPos < lv.scroll {
		lv.scroll = selPos
	} else if selPos >= lv.scroll+listH {
		lv.scroll = selPos - listH + 1
	}
	if lv.scroll < 0 {
		lv.scroll = 0
	}

	// The map keeps spinning while chat is up (chat is masked, so it stays put);
	// only the Esc "leave?" confirm freezes the spin (it's a brief modal that
	// isn't mask-aware, so it sits over a static frame).
	if !escConfirm && haveAny {
		lv.angle += 0.5 * dt
	}

	players := 0
	for i := range v.tanks {
		if !v.tanks[i].Bot {
			players++
		}
	}
	// Chat is part of the masked overlay: a content change (new/expired toast,
	// keystroke, transcript toggle) flips the signature -> a full repaint redraws
	// it and rebuilds the mask; between changes the delta blit skips its cells.
	var cs strings.Builder
	for i := range chat.toasts {
		fmt.Fprintf(&cs, "%d,", chat.toasts[i].seqKey)
	}
	chatSig := fmt.Sprintf("%t%t%t%q|%s", chat.active, chat.transcript, escConfirm, chat.input, cs.String())
	sig := fmt.Sprintf("%d|%d|%d|%t|%d|%d|%d|%v|%t|%t|%d|%d|%s",
		selIdx, voteCommit, voteFilter, voteReady, v.lobbyReady, lv.scroll, int(math.Ceil(v.timer)), v.votes, haveSel, escConfirm, players, len(view), chatSig)
	dirty := sig != lv.sig
	lv.sig = sig

	rnd.noFog = true // a clean, fully-lit preview - no distance haze dimming the map
	if haveAny {
		rnd.renderModel(mapOrbitCam(lv.size, lv.angle, rnd.W, rnd.H), lv.tris)
	} else {
		rnd.clearBlack() // first load only: a plain backdrop until the first geometry lands
	}
	if dirty {
		lv.prev = blitPanel(w, rnd, nil, 1, 1) // full repaint wipes the old overlay
		drawLobbyOverlay(w, cols, rows, v, selIdx, voteCommit, voteReady, lv.size, haveSel, lv.scroll, lv.mask, view, filterLabel)
		// Chat rides on top and adds its cells to the mask so the spinning map
		// never repaints under it. drawn after the overlay reset, so it accretes.
		mark := func(row, col, vis int) {
			for i := 0; i < vis; i++ {
				if c := col + i; row >= 1 && row <= rows && c >= 1 && c <= cols {
					lv.mask[(row-1)*cols+(c-1)] = true
				}
			}
		}
		drawChat(w, cols, rows, chat, mark)
		if escConfirm {
			drawEscConfirm(w, cols) // over the (now frozen) map
		}
	} else {
		lv.prev = blitPanelMasked(w, rnd, lv.prev, 1, 1, lv.mask) // delta; overlay+chat cells skipped
	}
	w.Flush()
}

// drawLobbyOverlay paints the glyph-only lobby overlays and records their cells
// in mask. Layout mirrors the SP picker: votable maps + tallies on the left,
// selected-map metadata bottom-right, countdown + players top-right.
func drawLobbyOverlay(w *bufio.Writer, cols, rows int, v viewState, selIdx, committed int, ready bool, size float64, haveGeom bool, scroll int, mask []bool, view []int, filterLabel string) {
	for i := range mask {
		mask[i] = false
	}
	markRun := func(row, col, vis int) {
		for i := 0; i < vis; i++ {
			if c := col + i; row >= 1 && row <= rows && c >= 1 && c <= cols {
				mask[(row-1)*cols+(c-1)] = true
			}
		}
	}
	rightAt := func(row, vis int, s string) {
		col := cols - vis
		if col < 2 {
			col = 2
		}
		fmt.Fprintf(w, "\x1b[%d;%dH%s\x1b[0m", row, col, s)
		markRun(row, col, vis)
	}
	title := "VOTE  NEXT  MAP"
	fmt.Fprintf(w, "\x1b[1;%dH\x1b[1;96;40m%s\x1b[0m", (cols-len(title))/2+1, title)
	markRun(1, (cols-len(title))/2+1, len(title))
	foot := "up/dn vote   </> filter   ENTER lock   C chat   ESC leave"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90;40m%s\x1b[0m", rows, (cols-len(foot))/2+1, foot)
	markRun(rows, (cols-len(foot))/2+1, len(foot))

	pairs := v.pairings
	if len(pairs) == 0 {
		msg := "waiting for arena..."
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37;40m%s\x1b[0m", rows/2, (cols-len(msg))/2+1, msg)
		markRun(rows/2, (cols-len(msg))/2+1, len(msg))
		return
	}
	votesOf := func(i int) int {
		if i >= 0 && i < len(v.votes) {
			return v.votes[i]
		}
		return 0
	}
	lead, leadN := -1, 0
	for i := range pairs {
		if n := votesOf(i); n > leadN {
			leadN, lead = n, i
		}
	}
	// Row 2: game-type filter on the left (</> cycles it), countdown + players on
	// the right.
	fmt.Fprintf(w, "\x1b[2;2H\x1b[1;35;40m< %s >\x1b[0m", filterLabel)
	markRun(2, 2, len(filterLabel)+4)
	nm := fmt.Sprintf("NEXT MATCH IN %d", int(math.Ceil(v.timer)))
	rightAt(1, len(nm), "\x1b[1;92;40m"+nm)
	players := 0
	for i := range v.tanks {
		if !v.tanks[i].Bot {
			players++
		}
	}
	pl := fmt.Sprintf("PLAYERS ONLINE %d", players)
	rightAt(2, len(pl), "\x1b[0;37;40m"+pl)
	if players > 0 { // how many have locked their vote (ENTER) -> drives fast-forward
		lk := fmt.Sprintf("LOCKED %d/%d", v.lobbyReady, players)
		style := "\x1b[0;37;40m"
		if v.lobbyReady >= players {
			style = "\x1b[1;92;40m" // everyone in: the match is about to fast-forward
		}
		rightAt(3, len(lk), style+lk)
	}
	// Left list: the filtered maps. Cursor = cyan bar, current leader = green, and
	// a check marks the map YOU locked. A vote count shows only where there are
	// votes (no wall of "0").
	listTop, listH := 3, rows-4
	if listH < 3 {
		listH = 3
	}
	for r := 0; r < listH; r++ {
		p := scroll + r
		if p >= len(view) {
			continue
		}
		i := view[p] // real pairing index
		row := listTop + r
		disp, _ := splitCapTag(pairs[i].Name)
		line := clip(disp, 15)
		if nv := votesOf(i); nv > 0 {
			unit := "votes"
			if nv == 1 {
				unit = "vote"
			}
			line = fmt.Sprintf("%-15s  %d %s", clip(disp, 15), nv, unit)
		}
		if i == committed { // your locked pick gets a check (CP437 0xFB)
			lock := " \xfb"
			if !ready {
				lock = " \xfa" // committed but not locked-in (you've since moved): a faint dot
			}
			line += lock
		}
		switch {
		case i == selIdx:
			fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;30;46m> %s \x1b[0m", row, line)
			markRun(row, 2, len(line)+3)
		case i == lead && leadN > 0:
			fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;32;40m  %s\x1b[0m", row, line)
			markRun(row, 2, len(line)+2)
		default:
			fmt.Fprintf(w, "\x1b[%d;2H\x1b[0;36;40m  %s\x1b[0m", row, line)
			markRun(row, 2, len(line)+2)
		}
	}
	// Bottom-right: selected map metadata (dim label / bright value) + blurb.
	disp, capStr := splitCapTag(pairs[selIdx].Name)
	mode := pairs[selIdx].Mode.String()
	meta := [][2]string{{"MODE", mode}}
	if capStr != "" {
		meta = append(meta, [2]string{"PLAYERS", capStr})
	}
	sizeStr := "loading"
	if haveGeom {
		sizeStr = mapSizeWord(size)
	}
	meta = append(meta, [2]string{"SIZE", sizeStr})
	blurb := mode
	if capStr != "" {
		blurb += " - up to " + capStr + " players"
	}
	blines := wrapText(blurb, 30)
	name := clip(disp, 24)
	row := rows - 1 - (1 + len(meta) + len(blines))
	if row < listTop {
		row = listTop
	}
	rightAt(row, len(name), "\x1b[1;93;40m"+name)
	row++
	for _, kv := range meta {
		rightAt(row, len(kv[0])+1+len(kv[1]), "\x1b[0;36;40m"+kv[0]+" \x1b[1;37;40m"+kv[1])
		row++
	}
	for _, bl := range blines {
		rightAt(row, len(bl), "\x1b[0;90;40m"+bl)
		row++
	}
}

// drawScoreboard overlays VICTORY/GAME OVER and a ranked frag list.
// drawResultBanner paints the caller's own result at the foot of the scoreboard:
// an achievement line (new personal best / placed #N / deepest wave), the score
// earned, and a compact points breakdown. Nothing shows for matches that weren't
// recorded (res == nil, e.g. a playtest).
func drawResultBanner(w *bufio.Writer, cols, rows int, res *matchResult, rank scoreRank) {
	if res == nil {
		return
	}
	center := func(row int, sgr, s string) {
		if s == "" {
			return
		}
		fmt.Fprintf(w, "\x1b[%d;%dH%s%s\x1b[0m", row, (cols-len(s))/2+1, sgr, s)
	}
	// Achievement line (only the strongest one).
	switch {
	case rank.personalBest:
		center(rows-4, "\x1b[1;93m", "** NEW PERSONAL BEST **")
	case rank.bestWave:
		center(rows-4, "\x1b[1;93m", "** DEEPEST WAVE YET **")
	case rank.rank > 0 && rank.rank <= 10:
		center(rows-4, "\x1b[1;96m", fmt.Sprintf("your #%d best run in this mode", rank.rank))
	}
	center(rows-3, "\x1b[1;97m", fmt.Sprintf("SCORE  %d", res.Score))
	center(rows-2, "\x1b[0;90m", scoreBreakdown(*res))
}

func drawScoreboard(w *bufio.Writer, cols, rows int, v viewState, res *matchResult, rank scoreRank) {
	rs := gm.RulesetFor(v.mode)
	won := v.winnerID == v.me
	title, fg := "GAME OVER", 4 // CGA red
	if rs.Teams == 2 {
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
	drawResultBanner(w, cols, rows, res, rank)
	if v.mode == gm.ModeFlagRun {
		line := fmt.Sprintf("collected %d of %d flags", v.flagsTotal-v.flagsLeft, v.flagsTotal)
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;97m%s\x1b[0m", rows/2, (cols-len(line))/2+1, line)
		return
	}
	if rs.Teams == 2 {
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
		name := t.Name
		if name == "" {
			name = "BOT"
		}
		if len(name) > 12 {
			name = name[:12]
		}
		style := "\x1b[0;37m"
		if t.ID == v.me {
			style = "\x1b[1;97m" // your row is bright
		}
		body := fmt.Sprintf("%-12s  %2d frags   %2d deaths", name, t.Kills, t.Deaths)
		// color swatch in the tank's color, then the body
		col := (cols-(len(body)+3))/2 + 1
		fmt.Fprintf(w, "\x1b[%d;%dH%s\xdb\xdb\x1b[0m %s%s\x1b[0m",
			listTop+i, col,
			fgEsc(clampB(t.Color[0]*255), clampB(t.Color[1]*255), clampB(t.Color[2]*255)),
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
	doorDropfile = dropfile // package default for identity lookups off the main flow (publish, score board)

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

	// Raw mode is set inside openTerm. The input reader blocks in its own
	// goroutine; Term.Read tolerates the non-blocking inherited door socket.
	fmt.Fprint(term, "\x1b[?25l\x1b[?7l\x1b>") // hide cursor, disable auto-wrap, normal keypad (numpad Enter = CR, not SS3)
	ip := &input{quitCh: make(chan struct{}), events: make(chan menuKey, 32), runes: make(chan rune, 64), cpr: make(chan time.Time, 1), resizeCh: make(chan [2]int, 1)}
	ip.winCols, ip.winRows = cols, rows // seed the live size with the startup detection
	go ip.reader(term)
	// Backstop a vanished client (half-open socket): disconnect after this long with
	// no input so the door exits and Synchronet reclaims the node. 0 disables.
	idleLimit := 10 * time.Minute
	if v := loadINI(defaultINIPath())["idle_timeout"]; v != "" {
		if m, e := strconv.Atoi(v); e == nil {
			idleLimit = time.Duration(m) * time.Minute
		}
	}
	go ip.idleWatchdog(term, idleLimit)
	logf("setup done: grid %dx%d (rows3d=%d), idle timeout %v, entering loop", cols, rows, rows3d, idleLimit)

	cleanup := func() {
		clearPresence(dropfile)
		restore()
		fmt.Fprint(term, "\x1b[?7h\x1b[0m\x1b[?25h\x1b[2J\x1b[H")
	}
	sigCh = make(chan os.Signal, 1)
	signal.Notify(sigCh, shutdownSignals...)

	w := bufio.NewWriterSize(&countWriter{w: term}, 1<<16)

	// Load author maps (editor output) so they're playable/pinnable offline. The
	// arena server loads its own set; this is the offline/door pool.
	if n := gm.LoadMapDir(authorMapsDir()); n > 0 {
		logf("loaded %d author map(s) from %s", n, authorMapsDir())
	}

	// The menu drives everything: pick a single-player mode, join the arena, or
	// open OPTIONS. Preferences are loaded per-BBS-user and editable in OPTIONS.
	settings := loadUserSettings(dropfile)
	colorMode = settings.colorMode                 // output palette (also covers the pre-login demo)
	ip.setBinds(effectiveBinds(settings.keyBinds)) // apply custom controls
	startUpdateCheck()                             // background GitHub probe for a newer client
	_, playerName := door32Identity(dropfile)      // the caller's handle, for scoreboards
	// Pre-login / anonymous launch (no dropfile user, e.g. login.js attract): drop
	// straight into the demo. Any key wakes it to the menu; Esc/disconnect exits.
	if !dropfileHasUser(dropfile) {
		runDemo(w, cols, rows, rows3d, rnd, ip, &chatUI{}, dropfile)
	}
	note := ""
	pendingJoin := false // a single-player match was abandoned to follow the party into the arena
	for {
		// Re-sync to the live terminal size at each screen so a resize that happened
		// in one screen carries into the next (the screens also resize in-loop).
		if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
			cols, rows = c, r
			rows3d = rows
			rnd.Resize(cols, 2*rows3d)
		}
		var choice menuChoice
		if pendingJoin {
			pendingJoin, choice = false, menuChoice{autoJoin: true} // skip the menu, go straight in
		} else {
			choice = runMenu(w, cols, rows, rows3d, rnd, ip, note, dropfile)
		}
		if choice.relayout {
			continue // terminal resized: loop top re-syncs cols/rows and re-enters
		}
		if choice.quit {
			cleanup()
			return
		}
		if choice.options {
			updatePresence(dropfile, "options", "")
			runOptions(w, cols, rows, rows3d, rnd, ip, dropfile, &settings)
			note = ""
			continue
		}
		if choice.help {
			runHelp(w, cols, rows, ip)
			note = ""
			continue
		}
		if choice.highScores {
			runHighScores(w, cols, rows, ip, dropfile)
			note = ""
			continue
		}
		if choice.card {
			runPlayerCard(w, cols, rows, ip, dropfile)
			note = ""
			continue
		}
		if choice.update {
			runVersionInfo(w, cols, rows, ip, dropfile)
			note = ""
			continue
		}
		if choice.party {
			updatePresence(dropfile, "party", "")
			runParty(w, cols, rows, ip, dropfile)
			note = ""
			continue
		}
		var vbody int
		var vcolor [3]float64
		if choice.autoJoin {
			// Following the party into the arena: skip the picker, drop in as the
			// default TANK (you can hot-swap with V once inside), after a brief
			// heads-up so it isn't a silent yank.
			vbody, vcolor = gm.BodyTank, gm.SelectColors[0]
			drawCenterNote(w, cols, rows, "Your party is in the arena - joining...")
			time.Sleep(1200 * time.Millisecond)
		} else {
			updatePresence(dropfile, "vehicle select", "")
			var vback, vquit bool
			vbody, vcolor, vback, vquit = runVehicleMenu(w, cols, rows, ip, &settings, dropfile)
			if vquit {
				cleanup()
				return
			}
			if vback { // Backspace from character select: back to the main menu
				note = ""
				continue
			}
		}
		if choice.campaign {
			if runCampaign(w, cols, rows, rows3d, rnd, ip, dropfile, &settings, vbody, vcolor, playerName) {
				cleanup()
				return
			}
			note = ""
			continue
		}
		var sess session
		state, detail := "single-player", choice.mode.String()
		if choice.online || choice.autoJoin {
			ns, err := connectArena(dropfile, vbody, vcolor)
			if err != nil {
				if re, ok := err.(*rejectErr); ok { // version mismatch etc.: actionable, show as-is
					note = re.Error()
				} else {
					note = "Could not reach the arena: " + err.Error()
				}
				continue
			}
			sess = ns
			state, detail = "online arena", ""
		} else {
			updatePresence(dropfile, "map select", choice.mode.String())
			mapIdx, back, mquit := runMapPicker(w, cols, rows, ip)
			if mquit {
				cleanup()
				return
			}
			if back {
				note = ""
				continue // back to the main menu
			}
			if mapIdx >= 0 {
				mode := choice.mode // a map with an explicit Rules.Mode plays in that mode
				if r := gm.Maps[mapIdx].Rules; r != nil && r.Mode >= 0 {
					mode = gm.Mode(r.Mode)
				}
				detail = mode.String() + " on " + gm.Maps[mapIdx].Name
				sess = newOfflineOnMap(mapIdx, botCountFor(mode, false), mode, settings.difficulty, settings.aimAssist, playerName, vcolor, vbody)
			} else {
				sess = newOfflineSession(botCountFor(choice.mode, false), choice.mode, settings.difficulty, settings.aimAssist, playerName, vcolor, vbody)
			}
		}
		note = ""
		// pickChar opens the character picker mid-match (used by the in-arena
		// change-character key); ok=false if the caller backed out.
		pickChar := func() (int, [3]float64, bool) {
			bod, col, back, quit := runVehicleMenuSimple(w, cols, rows, ip, &settings, dropfile)
			return bod, col, !back && !quit
		}
		quit, joinArena := playMatch(w, cols, rows, rows3d, rnd, ip, sess, dropfile, state, detail, false, pickChar)
		sess.close()
		if quit {
			cleanup()
			return
		}
		if state == "online arena" {
			autoJoinArmed = false // you've been in the arena: don't auto-rejoin while a mate lingers
		}
		if joinArena { // a party-mate entered the arena mid-single-player: follow them next loop
			pendingJoin = true
		}
		// mkBack: returned from the match to the menu
	}
}

// playMatch runs one session (offline or arena) to completion. Returns
// (quit, joinArena): quit=true if the player quit the program (Q / OS signal);
// joinArena=true if a party-mate entered the arena during a single-player match
// and we should follow them in (the abandoned match records no stats - it never
// reaches its Ended phase). Both false = backed out to the menu (Backspace).
// Shared by the main game and the editor's playtest. oneShot (the campaign runner)
// makes it return after a single match.
func playMatch(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, sess session, dropfile, presenceState, presenceDetail string, oneShot bool, pickChar func() (int, [3]float64, bool)) (bool, bool) {
	ip.setEscMenu(true) // in-game Esc = exit confirm, not instant quit
	defer ip.setEscMenu(false)
	netSess, _ := sess.(*netSession)     // non-nil only for an online arena match
	offSess, _ := sess.(*offlineSession) // non-nil only for a local single-player match
	escConfirm := false                  // the small "leave match?" banner
	escT := 0.0                          // seconds until it auto-dismisses
	var cam Cam
	prevHP := -1
	flash := 0.0
	const targetFPS = 30.0
	frameBudget := time.Second / time.Duration(targetFPS)
	pace := linkPace{budget: frameBudget}
	var prev []byte
	lastSig := ""
	start := time.Now()
	last := start
	fpsT := start
	frames := 0
	fps := 0
	// Info panel (I key) sampling; refreshed on the once-a-second fps rollover.
	var wrSum time.Duration // write+flush time accumulated since the rollover
	wrN := 0
	txBase := txBytes.Load()
	skBase := pace.skips
	var infoTx int64
	var infoWr time.Duration
	infoSk := 0
	infoRtt := -time.Millisecond // <0 = no CPR reply yet ("-")
	var pingSent, lastPing time.Time
	pingWait := false
	voteMode := -1                                                 // highlighted/previewed pairing index; up/dn moves it within the filter
	voteCommit := -1                                               // the pairing LOCKED with ENTER (sent as Input.Vote); -1 = not yet
	voteReady := false                                             // ENTER pressed and not since changed: counts toward fast-forward
	voteFilter := 0                                                // lobby game-type filter (0=ALL); </> cycles it
	var lobbyPairings []proto.LobbyEntry                           // latest votable pairings (for filter/nav math)
	lobbyN := 0                                                    // number of votable pairings (from the server's MsgLobby)
	lv := lobbyPreview{geomIdx: -1, mask: make([]bool, rows*cols)} // online lobby map preview
	killBanner := ""                                               // transient "KILLED X" top-center text
	killBannerT := 0.0
	toast := "" // transient author message (event system)
	toastT := 0.0
	deathBy := ""                   // "X killed you with Y" (shown on the death banner)
	killerID := -1                  // who killed us this death (for the kill cam); -1 = none
	var replay []replayFrame        // ring buffer of recent frames for the death-cam replay
	const replayMax = 90            // ~3s at 30fps
	const deadRecFrames = 20        // ~0.7s recorded into death so the replay shows the kill
	deadRec := 0                    // dead frames recorded so far this death
	replayStart := time.Time{}      // when the current death replay began (zero = live)
	recentKill := map[int]float64{} // tank id -> seconds left to show a leaderboard +1
	var oneShotAt time.Time         // oneShot: when the level resolved (end/out of lives)
	dropFlag := false               // G pressed this tick: drop the carried CTF flag
	cruiseDir := 0                  // latched auto-move: 0 none, 1 fwd, 2 back, 3 left, 4 right
	curMapSig := "?"                // signature of the currently-built map; rebuild on change
	wallsHidden := false            // border walls hidden during the death-cam
	defer func() { hideArenaWalls = false }()
	topdown := false
	lastPhase := gm.PhaseActive
	lastSelfDead := false    // the local tank's death state last frame (for the change-char gate)
	matchStart := time.Now() // reset when a match goes Active; used for stats duration
	_, statName := door32Identity(dropfile)
	recordScores := dropfileHasUser(dropfile) // anonymous/pre-login matches don't count
	matchOnline := presenceState == "online arena"
	matchDiff := gm.DiffNormal
	if osess, ok := sess.(*offlineSession); ok {
		matchDiff = osess.diff
	}
	var lastResult *matchResult // the finished-match record (drives the scoreboard banner)
	var resultRank scoreRank    // where that result placed locally
	lastPresence := time.Time{}
	lastChatPoll := time.Time{}
	chat := &chatUI{}

	lastSizePoll := time.Time{} // poll the terminal size immediately, then every ~1.5s
	for {
		select {
		case <-ip.quitCh:
			logf("match exit: input channel closed")
			return true, false
		case <-sigCh:
			logf("match exit: signal")
			return true, false
		default:
		}
		now := time.Now()
		if pollResize(w, ip, rnd, &cols, &rows, &rows3d, &lastSizePoll, now) {
			prev = nil                        // full re-encode at the new size
			curMapSig = "?"                   // rebuild the arena geometry buffer
			lv.sig = ""                       // repaint the lobby preview
			lv.mask = make([]bool, rows*cols) // mask tracks the new grid
		}
		if now.Sub(lastPresence) >= 10*time.Second {
			updatePresence(dropfile, presenceState, presenceDetail)
			lastPresence = now
		}
		chatPoll := 5 * time.Second
		if lastPhase == gm.PhaseLobby { // snappier so vote toasts feel live in the lobby
			chatPoll = 2 * time.Second
		}
		if now.Sub(lastChatPoll) >= chatPoll {
			if st, err := arenaStatus(); err == nil {
				chat.ingest(st.Chat)
				chat.setWho(st.Presence)
				// Single-player: follow the party if a mate enters the arena. Bailing
				// here (before the match's Ended phase) means it records no stats.
				if netSess == nil && presenceState == "single-player" && autoJoinArmed &&
					partyMateInArena(st.Presence, presenceSession(dropfile)) {
					drawCenterNote(w, cols, rows, "Your party is in the arena - joining...")
					time.Sleep(1200 * time.Millisecond)
					return false, true
				}
			} else {
				logf("chat/status poll unavailable: %v", err)
			}
			lastChatPoll = now
		}
		if chat.prune() {
			prev = nil
		}
		select { // info-panel ping reply (CPR), if one is in flight
		case at := <-ip.cpr:
			if pingWait {
				infoRtt = at.Sub(pingSent)
				pingWait = false
			}
		default:
		}
		if pingWait && now.Sub(pingSent) > 3*time.Second {
			pingWait = false // no CPR support (or reply lost): back to "-"
			infoRtt = -time.Millisecond
		}
		dt := now.Sub(last).Seconds()
		if dt > 0.1 {
			dt = 0.1 // clamp after a stall so nothing teleports
		}
		last = now
		if escConfirm {
			if escT -= dt; escT <= 0 { // unanswered: quietly dismiss
				escConfirm = false
				prev = nil
			}
		}

		// Drain discrete nav events; in the lobby they cycle our mode vote.
	drainEvents:
		for {
			select {
			case k := <-ip.events:
				if chat.active {
					switch k {
					case mkChatToggle:
						chat.active, chat.cmd = false, false
						chat.input = ""
						prev = nil
					case mkTranscriptToggle:
						chat.transcript = !chat.transcript
						prev = nil
					case mkEnter:
						if msg := chat.submit(dropfile); msg != "" {
							chat.toasts = append(chat.toasts, chatToast{msg: proto.ChatMessage{Handle: "system", Text: msg}, until: time.Now().Add(20 * time.Second)})
						}
						if st, err := arenaStatus(); err == nil {
							chat.ingest(st.Chat)
							chat.setWho(st.Presence)
						}
						prev = nil
					case mkBack:
						chat.backspace()
					case mkEsc: // Esc closes chat compose (not the match)
						chat.active, chat.cmd = false, false
						chat.input = ""
						prev = nil
					}
					continue
				}
				if chat.transcript {
					switch k {
					case mkTranscriptToggle, mkEsc:
						chat.transcript = false
						prev = nil
					case mkChatToggle:
						chat.active = true
						prev = nil
					}
					continue
				}
				switch {
				case k == mkEsc: // toggle the exit confirm (second Esc = stay)
					escConfirm = !escConfirm
					escT = 5
					prev = nil
				case k == mkChatToggle:
					chat.active = !chat.active
					chat.cmd = false
					if !chat.active {
						chat.input = ""
					}
					prev = nil
				case k == mkCmdToggle:
					chat.active, chat.cmd, chat.input = true, true, ""
					prev = nil
				case k == mkTranscriptToggle:
					chat.transcript = !chat.transcript
					prev = nil
				case k == mkTab:
					topdown = !topdown
					prev = nil // view changed: full repaint
				case k == mkChangeChar:
					// Swap character while dead (online or single-player) or in the
					// online lobby - so you re-spawn as the new one, never a mid-fight
					// morph. The picker blocks the loop, so reset the frame clock after
					// it so the next step doesn't get a multi-second dt (which produced
					// garbage prediction and could crash the renderer).
					if pickChar != nil && (netSess != nil || offSess != nil) && (lastSelfDead || lastPhase == gm.PhaseLobby) {
						if bod, col, ok := pickChar(); ok {
							if netSess != nil {
								netSess.sendChangeChar(bod, col)
							} else {
								offSess.changeChar(bod, col)
							}
						}
						prev, lastSig = nil, "" // picker took the screen: force a full repaint
						last = time.Now()       // don't feed the blocked-picker gap to dt
					}
				case k == mkBack:
					return false, false // back out of the match to the menu / editor
				case lastPhase == gm.PhaseLobby && k == mkUp:
					voteMode, voteReady = lobbyMoveVote(lobbyPairings, voteFilter, voteMode, -1), false
				case lastPhase == gm.PhaseLobby && k == mkDown:
					voteMode, voteReady = lobbyMoveVote(lobbyPairings, voteFilter, voteMode, +1), false
				case lastPhase == gm.PhaseLobby && k == mkLeft:
					voteFilter, voteMode = lobbyMoveFilter(lobbyPairings, voteFilter, -1)
					voteReady = false
				case lastPhase == gm.PhaseLobby && k == mkRight:
					voteFilter, voteMode = lobbyMoveFilter(lobbyPairings, voteFilter, +1)
					voteReady = false
				case lastPhase == gm.PhaseLobby && k == mkEnter:
					if voteMode >= 0 { // lock this pick in (ENTER); all locked -> fast-forward
						voteCommit, voteReady = voteMode, true
					}
				case k == mkCruiseF: // Shift+W/A/S/D latches auto-movement in that direction
					cruiseDir = 1
				case k == mkCruiseB:
					cruiseDir = 2
				case k == mkCruiseL:
					cruiseDir = 3
				case k == mkCruiseR:
					cruiseDir = 4
				case k == mkCruiseSL:
					cruiseDir = 5
				case k == mkCruiseSR:
					cruiseDir = 6
				}
			default:
				break drainEvents
			}
		}
	drainRunes:
		for {
			select {
			case r := <-ip.runes:
				switch {
				case chat.active:
					chat.appendRune(r)
				case escConfirm && (r == 'y' || r == 'Y'):
					return false, false // leave the match: same path as Backspace
				case escConfirm && (r == 'n' || r == 'N'):
					escConfirm = false
					prev = nil
				case r == 'i' || r == 'I':
					infoPanel = !infoPanel
					prev = nil // full repaint adds/clears the strip cleanly
				case r == 'g' || r == 'G':
					dropFlag = true // CTF: set the carried flag down this tick
				}
			default:
				break drainRunes
			}
		}
		gin := ip.snapshot()
		if chat.active || chat.transcript {
			gin = gm.Input{}
		}
		// Lobby vote is the map you've LOCKED with ENTER (voteCommit), not the one
		// you're previewing (voteMode); voteReady drives the all-locked fast-forward.
		gin.Vote = voteCommit
		gin.Ready = voteReady
		gin.Drop, dropFlag = dropFlag, false // one-shot

		// Cruise control: disengage on an opposing-axis manual press, else latch the
		// movement so you can aim/fire while driving. Same-axis steering is allowed.
		switch cruiseDir {
		case 1:
			if gin.Reverse {
				cruiseDir = 0
			} else {
				gin.Throttle = true
			}
		case 2:
			if gin.Throttle {
				cruiseDir = 0
			} else {
				gin.Reverse = true
			}
		case 3:
			if gin.HullR {
				cruiseDir = 0
			} else {
				gin.HullL = true
			}
		case 4:
			if gin.HullL {
				cruiseDir = 0
			} else {
				gin.HullR = true
			}
		case 5: // strafe left
			if gin.StrafeR {
				cruiseDir = 0
			} else {
				gin.StrafeL = true
			}
		case 6: // strafe right
			if gin.StrafeL {
				cruiseDir = 0
			} else {
				gin.StrafeR = true
			}
		}

		v := sess.step(dt, gin)
		// Stats: time each match and record one result on the Active->Ended edge.
		if v.phase == gm.PhaseActive && lastPhase != gm.PhaseActive {
			matchStart = time.Now()
		}
		if v.phase == gm.PhaseEnded && lastPhase == gm.PhaseActive && presenceState != "playtest" && recordScores {
			r := matchResultFrom(v, statName, time.Since(matchStart).Seconds())
			r.Online = matchOnline
			r.Difficulty = matchDiff
			r.Score = scoreMatch(r)
			lastResult = &r
			resultRank = recordResultRanked(dropfile, r) // record + learn where it placed
			// Solo scores stay local; only real arena matches feed the global board.
			if r.Online && arenaConfigured() {
				go submitScore(r, statName)
			}
		}
		lastPhase = v.phase
		lastSelfDead = v.self.Dead
		lobbyN = len(v.pairings)
		lobbyPairings = v.pairings
		if v.phase != gm.PhaseLobby {
			lv.sig = "" // re-entering the lobby will then force a full preview repaint
			// Drop any locked vote once a match starts: the NEXT lobby must be voted
			// fresh, so a held lock can't carry over and instant-fast-forward the
			// between-rounds vote (which skips the 30s map pick). Cleared here, before
			// the lobby begins, so no stale Ready/Vote leaks on its first tick.
			voteCommit, voteReady = -1, false
		}
		if !v.ready { // no view yet (awaiting first STATE from the server)
			// Handshake succeeded but no usable STATE has arrived. Normally a
			// version guard rejects a mismatched arena up front, but an arena that
			// predates the guard (or wasn't restarted after an update) can accept
			// us and then send a wire format we can't decode - so guard against an
			// endless "Connecting..." by bailing with a hint.
			if time.Since(start) > 12*time.Second {
				w.WriteString("\x1b[2J\x1b[H")
				for i, line := range []string{
					"The arena accepted the connection but sent no usable data.",
					"It may be running an older version - ask the sysop to restart it.",
					"",
					"Press any key to return to the menu.",
				} {
					style := "\x1b[0;37m"
					if i == 0 {
						style = "\x1b[1;91m"
					} else if i == 3 {
						style = "\x1b[0;90m"
					}
					fmt.Fprintf(w, "\x1b[%d;%dH%s%s\x1b[0m", rows/2-2+i, (cols-len(line))/2+1, style, line)
				}
				w.Flush()
				select { // any key, disconnect, or a generous auto-dismiss
				case <-ip.quitCh:
					return true, false
				case <-ip.events:
				case <-ip.runes:
				case <-time.After(20 * time.Second):
				}
				return false, false
			}
			fmt.Fprint(w, "\x1b[2J\x1b[H\x1b[0;1;37m  Connecting to arena...\x1b[0m")
			w.Flush()
			prev = nil // force a full repaint on the first real frame
			if d := frameBudget - time.Since(now); d > 0 {
				time.Sleep(d)
			}
			continue
		}
		// Online vote lobby: a self-contained rotating map preview + vote overlays,
		// fetched lazily from the server. Bypasses the world render entirely.
		if ns, ok := sess.(*netSession); ok && v.phase == gm.PhaseLobby {
			if voteMode < 0 && lobbyN > 0 {
				voteMode = 0 // default the highlight to the first map so there's a preview
			}
			renderLobbyFrame(w, cols, rows, rows3d, rnd, ns, v, voteMode, voteCommit, voteFilter, voteReady, dt, chat, escConfirm, &lv)
			prev = nil // leaving the lobby forces a full game-view repaint
			if d := frameBudget - time.Since(now); d > 0 {
				time.Sleep(d)
			}
			continue
		}
		p := v.self
		if oneShot && oneShotAt.IsZero() {
			// The level is decided when the match ends, or the moment the player
			// is permanently out of lives (no elimination rule ends Flag Run).
			if v.phase == gm.PhaseEnded || (p.Dead && p.Lives <= 0) {
				oneShotAt = now
			}
		}
		if !oneShotAt.IsZero() && now.Sub(oneShotAt) > 3*time.Second {
			return false, false // back to the campaign runner with the outcome on the world
		}

		// Hide the border walls during the death-cam AND the count-in establishing
		// orbit, so they can't block the view (rebuild the arena on the transition).
		if wantHide := p.Dead || v.phase == gm.PhaseCountdown; wantHide != wallsHidden {
			wallsHidden = wantHide
			hideArenaWalls = wallsHidden
			curMapSig = "?" // force an arena rebuild with/without walls
		}

		// Kill feed: latch this tick's events into transient HUD state.
		for _, k := range v.kills {
			if k.Killer == v.me && k.Victim != v.me {
				killBanner, killBannerT = "KILLED "+tankName(v, k.Victim), 3.5
			}
			if k.Victim == v.me {
				deathBy = deathText(v, k)
				killerID = k.Killer // for the kill cam
			}
			if k.Killer >= 0 {
				recentKill[k.Killer] = 5.0
			}
		}
		if killBannerT > 0 {
			if killBannerT -= dt; killBannerT <= 0 {
				killBanner = ""
			}
		}
		// Author toasts (event system): latch the newest message for a few seconds.
		if len(v.events) > 0 {
			toast, toastT = v.events[len(v.events)-1], 4.0
		}
		if toastT > 0 {
			if toastT -= dt; toastT <= 0 {
				toast = ""
			}
		}
		for id := range recentKill {
			if recentKill[id] -= dt; recentKill[id] <= 0 {
				delete(recentKill, id)
			}
		}
		if !p.Dead {
			deathBy = ""
			killerID = -1
			replayStart = time.Time{}
			deadRec = 0
			replay = append(replay, replayFrame{v.tanks, v.shots, v.flags, v.pickups, v.ents})
		} else {
			cruiseDir = 0 // don't auto-drive out of a respawn
			// Keep recording a beat into death before freezing the buffer:
			// the killing hit and the explosion FX happen on the dead frames,
			// so a buffer frozen on the last alive tick never shows the kill.
			if deadRec < deadRecFrames {
				deadRec++
				replay = append(replay, replayFrame{v.tanks, v.shots, v.flags, v.pickups, v.ents})
			}
			if replayStart.IsZero() { // first dead frame: start playback
				replayStart = now
			}
		}
		if len(replay) > replayMax { // keep only the most recent ~3s
			copy(replay, replay[1:])
			replay = replay[:replayMax]
		}

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

		if pace.skipFrame() { // link saturated: keep playing the sim, skip drawing
			if d := frameBudget - time.Since(now); d > 0 {
				time.Sleep(d)
			}
			continue
		}

		// Death-cam replay: while dead, render a recorded frame (the lead-up to the
		// kill) instead of the live world, from a 3rd-person camera framing the killer.
		rtanks, rshots, rflags, rpickups, rents := v.tanks, v.shots, v.flags, v.pickups, v.ents
		renderMe := v.me
		inReplay := p.Dead && len(replay) > 0
		if inReplay {
			// 2/3 speed (20 fps): the full 90-frame buffer - death frames at its
			// tail - completes ~0.5s before the 5s human respawn, so the kill
			// itself shows and briefly holds instead of being cut off.
			idx := int(now.Sub(replayStart).Seconds() * 20)
			if idx >= len(replay) {
				idx = len(replay) - 1
			}
			rf := replay[idx]
			rtanks, rshots, rflags, rpickups, rents = rf.tanks, rf.shots, rf.flags, rf.pickups, rf.ents
			renderMe = -1 // show our own tank in the replay
		}
		if v.phase == gm.PhaseCountdown {
			renderMe = -1 // establishing orbit shows every tank, ours included
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
		} else if p.Dead {
			// Kill cam: 3rd-person from the (recorded) victim spot, framing the killer.
			self, _ := posInSnap(rtanks, v.me)
			target := self
			if kp, ok := posInSnap(rtanks, killerID); ok {
				target = kp
			}
			dx, dz := self.X-target.X, self.Z-target.Z
			n := math.Hypot(dx, dz)
			if n < 0.5 { // killer unknown / on top of us: slow orbit instead
				ang := now.Sub(start).Seconds() * 0.6
				dx, dz, n = math.Sin(ang), math.Cos(ang), 1
			}
			const camDist, camHigh = 7.0, 4.5
			cam.pos = V3{self.X + dx/n*camDist, self.Y + camHigh, self.Z + dz/n*camDist}
			lx, lz := target.X-cam.pos.X, target.Z-cam.pos.Z
			cam.yaw = math.Atan2(lx, lz)
			cam.pitch = math.Atan2(cam.pos.Y-(target.Y+0.8), math.Hypot(lx, lz)) // look down at the action
		} else if v.phase == gm.PhaseCountdown {
			// Count-in establishing shot: slowly orbit the whole arena (walls hidden,
			// below) instead of staring at whatever wall your spawn happens to face.
			cam = mapOrbitCam(arenaSize, now.Sub(start).Seconds()*0.35, rnd.W, rnd.H)
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
		rnd.renderWorld(cam, now.Sub(start).Seconds(), rtanks, rshots, rflags, rpickups, v.gmap.Entities, rents, v.zones, renderMe, flash, topdown, v.phase == gm.PhaseCountdown, reticle, v.viewTurret)
		frame, cur := encode(rnd, cols, rows3d, prev)
		prev = cur
		wstart := time.Now()
		w.Write(frame)

		// HUD overlays (drawn over the top-left sky; no bottom bar anymore)
		frames++
		if elapsed := now.Sub(fpsT); elapsed >= time.Second {
			// Normalize over the real interval: heavy frame-skipping stretches
			// the rollover past a second, and raw counts would overstate rates.
			fps = int(float64(frames) * float64(time.Second) / float64(elapsed))
			frames = 0
			fpsT = now
			tx := txBytes.Load()
			infoTx = (tx - txBase) * int64(time.Second) / int64(elapsed)
			txBase = tx
			infoWr = 0
			if wrN > 0 {
				infoWr = wrSum / time.Duration(wrN)
			}
			wrSum, wrN = 0, 0
			infoSk = pace.skips - skBase
			skBase = pace.skips
			logf("fps=%d phase=%d t=%.0f hp=%d kills=%d deaths=%d shots=%d", fps, v.phase, v.timer, p.HP, p.Kills, p.Deaths, len(v.shots))
		}
		switch v.phase {
		case gm.PhaseCountdown:
			drawCountdown(w, cols, rows, v)
		case gm.PhaseEnded:
			drawScoreboard(w, cols, rows, v, lastResult, resultRank)
		case gm.PhaseLobby:
			drawLobby(w, cols, rows, v, voteMode)
		default:
			if !topdown {
				drawRadarCorners(w, cols)
			}
			drawLeaderboard(w, cols, rows, v, recentKill)
			drawStatus(w, cols, v, &p) // mode + clock + mode-specifics, row 2 (below HP bar)
			if p.Dead {
				swapHint := ""
				if pickChar != nil && (netSess != nil || offSess != nil) {
					swapHint = "Press V to change character"
				}
				drawDeathBanner(w, cols, rows, p.RespawnIn, deathBy, swapHint)
			} else {
				drawHPBar(w, &p)
				if killBannerT > 0 {
					drawKillBanner(w, cols, killBanner)
				}
				if cruiseDir != 0 {
					names := []string{"^", "v", "<", ">", "STRAFE <", "STRAFE >"}
					tag := "CRUISE " + names[cruiseDir-1]
					fmt.Fprintf(w, "\x1b[4;%dH\x1b[1;96m%s\x1b[0m", cols-len(tag), tag)
				}
			}
			if toastT > 0 { // author message (shows whether alive or dead)
				drawToast(w, cols, toast)
			}
		}
		if infoPanel {
			drawInfoPanel(w, cols, fps, infoTx, infoWr, infoSk, infoRtt)
		}
		if escConfirm {
			drawEscConfirm(w, cols)
		}
		drawChat(w, cols, rows, chat, nil)
		if infoPanel && !pingWait && now.Sub(lastPing) >= 2*time.Second {
			w.WriteString("\x1b[6n") // CPR ping rides this frame; reader clocks the reply
			pingSent, lastPing, pingWait = time.Now(), now, true
		}
		w.Flush()
		wd := time.Since(wstart)
		pace.note(wd)
		wrSum += wd
		wrN++

		if d := frameBudget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
}
