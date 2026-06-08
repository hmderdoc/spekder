package main

import (
	"bufio"
	"fmt"
	"math"
	"time"

	gm "spekder/internal/game"
)

// ---------------------------------------------------------------------------
// asset catalog (palette)
// ---------------------------------------------------------------------------

// catItem is one placeable asset: a render hint + footprint for the ghost preview,
// and a placer that appends the real object to the map at a (snapped, floor) point.
type catItem struct {
	name  string
	kind  string // ghost render kind ("turret"/"trampoline"/"hazard"/... else a box)
	half  gm.V3  // ghost footprint
	place func(m *gm.Map, p gm.V3)
}

var (
	colWall = [3]float64{0.44, 0.42, 0.48}
	colPlat = [3]float64{0.40, 0.44, 0.52}
	colHaz  = [3]float64{0.85, 0.28, 0.12}
	colTele = [3]float64{0.30, 0.70, 0.90}
	colTram = [3]float64{0.30, 0.80, 0.55}
	colTurr = [3]float64{0.55, 0.50, 0.30}
	colZone = [3]float64{0.55, 0.55, 0.60}
)

var catItems = []catItem{
	{"WALL", "wall", gm.V3{X: 1, Y: 1, Z: 1}, func(m *gm.Map, p gm.V3) {
		m.Obstacles = append(m.Obstacles, gm.Box{Pos: gm.V3{X: p.X, Y: 1, Z: p.Z}, Half: gm.V3{X: 1, Y: 1, Z: 1}, Color: colWall})
	}},
	{"PLATFORM", "wall", gm.V3{X: 3, Y: 0.4, Z: 3}, func(m *gm.Map, p gm.V3) {
		m.Obstacles = append(m.Obstacles, gm.Box{Pos: gm.V3{X: p.X, Y: 0.4, Z: p.Z}, Half: gm.V3{X: 3, Y: 0.4, Z: 3}, Color: colPlat})
	}},
	{"RAMP", "wall", gm.V3{X: 2.5, Y: 1.5, Z: 3}, func(m *gm.Map, p gm.V3) {
		m.Ramps = append(m.Ramps, gm.Ramp{Pos: gm.V3{X: p.X, Z: p.Z}, Half: gm.V3{X: 2.5, Z: 3}, H: 3, Dir: 0, Color: colPlat})
	}},
	{"TURRET", "turret", gm.V3{X: 0.7, Y: 0.7, Z: 0.7}, func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "turret", Pos: gm.V3{X: p.X, Y: 0.7, Z: p.Z}, Half: gm.V3{X: 0.7, Y: 0.7, Z: 0.7},
			Color: colTurr, Solid: true, Turret: &gm.TurretTrait{Range: 22, FireDelay: 1.4, Dmg: 14, TurnRate: 1.6},
			Destruct: &gm.DestructTrait{MaxHP: 60}, Respawn: &gm.RespawnTrait{Delay: 14}})
	}},
	{"HAZARD", "hazard", gm.V3{X: 2, Y: 0.2, Z: 2}, func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "hazard", Pos: gm.V3{X: p.X, Y: 0.2, Z: p.Z}, Half: gm.V3{X: 2, Y: 0.2, Z: 2},
			Color: colHaz, Hazard: &gm.HazardTrait{DPS: 20}})
	}},
	{"TELEPORTER", "teleporter", gm.V3{X: 1, Y: 0.2, Z: 1}, func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "teleporter", Pos: gm.V3{X: p.X, Y: 0.2, Z: p.Z}, Half: gm.V3{X: 1, Y: 0.2, Z: 1},
			Color: colTele, Teleport: &gm.TeleportTrait{Dest: gm.V3{X: -p.X, Z: -p.Z}, Cooldown: 1.5}})
	}},
	{"TRAMPOLINE", "trampoline", gm.V3{X: 1.5, Y: 0.2, Z: 1.5}, func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "trampoline", Pos: gm.V3{X: p.X, Y: 0.2, Z: p.Z}, Half: gm.V3{X: 1.5, Y: 0.2, Z: 1.5},
			Color: colTram, Bounce: &gm.BounceTrait{Power: 13}})
	}},
	{"FLAG (NEUTRAL)", "flag", gm.V3{X: 0.5, Y: 0.5, Z: 0.5}, placeFlag(-1)},
	{"FLAG (RED)", "flag", gm.V3{X: 0.5, Y: 0.5, Z: 0.5}, placeFlag(0)},
	{"FLAG (BLUE)", "flag", gm.V3{X: 0.5, Y: 0.5, Z: 0.5}, placeFlag(1)},
	{"ZONE (HILL)", "zone", gm.V3{X: 4, Y: 1, Z: 4}, func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "zone", Pos: gm.V3{X: p.X, Z: p.Z}, Half: gm.V3{X: 4, Y: 1, Z: 4},
			Color: colZone, Zone: &gm.ZoneTrait{Capture: 4}})
	}},
	{"SPAWN POINT", "wall", gm.V3{X: 0.6, Y: 0.6, Z: 0.6}, func(m *gm.Map, p gm.V3) {
		m.Spawns = append(m.Spawns, gm.V3{X: p.X, Z: p.Z})
	}},
	{"PICKUP SPOT", "wall", gm.V3{X: 0.4, Y: 0.4, Z: 0.4}, func(m *gm.Map, p gm.V3) {
		m.Pickups = append(m.Pickups, gm.V3{X: p.X, Z: p.Z})
	}},
}

func placeFlag(team int) func(m *gm.Map, p gm.V3) {
	return func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "flag", Pos: gm.V3{X: p.X, Z: p.Z}, Half: gm.V3{X: 0.5, Y: 0.5, Z: 0.5},
			Flag: &gm.FlagTrait{Team: team}})
	}
}

// ---------------------------------------------------------------------------
// editor loop
// ---------------------------------------------------------------------------

const (
	edNav = iota
	edPalette
	edPlace
)

// runEditor is the in-door map editor (Phase C). Stages 1-2: free-fly / top-down
// views, a catalog palette, ghost preview, and placement (3D gun-sight raycast or
// top-down cursor). Backspace backs out of each mode (and exits to the menu from
// nav); Q quits the program.
func runEditor(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input) {
	m := gm.Map{Name: "UNTITLED", Size: 20, Spawns: []gm.V3{{X: -14, Z: -14}, {X: 14, Z: 14}}}
	buildArena(m)

	cam := Cam{pos: gm.V3{X: 0, Y: 14, Z: -24}, yaw: 0, pitch: 0.5}
	topdown := false
	mode := edNav
	palIdx := 0       // palette selection
	itemIdx := 0      // chosen catalog item for placing
	cursor := gm.V3{} // top-down placement cursor
	const grid = 1.0
	const flySpeed, lookRate, cursorSpeed = 16.0, 1.7, 18.0
	var prev []byte
	prevFire := false

	rebuild := func() { buildArena(m); prev = nil }

	w.WriteString("\x1b[2J\x1b[H")
	frameBudget := time.Second / 30
	start := time.Now()
	last := start
	for {
		select {
		case <-ip.quitCh:
			return
		default:
		}
		now := time.Now()
		dt := now.Sub(last).Seconds()
		if dt > 0.1 {
			dt = 0.1
		}
		last = now

		// Discrete events: mode transitions + palette navigation + view toggle.
	drain:
		for {
			select {
			case k := <-ip.events:
				switch k {
				case mkTab:
					topdown = !topdown
					prev = nil
				case mkBack:
					switch mode {
					case edNav:
						return // exit editor
					default:
						mode = edNav
						prev = nil
					}
				case mkEnter:
					switch mode {
					case edNav, edPlace:
						mode = edPalette
						palIdx = itemIdx
						prev = nil
					case edPalette:
						itemIdx = palIdx
						mode = edPlace
						prev = nil
					}
				case mkUp:
					if mode == edPalette {
						palIdx = (palIdx - 1 + len(catItems)) % len(catItems)
					}
				case mkDown:
					if mode == edPalette {
						palIdx = (palIdx + 1) % len(catItems)
					}
				}
			default:
				break drain
			}
		}

		in := ip.snapshot()

		// Camera / cursor movement (not while the palette is open).
		if mode != edPalette {
			if topdown && mode == edPlace {
				// Move the placement cursor on the X/Z plane.
				s := cursorSpeed * dt
				if in.TurretR {
					cursor.X += s
				}
				if in.TurretL {
					cursor.X -= s
				}
				if in.AimDown {
					cursor.Z -= s
				}
				if in.AimUp {
					cursor.Z += s
				}
				lim := arenaSize
				cursor.X = clampF(cursor.X, -lim, lim)
				cursor.Z = clampF(cursor.Z, -lim, lim)
			} else if !topdown {
				flyCam(&cam, in, flySpeed, lookRate, dt)
				step := flySpeed * dt // R/F rise/fall (works in nav and place; SPACE = drop)
				if ip.held(aEdUp) {
					cam.pos.Y += step
				}
				if ip.held(aEdDown) {
					cam.pos.Y -= step
				}
				cam.pos.Y = clampF(cam.pos.Y, 1, 80)
			}
		}

		// Where would a placed item land? Top-down: the cursor. 3D: floor raycast.
		ghost := cursor
		if mode == edPlace && !topdown {
			ghost = raycastFloor(cam)
			cursor = ghost
		}
		ghost = snap(ghost, grid)

		// Place on a fresh SPACE press (edge-triggered) while in place mode.
		if mode == edPlace && in.Fire && !prevFire {
			catItems[itemIdx].place(&m, ghost)
			rebuild()
		}
		prevFire = in.Fire

		// --- render ---
		rc := cam
		if topdown {
			a := arenaSize + 2
			camY := a
			if r := float64(rnd.W) / float64(rnd.H); r > 1 {
				camY = a * r
			}
			rc = Cam{pos: gm.V3{Y: camY}, yaw: 0, pitch: math.Pi / 2}
		}
		entT := m.Entities
		entS := editorEntSnaps(m)
		if mode == edPlace { // append the ghost preview
			it := catItems[itemIdx]
			g := gm.Entity{Kind: it.kind, Pos: gm.V3{X: ghost.X, Y: it.half.Y, Z: ghost.Z}, Half: it.half, Color: [3]float64{0.45, 0.95, 1.0}}
			entT = append(append([]gm.Entity(nil), m.Entities...), g)
			entS = append(entS, gm.EntitySnap{Yaw: 0})
		}
		reticle := [3]byte{120, 220, 140}
		rnd.renderWorld(rc, now.Sub(start).Seconds(), nil, nil, nil, nil,
			entT, entS, editorZoneSnaps(m), -1, 0, topdown, reticle, 0)
		frame, cur := encode(rnd, cols, rows3d, prev)
		prev = cur
		w.Write(frame)

		drawEditorBar(w, cols, rows, m, topdown, mode, itemIdx, ghost)
		if mode == edPalette {
			drawPalette(w, cols, rows, palIdx)
		}
		w.Flush()

		if d := frameBudget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
}

// flyCam applies free-fly move + look to the 3D camera from held input (W/S/A/D
// + ,/. and arrows). Vertical (R/F) is handled by the caller.
func flyCam(cam *Cam, in gm.Input, fly, look, dt float64) {
	if in.TurretL {
		cam.yaw -= look * dt
	}
	if in.TurretR {
		cam.yaw += look * dt
	}
	if in.AimUp {
		cam.pitch -= look * dt
	}
	if in.AimDown {
		cam.pitch += look * dt
	}
	cam.pitch = clampF(cam.pitch, -1.35, 1.35)
	sy, cy := math.Sin(cam.yaw), math.Cos(cam.yaw)
	step := fly * dt
	if in.Throttle {
		cam.pos.X += sy * step
		cam.pos.Z += cy * step
	}
	if in.Reverse {
		cam.pos.X -= sy * step
		cam.pos.Z -= cy * step
	}
	if in.HullR {
		cam.pos.X += cy * step
		cam.pos.Z -= sy * step
	}
	if in.HullL {
		cam.pos.X -= cy * step
		cam.pos.Z += sy * step
	}
}

// raycastFloor returns where the camera's forward ray meets the floor (y=0),
// capped in reach; if looking level/up it projects a fixed distance ahead.
func raycastFloor(cam Cam) gm.V3 {
	cosp := math.Cos(cam.pitch)
	fx, fz := math.Sin(cam.yaw)*cosp, math.Cos(cam.yaw)*cosp
	fy := -math.Sin(cam.pitch) // pitch>0 looks down -> fy<0
	if fy < -1e-3 {
		t := cam.pos.Y / -fy
		if t > 45 {
			t = 45
		}
		return gm.V3{X: cam.pos.X + fx*t, Z: cam.pos.Z + fz*t}
	}
	return gm.V3{X: cam.pos.X + fx*14, Z: cam.pos.Z + fz*14}
}

func snap(p gm.V3, g float64) gm.V3 {
	if g <= 0 {
		return p
	}
	return gm.V3{X: math.Round(p.X/g) * g, Y: p.Y, Z: math.Round(p.Z/g) * g}
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func editorEntSnaps(m gm.Map) []gm.EntitySnap {
	out := make([]gm.EntitySnap, len(m.Entities))
	for i, e := range m.Entities {
		hp := 0
		if e.Destruct != nil {
			hp = e.Destruct.MaxHP
		}
		out[i] = gm.EntitySnap{HP: hp, Yaw: e.Yaw, Pitch: e.Pitch}
	}
	return out
}

func editorZoneSnaps(m gm.Map) []gm.ZoneSnap {
	var out []gm.ZoneSnap
	for _, e := range m.Entities {
		if e.Zone != nil {
			out = append(out, gm.ZoneSnap{Pos: gm.V3{X: e.Pos.X, Z: e.Pos.Z}, Half: e.Half, Color: [3]float64{0.55, 0.55, 0.6}})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// editor HUD
// ---------------------------------------------------------------------------

func drawEditorBar(w *bufio.Writer, cols, rows int, m gm.Map, topdown bool, mode, itemIdx int, ghost gm.V3) {
	view := "3D"
	if topdown {
		view = "TOP"
	}
	// Width capped at cols-1: never touch the bottom-right cell (autowrap scroll).
	width := cols - 1
	top := fmt.Sprintf(" EDIT %s  [%s]  obst:%d ent:%d spawn:%d ", m.Name, view, len(m.Obstacles), len(m.Entities), len(m.Spawns))
	fmt.Fprintf(w, "\x1b[1;1H\x1b[1;30;46m%-*s\x1b[0m", width, clip(top, width))

	var help string
	switch mode {
	case edPalette:
		help = "up/dn pick   ENTER choose   BKSP cancel"
	case edPlace:
		help = fmt.Sprintf("PLACING %s @ %.0f,%.0f   SPACE drop  R/F up/down  ENTER palette  TAB view  BKSP done",
			catItems[itemIdx].name, ghost.X, ghost.Z)
	default:
		help = "WASD fly  ,/. + arrows look  R/F up/down  ENTER catalog  TAB view  BKSP exit"
	}
	fmt.Fprintf(w, "\x1b[%d;1H\x1b[0;90m%s\x1b[0m", rows, clip(centered(help, width), width))
}

// drawPalette overlays the catalog list, highlighting the current selection.
func drawPalette(w *bufio.Writer, cols, rows, sel int) {
	title := "CATALOG"
	row := rows/2 - len(catItems)/2 - 1
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;97m%s\x1b[0m", row, (cols-len(title))/2+1, title)
	row += 2
	for i, it := range catItems {
		style := "\x1b[0;36m"
		marker := "  "
		if i == sel {
			style, marker = "\x1b[1;30;46m", "> "
		}
		line := marker + it.name
		fmt.Fprintf(w, "\x1b[%d;%dH%s  %s  \x1b[0m", row+i, (cols-len(line)-4)/2+1, style, line)
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func centered(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return fmt.Sprintf("%*s%s", (n-len(s))/2, "", s)
}
