package main

import (
	"bufio"
	"fmt"
	"math"
	"time"

	gm "spekder/internal/game"
)

// runEditor is the in-door map editor (Phase C). Stage 1: a working map rendered
// in 3D (free-fly camera) and top-down (TAB), with a status/help bar. Placing and
// editing assets land in later stages. Backspace exits to the menu; Q quits.
func runEditor(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input) {
	m := gm.Map{Name: "UNTITLED", Size: 20, Spawns: []gm.V3{{X: -14, Z: -14}, {X: 14, Z: 14}}}
	buildArena(m)

	cam := Cam{pos: gm.V3{X: 0, Y: 14, Z: -24}, yaw: 0, pitch: 0.5}
	topdown := false
	var prev []byte
	const flySpeed = 16.0
	const lookRate = 1.7

	w.WriteString("\x1b[2J\x1b[H")
	frameBudget := time.Second / 30
	start := time.Now()
	last := start
	for {
		select {
		case <-ip.quitCh:
			return // Q / Ctrl-C: bubble up (program quits)
		default:
		}
		now := time.Now()
		dt := now.Sub(last).Seconds()
		if dt > 0.1 {
			dt = 0.1
		}
		last = now

		exit := false
	drain:
		for {
			select {
			case k := <-ip.events:
				switch k {
				case mkTab:
					topdown = !topdown
					prev = nil
				case mkBack:
					exit = true
				}
			default:
				break drain
			}
		}
		if exit {
			return
		}

		in := ip.snapshot()
		if !topdown { // free-fly the 3D camera
			if in.TurretL {
				cam.yaw -= lookRate * dt
			}
			if in.TurretR {
				cam.yaw += lookRate * dt
			}
			if in.AimUp {
				cam.pitch -= lookRate * dt // pitch < 0 looks up (matches projection)
			}
			if in.AimDown {
				cam.pitch += lookRate * dt
			}
			cam.pitch = clampF(cam.pitch, -1.35, 1.35)
			sy, cy := math.Sin(cam.yaw), math.Cos(cam.yaw)
			step := flySpeed * dt
			if in.Throttle { // forward
				cam.pos.X += sy * step
				cam.pos.Z += cy * step
			}
			if in.Reverse {
				cam.pos.X -= sy * step
				cam.pos.Z -= cy * step
			}
			if in.HullR { // strafe right
				cam.pos.X += cy * step
				cam.pos.Z -= sy * step
			}
			if in.HullL {
				cam.pos.X -= cy * step
				cam.pos.Z += sy * step
			}
			if in.Fire { // SPACE: up
				cam.pos.Y += step
			}
			if in.Jump { // ENTER: down
				cam.pos.Y -= step
			}
			cam.pos.Y = clampF(cam.pos.Y, 1, 80)
		}

		rc := cam
		if topdown {
			a := arenaSize + 2
			camY := a
			if r := float64(rnd.W) / float64(rnd.H); r > 1 {
				camY = a * r
			}
			rc = Cam{pos: gm.V3{Y: camY}, yaw: 0, pitch: math.Pi / 2}
		}
		reticle := [3]byte{120, 220, 140}
		rnd.renderWorld(rc, now.Sub(start).Seconds(), nil, nil, nil, nil,
			m.Entities, editorEntSnaps(m), editorZoneSnaps(m), -1, 0, topdown, reticle, 0)
		frame, cur := encode(rnd, cols, rows3d, prev)
		prev = cur
		w.Write(frame)
		drawEditorBar(w, cols, rows, m, topdown)
		w.Flush()

		if d := frameBudget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
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

// editorEntSnaps builds preview snapshots for a map's entities (full HP, authored
// facing) so turrets/walls render as they will in game.
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

// editorZoneSnaps previews King-of-the-Hill zones placed in the map (neutral).
func editorZoneSnaps(m gm.Map) []gm.ZoneSnap {
	var out []gm.ZoneSnap
	for _, e := range m.Entities {
		if e.Zone != nil {
			out = append(out, gm.ZoneSnap{Pos: gm.V3{X: e.Pos.X, Z: e.Pos.Z}, Half: e.Half, Color: [3]float64{0.55, 0.55, 0.6}})
		}
	}
	return out
}

// drawEditorBar paints the editor's status/help line(s): map name + view, and the
// control hints.
func drawEditorBar(w *bufio.Writer, cols, rows int, m gm.Map, topdown bool) {
	view := "3D"
	if topdown {
		view = "TOP-DOWN"
	}
	top := fmt.Sprintf(" EDIT: %s   [%s]   %d obstacles  %d entities ", m.Name, view, len(m.Obstacles), len(m.Entities))
	fmt.Fprintf(w, "\x1b[1;1H\x1b[1;30;46m%-*s\x1b[0m", cols, clip(top, cols))
	help := "WASD fly  ,/. look  up/dn pitch  SPACE/ENTER rise/fall  TAB view  BKSP exit"
	fmt.Fprintf(w, "\x1b[%d;1H\x1b[0;90m%s\x1b[0m", rows, clip(centered(help, cols), cols))
}

// clip truncates s to at most n runes; centered pads s to width n centered.
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
	pad := (n - len(s)) / 2
	return fmt.Sprintf("%*s%s", pad, "", s)
}
