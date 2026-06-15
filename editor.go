package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
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
	// dragMake (nil = not draggable) builds an object spanning two points a->b, for
	// extended walls / platforms: drag a line for a wall, a rectangle for a floor.
	dragMake func(m *gm.Map, a, b gm.V3)
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

// Placers take p = the surface point under the cursor (p.Y = top of any obstacle/
// ramp there, else 0), so things stack on platforms/ramps instead of the floor.
// WALL/PLATFORM also support drag (dragMake): SPACE to anchor, SPACE to span.
var catItems = []catItem{
	{name: "WALL", kind: "wall", half: gm.V3{X: 1, Y: 1, Z: 1}, dragMake: spanBox(1, 0.5, colWall),
		place: func(m *gm.Map, p gm.V3) {
			m.Obstacles = append(m.Obstacles, gm.Box{Pos: gm.V3{X: p.X, Y: p.Y + 1, Z: p.Z}, Half: gm.V3{X: 1, Y: 1, Z: 1}, Color: colWall})
		}},
	{name: "PLATFORM", kind: "wall", half: gm.V3{X: 3, Y: 0.4, Z: 3}, dragMake: spanBox(0.4, 1.5, colPlat),
		place: func(m *gm.Map, p gm.V3) {
			m.Obstacles = append(m.Obstacles, gm.Box{Pos: gm.V3{X: p.X, Y: p.Y + 0.4, Z: p.Z}, Half: gm.V3{X: 3, Y: 0.4, Z: 3}, Color: colPlat})
		}},
	{name: "RAMP", kind: "wall", half: gm.V3{X: 2.5, Y: 1.5, Z: 3}, place: func(m *gm.Map, p gm.V3) {
		m.Ramps = append(m.Ramps, gm.Ramp{Pos: gm.V3{X: p.X, Y: p.Y, Z: p.Z}, Half: gm.V3{X: 2.5, Z: 3}, H: 3, Dir: 0, Color: colPlat})
	}},
	{name: "TURRET", kind: "turret", half: gm.V3{X: 0.7, Y: 0.7, Z: 0.7}, place: func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "turret", Pos: gm.V3{X: p.X, Y: p.Y + 0.7, Z: p.Z}, Half: gm.V3{X: 0.7, Y: 0.7, Z: 0.7},
			Color: colTurr, Solid: true, Turret: &gm.TurretTrait{Range: 22, FireDelay: 1.4, Dmg: 14, TurnRate: 1.6},
			Destruct: &gm.DestructTrait{MaxHP: 60}, Respawn: &gm.RespawnTrait{Delay: 14}})
	}},
	{name: "HAZARD", kind: "hazard", half: gm.V3{X: 2, Y: 0.2, Z: 2}, place: func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "hazard", Pos: gm.V3{X: p.X, Y: p.Y + 0.2, Z: p.Z}, Half: gm.V3{X: 2, Y: 0.2, Z: 2},
			Color: colHaz, Hazard: &gm.HazardTrait{DPS: 20}})
	}},
	{name: "TELEPORTER", kind: "teleporter", half: gm.V3{X: 1, Y: 0.2, Z: 1}, place: func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "teleporter", Pos: gm.V3{X: p.X, Y: p.Y + 0.2, Z: p.Z}, Half: gm.V3{X: 1, Y: 0.2, Z: 1},
			Color: colTele, Teleport: &gm.TeleportTrait{Dest: gm.V3{X: -p.X, Z: -p.Z}, Cooldown: 1.5}})
	}},
	{name: "TRAMPOLINE", kind: "trampoline", half: gm.V3{X: 1.5, Y: 0.2, Z: 1.5}, place: func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "trampoline", Pos: gm.V3{X: p.X, Y: p.Y + 0.2, Z: p.Z}, Half: gm.V3{X: 1.5, Y: 0.2, Z: 1.5},
			Color: colTram, Bounce: &gm.BounceTrait{Power: 13}})
	}},
	{name: "FLAG (NEUTRAL)", kind: "flag", half: gm.V3{X: 0.5, Y: 0.5, Z: 0.5}, place: placeFlag(-1)},
	{name: "FLAG (RED)", kind: "flag", half: gm.V3{X: 0.5, Y: 0.5, Z: 0.5}, place: placeFlag(0)},
	{name: "FLAG (BLUE)", kind: "flag", half: gm.V3{X: 0.5, Y: 0.5, Z: 0.5}, place: placeFlag(1)},
	{name: "ZONE (HILL)", kind: "zone", half: gm.V3{X: 4, Y: 1, Z: 4}, place: func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "zone", Pos: gm.V3{X: p.X, Y: p.Y, Z: p.Z}, Half: gm.V3{X: 4, Y: 1, Z: 4},
			Color: colZone, Zone: &gm.ZoneTrait{Capture: 4}})
	}},
	{name: "TRIGGER", kind: "trigger", half: gm.V3{X: 2, Y: 1, Z: 2}, place: func(m *gm.Map, p gm.V3) {
		m.Entities = append(m.Entities, gm.Entity{Kind: "trigger", Pos: gm.V3{X: p.X, Y: p.Y + 0.05, Z: p.Z}, Half: gm.V3{X: 2, Y: 1, Z: 2},
			Color: colZone, Trigger: &gm.TriggerTrait{}}) // select it + ENTER to add entered/exited behaviors
	}},
	{name: "PATH POINT", kind: "wall", half: gm.V3{X: 0.4, Y: 0.4, Z: 0.4}, place: func(m *gm.Map, p gm.V3) {
		if len(m.Paths) == 0 {
			m.Paths = append(m.Paths, gm.Path{Name: "main"}) // single default path for the move action
		}
		m.Paths[0].Points = append(m.Paths[0].Points, gm.V3{X: p.X, Z: p.Z})
	}},
	{name: "SPAWN POINT", kind: "wall", half: gm.V3{X: 0.6, Y: 0.6, Z: 0.6}, place: func(m *gm.Map, p gm.V3) {
		m.Spawns = append(m.Spawns, gm.V3{X: p.X, Z: p.Z})
	}},
	{name: "PICKUP SPOT", kind: "wall", half: gm.V3{X: 0.4, Y: 0.4, Z: 0.4}, place: func(m *gm.Map, p gm.V3) {
		m.Pickups = append(m.Pickups, gm.MapPickup{Pos: gm.V3{X: p.X, Z: p.Z}, Kind: -1}) // -1 = any (set in edit panel)
	}},
}

// spanBox returns a dragMake that adds a box spanning a->b with the given half-
// height and edge padding (so dragging a line makes a wall, a rectangle a floor).
func spanBox(halfY, pad float64, col [3]float64) func(m *gm.Map, a, b gm.V3) {
	return func(m *gm.Map, a, b gm.V3) {
		cx, cz := (a.X+b.X)/2, (a.Z+b.Z)/2
		hx := math.Abs(b.X-a.X)/2 + pad
		hz := math.Abs(b.Z-a.Z)/2 + pad
		base := math.Max(a.Y, b.Y)
		m.Obstacles = append(m.Obstacles, gm.Box{Pos: gm.V3{X: cx, Y: base + halfY, Z: cz}, Half: gm.V3{X: hx, Y: halfY, Z: hz}, Color: col})
	}
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
	edFile   // file menu: save / load / new
	edName   // typing a map name (to save)
	edLoad   // picking a usermap to load
	edSelect // selecting/editing a placed object
	edRules  // editing the map's victory conditions
)

var fileMenu = []string{"SAVE", "LOAD", "PLAYTEST", "PUBLISH", "RULES", "LOGIC", "ACTORS", "NEW", "BACK"}

var ruleFields = []string{"mode", "time", "target", "lives", "bots"}

// ---------------------------------------------------------------------------
// selection + field editing
// ---------------------------------------------------------------------------

const (
	selObstacle = iota
	selRamp
	selEntity
	selSpawn
	selPickup
	selPath // a waypoint in the default path (m.Paths[0])
)

// selRef points at one placed object across the map's heterogeneous slices.
type selRef struct {
	kind, idx int
}

// listSelectables returns every placed object in a stable order for cycling.
func listSelectables(m gm.Map) []selRef {
	var out []selRef
	for i := range m.Entities {
		out = append(out, selRef{selEntity, i})
	}
	for i := range m.Obstacles {
		out = append(out, selRef{selObstacle, i})
	}
	for i := range m.Ramps {
		out = append(out, selRef{selRamp, i})
	}
	for i := range m.Spawns {
		out = append(out, selRef{selSpawn, i})
	}
	for i := range m.Pickups {
		out = append(out, selRef{selPickup, i})
	}
	if len(m.Paths) > 0 {
		for i := range m.Paths[0].Points {
			out = append(out, selRef{selPath, i})
		}
	}
	return out
}

// selPos is the world position of a selected object (for the highlight marker).
func selPos(m *gm.Map, r selRef) gm.V3 {
	switch r.kind {
	case selObstacle:
		return m.Obstacles[r.idx].Pos
	case selRamp:
		return m.Ramps[r.idx].Pos
	case selEntity:
		return m.Entities[r.idx].Pos
	case selSpawn:
		return m.Spawns[r.idx]
	case selPickup:
		return m.Pickups[r.idx].Pos
	case selPath:
		return m.Paths[0].Points[r.idx]
	}
	return gm.V3{}
}

// selLabel names the selected object for the edit panel.
func selLabel(m *gm.Map, r selRef) string {
	switch r.kind {
	case selObstacle:
		return "WALL/BOX"
	case selRamp:
		return "RAMP"
	case selEntity:
		return strings.ToUpper(m.Entities[r.idx].Kind)
	case selSpawn:
		return "SPAWN"
	case selPickup:
		return "PICKUP"
	case selPath:
		return fmt.Sprintf("PATH PT %d", r.idx)
	}
	return "?"
}

// deleteSel removes the selected object from its slice.
func deleteSel(m *gm.Map, r selRef) {
	switch r.kind {
	case selObstacle:
		m.Obstacles = append(m.Obstacles[:r.idx], m.Obstacles[r.idx+1:]...)
	case selRamp:
		m.Ramps = append(m.Ramps[:r.idx], m.Ramps[r.idx+1:]...)
	case selEntity:
		m.Entities = append(m.Entities[:r.idx], m.Entities[r.idx+1:]...)
	case selSpawn:
		m.Spawns = append(m.Spawns[:r.idx], m.Spawns[r.idx+1:]...)
	case selPickup:
		m.Pickups = append(m.Pickups[:r.idx], m.Pickups[r.idx+1:]...)
	case selPath:
		p := m.Paths[0].Points
		m.Paths[0].Points = append(p[:r.idx], p[r.idx+1:]...)
	}
}

// editField is one tweakable value of a selected object.
type editField struct {
	name string
	get  func() float64
	set  func(float64)
	step float64
}

func vecFields(label string, p *gm.V3, includeY bool) []editField {
	f := []editField{
		{label + "x", func() float64 { return p.X }, func(v float64) { p.X = v }, 0.5},
	}
	if includeY {
		f = append(f, editField{label + "y", func() float64 { return p.Y }, func(v float64) { p.Y = v }, 0.25})
	}
	f = append(f, editField{label + "z", func() float64 { return p.Z }, func(v float64) { p.Z = v }, 0.5})
	return f
}

// fieldsFor returns the editable fields of the selected object (position, size,
// and any trait params). Pointers into the map slices are valid because editing
// never appends (so the backing arrays don't move).
func fieldsFor(m *gm.Map, r selRef) []editField {
	switch r.kind {
	case selObstacle:
		b := &m.Obstacles[r.idx]
		f := vecFields("pos", &b.Pos, true)
		return append(f, vecFields("half", &b.Half, true)...)
	case selRamp:
		rp := &m.Ramps[r.idx]
		f := vecFields("pos", &rp.Pos, false)
		f = append(f, vecFields("half", &rp.Half, false)...)
		f = append(f,
			editField{"height", func() float64 { return rp.H }, func(v float64) { rp.H = v }, 0.5},
			editField{"dir(0-3)", func() float64 { return float64(rp.Dir) }, func(v float64) { rp.Dir = ((int(v) % 4) + 4) % 4 }, 1})
		return f
	case selSpawn:
		return vecFields("pos", &m.Spawns[r.idx], false)
	case selPath:
		return vecFields("pos", &m.Paths[0].Points[r.idx], false)
	case selPickup:
		pk := &m.Pickups[r.idx]
		f := vecFields("pos", &pk.Pos, false)
		return append(f,
			editField{"kind", func() float64 { return float64(pk.Kind) }, func(v float64) { pk.Kind = clampI(int(v), -1, gm.PickWeapon) }, 1},
			editField{"weapon", func() float64 { return float64(pk.Weapon) }, func(v float64) { pk.Weapon = clampI(int(v), 0, len(gm.Weapons)-1) }, 1})
	case selEntity:
		e := &m.Entities[r.idx]
		f := vecFields("pos", &e.Pos, true)
		f = append(f, vecFields("half", &e.Half, true)...)
		if t := e.Turret; t != nil {
			f = append(f,
				editField{"weapon", func() float64 { return float64(e.Weapon) }, func(v float64) { e.Weapon = clampI(int(v), 0, len(gm.Weapons)-1) }, 1},
				editField{"range", func() float64 { return t.Range }, func(v float64) { t.Range = maxF(v, 0) }, 1},
				editField{"dmg", func() float64 { return float64(t.Dmg) }, func(v float64) { t.Dmg = int(maxF(v, 0)) }, 1},
				editField{"fireDelay", func() float64 { return t.FireDelay }, func(v float64) { t.FireDelay = maxF(v, 0) }, 0.1},
				editField{"turnRate", func() float64 { return t.TurnRate }, func(v float64) { t.TurnRate = maxF(v, 0) }, 0.1})
		}
		if h := e.Hazard; h != nil {
			f = append(f, editField{"dps", func() float64 { return h.DPS }, func(v float64) { h.DPS = maxF(v, 0) }, 1})
		}
		if tp := e.Teleport; tp != nil {
			f = append(f,
				editField{"destX", func() float64 { return tp.Dest.X }, func(v float64) { tp.Dest.X = v }, 0.5},
				editField{"destZ", func() float64 { return tp.Dest.Z }, func(v float64) { tp.Dest.Z = v }, 0.5},
				editField{"cooldown", func() float64 { return tp.Cooldown }, func(v float64) { tp.Cooldown = maxF(v, 0) }, 0.1})
		}
		if d := e.Destruct; d != nil {
			f = append(f, editField{"maxHp", func() float64 { return float64(d.MaxHP) }, func(v float64) { d.MaxHP = int(maxF(v, 1)) }, 5})
		}
		if rs := e.Respawn; rs != nil {
			f = append(f, editField{"respawn", func() float64 { return rs.Delay }, func(v float64) { rs.Delay = maxF(v, 0) }, 1})
		}
		if b := e.Bounce; b != nil {
			f = append(f, editField{"power", func() float64 { return b.Power }, func(v float64) { b.Power = maxF(v, 0.1) }, 0.5})
		}
		if fl := e.Flag; fl != nil {
			f = append(f, editField{"team(-1/0/1)", func() float64 { return float64(fl.Team) }, func(v float64) { fl.Team = clampI(int(v), -1, 1) }, 1})
		}
		if z := e.Zone; z != nil {
			f = append(f, editField{"capture", func() float64 { return z.Capture }, func(v float64) { z.Capture = maxF(v, 0.5) }, 0.5})
		}
		return f
	}
	return nil
}

func maxF(v, lo float64) float64 {
	if v < lo {
		return lo
	}
	return v
}
func clampI(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// runEditor is the in-door map editor (Phase C). Stages 1-2: free-fly / top-down
// views, a catalog palette, ghost preview, and placement (3D gun-sight raycast or
// top-down cursor). Backspace backs out of each mode (and exits to the menu from
// nav); Q quits the program.
func runEditor(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, s *userSettings, playerName, dropfile string) {
	m := gm.Map{Name: "UNTITLED", Size: 20, Spawns: []gm.V3{{X: -14, Z: -14}, {X: 14, Z: 14}}}
	buildArena(m)

	cam := Cam{pos: gm.V3{X: 0, Y: 14, Z: -24}, yaw: 0, pitch: 0.5}
	topdown := false
	mode := edNav
	palIdx := 0  // palette selection
	itemIdx := 0 // chosen catalog item for placing
	fileIdx := 0 // file-menu selection
	loadIdx := 0 // load-list selection
	var loadList []string
	cursor := gm.V3{} // top-down placement cursor
	grid := 1.0       // snap step (cycled with G; 0 = free)
	gridSteps := []float64{1, 2, 0.5, 0}
	gridIdx := 0
	const flySpeed, lookRate, cursorSpeed = 16.0, 1.7, 18.0
	var prev []byte
	var dragging bool // extended wall/platform: anchor set, awaiting the end point
	var anchor gm.V3
	prevFire, prevMenu, prevSel, prevDel, prevGrid := false, false, false, false, false
	status, statusT := "", 0.0
	setStatus := func(s string) { status, statusT = s, 3.0 }
	var selList []selRef
	selIdx, fieldIdx, adjustT := 0, 0, 0.0
	ruleIdx := 0 // selected field in the RULES panel

	rebuild := func() { buildArena(m); prev = nil }

	w.WriteString("\x1b[2J\x1b[H")
	frameBudget := time.Second / 30
	start := time.Now()
	last := start
	lastPresence := time.Time{}
	for {
		select {
		case <-ip.quitCh:
			return
		default:
		}
		now := time.Now()
		if now.Sub(lastPresence) >= 10*time.Second {
			updatePresence(dropfile, "map editor", "")
			lastPresence = now
		}
		dt := now.Sub(last).Seconds()
		if dt > 0.1 {
			dt = 0.1
		}
		last = now

		// Text entry (map name): drain typed runes while in edName.
		if mode == edName {
		drainRunes:
			for {
				select {
				case r := <-ip.runes:
					if len(m.Name) < 24 && (r == ' ' || r == '-' || r == '_' ||
						(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
						m.Name += string(r)
					}
				default:
					break drainRunes
				}
			}
		}

		// Discrete events: mode transitions + list navigation + view toggle.
	drain:
		for {
			select {
			case k := <-ip.events:
				switch k {
				case mkTab:
					if mode == edNav || mode == edPlace {
						topdown = !topdown
						prev = nil
					}
				case mkBack:
					switch mode {
					case edNav:
						return // exit editor
					case edName:
						if len(m.Name) > 0 { // backspace deletes a char while naming
							m.Name = m.Name[:len(m.Name)-1]
						}
					case edPlace:
						if dragging { // cancel an in-progress drag, stay in place mode
							dragging = false
						} else {
							mode, prev = edNav, nil
						}
					default:
						mode = edNav
						prev = nil
					}
				case mkEnter:
					switch mode {
					case edNav, edPlace:
						mode, palIdx, prev = edPalette, itemIdx, nil
					case edPalette:
						itemIdx, mode, prev = palIdx, edPlace, nil
					case edFile:
						switch fileMenu[fileIdx] {
						case "SAVE":
							ip.drainRunes() // ignore keystrokes typed before the field opened
							mode, prev = edName, nil
						case "LOAD":
							loadList = listUserMaps()
							loadIdx, mode, prev = 0, edLoad, nil
						case "PLAYTEST":
							if gm.FatalIssues(gm.ValidateMap(m)) {
								setStatus("fix errors before playtest")
								mode = edNav
							} else {
								// Temporarily add the working map, play it, then remove it.
								idx := len(gm.Maps)
								gm.Maps = append(gm.Maps, m)
								sess := newOfflineOnMap(idx, offlineBots, gm.EffectiveMode(m), 1, s.difficulty, s.aimAssist, playerName, gm.SelectColors[0], nil, gm.BodyTank)
								quit := playMatch(w, cols, rows, rows3d, rnd, ip, sess, dropfile, "playtest", m.Name, false, nil)
								sess.close()
								gm.Maps = gm.Maps[:idx]
								if quit {
									return // Q during playtest: leave the editor (program quits)
								}
								w.WriteString("\x1b[2J\x1b[H")
								rebuild()
								mode = edNav
							}
						case "PUBLISH":
							if gm.FatalIssues(gm.ValidateMap(m)) {
								setStatus("fix errors before publishing")
							} else {
								// One blocking network round-trip; show a notice first.
								fmt.Fprintf(w, "\x1b[2;1H\x1b[0;1;33m publishing %s ... \x1b[0m", clip(m.Name, cols-14))
								w.Flush()
								if msg, err := publishMap(m); err != nil {
									setStatus("publish failed: " + err.Error())
								} else {
									setStatus(msg)
								}
							}
							mode, prev = edNav, nil
						case "RULES":
							if m.Rules == nil { // start from "all default"
								m.Rules = &gm.MapRules{Mode: -1, TimeLimit: -1, Target: -1, Lives: -1, Bots: -1}
							}
							ruleIdx, mode, prev = 0, edRules, nil
						case "LOGIC": // director (map-level) event rules
							runBehaviorEditor(w, cols, rows, ip, nil, &m.Logic, "DIRECTOR LOGIC")
							w.WriteString("\x1b[2J\x1b[H")
							rebuild()
							mode = edNav
						case "ACTORS": // mobile-boss templates (spawn @name)
							runActorEditor(w, cols, rows, ip, &m)
							w.WriteString("\x1b[2J\x1b[H")
							rebuild()
							mode = edNav
						case "NEW":
							m = gm.Map{Name: "UNTITLED", Size: 20, Spawns: []gm.V3{{X: -14, Z: -14}, {X: 14, Z: 14}}}
							rebuild()
							setStatus("new map")
							mode = edNav
						case "BACK":
							mode, prev = edNav, nil
						}
					case edName:
						setStatus(saveEditorMap(m))
						mode, prev = edNav, nil
					case edLoad:
						if loadIdx < len(loadList) {
							if lm, err := loadUserMap(loadList[loadIdx]); err == nil {
								m = lm
								rebuild()
								setStatus("loaded " + loadList[loadIdx])
							} else {
								setStatus("load failed")
							}
						}
						mode, prev = edNav, nil
					case edSelect: // ENTER on a selected entity opens its behavior editor
						if len(selList) > 0 && selList[selIdx].kind == selEntity {
							runBehaviorEditor(w, cols, rows, ip, &m.Entities[selList[selIdx].idx], nil, "BEHAVIORS")
							w.WriteString("\x1b[2J\x1b[H")
							rebuild()
						}
					}
				case mkUp:
					switch mode {
					case edPalette:
						palIdx = (palIdx - 1 + len(catItems)) % len(catItems)
					case edFile:
						fileIdx = (fileIdx - 1 + len(fileMenu)) % len(fileMenu)
					case edLoad:
						if len(loadList) > 0 {
							loadIdx = (loadIdx - 1 + len(loadList)) % len(loadList)
						}
					case edSelect:
						if n := len(fieldsFor(&m, selList[selIdx])); n > 0 {
							fieldIdx = (fieldIdx - 1 + n) % n
						}
					case edRules:
						ruleIdx = (ruleIdx - 1 + len(ruleFields)) % len(ruleFields)
					}
				case mkDown:
					switch mode {
					case edPalette:
						palIdx = (palIdx + 1) % len(catItems)
					case edFile:
						fileIdx = (fileIdx + 1) % len(fileMenu)
					case edLoad:
						if len(loadList) > 0 {
							loadIdx = (loadIdx + 1) % len(loadList)
						}
					case edSelect:
						if n := len(fieldsFor(&m, selList[selIdx])); n > 0 {
							fieldIdx = (fieldIdx + 1) % n
						}
					case edRules:
						ruleIdx = (ruleIdx + 1) % len(ruleFields)
					}
				case mkLeft:
					if mode == edSelect && len(selList) > 0 {
						selIdx = (selIdx - 1 + len(selList)) % len(selList)
						fieldIdx = 0
					}
				case mkRight:
					if mode == edSelect && len(selList) > 0 {
						selIdx = (selIdx + 1) % len(selList)
						fieldIdx = 0
					}
				}
			default:
				break drain
			}
		}

		in := ip.snapshot()

		// M opens the file menu from nav; V enters select/edit (both edge-triggered).
		if menuNow := ip.held(aEdMenu); menuNow && !prevMenu && mode == edNav {
			mode, fileIdx, prev = edFile, 0, nil
		}
		prevMenu = ip.held(aEdMenu)
		if selNow := ip.held(aEdSelect); selNow && !prevSel && mode == edNav {
			selList = listSelectables(m)
			if len(selList) == 0 {
				setStatus("nothing placed to edit")
			} else {
				selIdx, fieldIdx, mode, prev = 0, 0, edSelect, nil
			}
		}
		prevSel = ip.held(aEdSelect)
		if gridNow := ip.held(aEdGrid); gridNow && !prevGrid {
			gridIdx = (gridIdx + 1) % len(gridSteps)
			grid = gridSteps[gridIdx]
			if grid == 0 {
				setStatus("grid snap OFF")
			} else {
				setStatus(fmt.Sprintf("grid snap %.1f", grid))
			}
		}
		prevGrid = ip.held(aEdGrid)

		// In select mode: ,/. adjust the current field (throttled while held); X deletes.
		if mode == edSelect && len(selList) > 0 {
			r := selList[selIdx]
			fields := fieldsFor(&m, r)
			if fieldIdx >= len(fields) {
				fieldIdx = 0
			}
			adjustT -= dt
			if (in.TurretL || in.TurretR) && adjustT <= 0 && len(fields) > 0 {
				adjustT = 0.07
				f := fields[fieldIdx]
				if in.TurretL {
					f.set(f.get() - f.step)
				} else {
					f.set(f.get() + f.step)
				}
				if r.kind == selObstacle || r.kind == selRamp {
					rebuild() // static geometry changed
				}
			}
			if delNow := ip.held(aEdDelete); delNow && !prevDel {
				deleteSel(&m, r)
				rebuild()
				selList = listSelectables(m)
				if len(selList) == 0 {
					mode = edNav
				} else if selIdx >= len(selList) {
					selIdx = len(selList) - 1
				}
				fieldIdx = 0
				setStatus("deleted")
			}
			prevDel = ip.held(aEdDelete)
		}

		// In rules mode: ,/. adjust the selected victory-condition field (throttled).
		if mode == edRules && m.Rules != nil {
			adjustT -= dt
			if (in.TurretL || in.TurretR) && adjustT <= 0 {
				adjustT = 0.12
				dir := 1
				if in.TurretL {
					dir = -1
				}
				adjustRule(m.Rules, ruleIdx, dir)
			}
		}

		if statusT > 0 {
			statusT -= dt
		}

		// Camera / cursor movement only while navigating or placing.
		if mode == edNav || mode == edPlace {
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
		ghost.Y = gm.GroundHeight(m, ghost.X, ghost.Z, 1e9) // stack on the surface under the cursor

		// Place on a fresh SPACE press (edge-triggered) while in place mode. Draggable
		// items (wall/platform) use two presses: anchor, then span.
		if mode == edPlace && in.Fire && !prevFire {
			it := catItems[itemIdx]
			switch {
			case it.dragMake != nil && !dragging:
				anchor, dragging = ghost, true
			case it.dragMake != nil && dragging:
				it.dragMake(&m, anchor, ghost)
				dragging = false
				rebuild()
			default:
				it.place(&m, ghost)
				rebuild()
			}
		}
		prevFire = in.Fire
		if mode != edPlace {
			dragging = false
		}

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
		entT := append([]gm.Entity(nil), m.Entities...)
		entS := editorEntSnaps(m)
		addMarker := func(p gm.V3, half gm.V3, col [3]float64) {
			entT = append(entT, gm.Entity{Kind: "wall", Pos: p, Half: half, Color: col})
			entS = append(entS, gm.EntitySnap{})
		}
		for _, s := range m.Spawns { // make spawns/pickups visible in the editor
			addMarker(gm.V3{X: s.X, Y: 0.5, Z: s.Z}, gm.V3{X: 0.5, Y: 0.5, Z: 0.5}, [3]float64{0.30, 0.85, 0.40})
		}
		for _, s := range m.Pickups { // colored by kind so typed spots read at a glance
			addMarker(gm.V3{X: s.Pos.X, Y: 0.4, Z: s.Pos.Z}, gm.V3{X: 0.4, Y: 0.4, Z: 0.4}, pickupColor(s.Kind))
		}
		if mode == edPlace { // ghost preview of the item to drop
			it := catItems[itemIdx]
			gcol := [3]float64{0.45, 0.95, 1.0}
			if dragging { // span preview from the anchor to the cursor
				cx, cz := (anchor.X+ghost.X)/2, (anchor.Z+ghost.Z)/2
				hx := math.Abs(ghost.X-anchor.X)/2 + 0.5
				hz := math.Abs(ghost.Z-anchor.Z)/2 + 0.5
				addMarker(gm.V3{X: cx, Y: math.Max(anchor.Y, ghost.Y) + it.half.Y, Z: cz}, gm.V3{X: hx, Y: it.half.Y, Z: hz}, gcol)
			} else {
				addMarker(gm.V3{X: ghost.X, Y: ghost.Y + it.half.Y, Z: ghost.Z}, it.half, gcol)
			}
		}
		if mode == edSelect && len(selList) > 0 { // highlight the selected object
			p := selPos(&m, selList[selIdx])
			addMarker(gm.V3{X: p.X, Y: p.Y + 0.05, Z: p.Z}, gm.V3{X: 1.3, Y: 1.3, Z: 1.3}, [3]float64{1.0, 1.0, 0.25})
		}
		reticle := [3]byte{120, 220, 140}
		rnd.renderWorld(rc, now.Sub(start).Seconds(), nil, nil, nil, nil,
			entT, entS, editorZoneSnaps(m), -1, 0, topdown, false, reticle, 0)
		frame, cur := encode(rnd, cols, rows3d, prev)
		prev = cur
		w.Write(frame)

		errs, warns := 0, 0
		for _, is := range gm.ValidateMap(m) {
			if is.Fatal {
				errs++
			} else {
				warns++
			}
		}
		drawEditorBar(w, cols, rows, m, topdown, mode, itemIdx, ghost, grid, errs, warns)
		switch mode {
		case edPalette:
			drawListOverlay(w, cols, rows, "CATALOG", catNames(), palIdx)
		case edFile:
			drawListOverlay(w, cols, rows, "FILE", fileMenu, fileIdx)
		case edLoad:
			if len(loadList) == 0 {
				drawListOverlay(w, cols, rows, "LOAD (none saved)", []string{"BKSP to cancel"}, 0)
			} else {
				drawListOverlay(w, cols, rows, "LOAD", loadList, loadIdx)
			}
		case edName:
			drawNameEntry(w, cols, rows, m.Name)
		case edSelect:
			if len(selList) > 0 {
				drawEditPanel(w, rows, selLabel(&m, selList[selIdx]), selIdx+1, len(selList), fieldsFor(&m, selList[selIdx]), fieldIdx)
			}
		case edRules:
			drawRulesPanel(w, rows, m, ruleIdx)
		}
		if statusT > 0 {
			fmt.Fprintf(w, "\x1b[2;1H\x1b[0;1;33m %s \x1b[0m", clip(status, cols-2))
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

func drawEditorBar(w *bufio.Writer, cols, rows int, m gm.Map, topdown bool, mode, itemIdx int, ghost gm.V3, grid float64, errs, warns int) {
	view := "3D"
	if topdown {
		view = "TOP"
	}
	gridS := "off"
	if grid > 0 {
		gridS = fmt.Sprintf("%.1f", grid)
	}
	valid := "ok"
	if errs > 0 || warns > 0 {
		valid = fmt.Sprintf("%dx %d!", errs, warns) // x = errors, ! = warnings
	}
	// Width capped at cols-1: never touch the bottom-right cell (autowrap scroll).
	width := cols - 1
	top := fmt.Sprintf(" EDIT %s  [%s]  obst:%d ent:%d spawn:%d  grid:%s  chk:%s ",
		m.Name, view, len(m.Obstacles), len(m.Entities), len(m.Spawns), gridS, valid)
	fmt.Fprintf(w, "\x1b[1;1H\x1b[1;30;46m%-*s\x1b[0m", width, clip(top, width))

	var help string
	switch mode {
	case edPalette:
		help = "up/dn pick   ENTER choose   BKSP cancel"
	case edPlace:
		drop := "SPACE drop"
		if catItems[itemIdx].dragMake != nil {
			drop = "SPACE anchor>span"
		}
		help = fmt.Sprintf("PLACING %s @ %.0f,%.0f   %s  R/F up/down  ENTER palette  TAB view  BKSP done",
			catItems[itemIdx].name, ghost.X, ghost.Z, drop)
	case edFile, edName, edLoad:
		help = "up/dn pick   ENTER choose   BKSP cancel"
	case edSelect:
		help = "</> object   up/dn field   ,/. adjust   X delete   BKSP done"
	case edRules:
		help = "up/dn field   ,/. adjust   BKSP done"
	default:
		help = "WASD fly  arrows look  R/F up/down  ENTER catalog  V edit  G grid  M file  TAB view  BKSP exit"
	}
	fmt.Fprintf(w, "\x1b[%d;1H\x1b[0;90m%s\x1b[0m", rows, clip(centered(help, width), width))
}

func catNames() []string {
	out := make([]string, len(catItems))
	for i, it := range catItems {
		out[i] = it.name
	}
	return out
}

// drawListOverlay centers a titled, selectable list (catalog / file / load).
func drawListOverlay(w *bufio.Writer, cols, rows int, title string, items []string, sel int) {
	row := rows/2 - len(items)/2 - 1
	if row < 2 {
		row = 2
	}
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;97m%s\x1b[0m", row, (cols-len(title))/2+1, title)
	row += 2
	for i, it := range items {
		style, marker := "\x1b[0;36m", "  "
		if i == sel {
			style, marker = "\x1b[1;30;46m", "> "
		}
		line := marker + it
		fmt.Fprintf(w, "\x1b[%d;%dH%s  %s  \x1b[0m", row+i, (cols-len(line)-4)/2+1, style, line)
	}
}

// drawEditPanel lists the selected object's editable fields down the left edge,
// highlighting the current field.
func drawEditPanel(w *bufio.Writer, rows int, label string, n, total int, fields []editField, sel int) {
	row := 3
	fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;97mEDIT %s (%d/%d)\x1b[0m", row, label, n, total)
	for i, f := range fields {
		row++
		if row >= rows {
			break
		}
		style := "\x1b[0;37m"
		if i == sel {
			style = "\x1b[1;30;46m"
		}
		if f.name == "weapon" { // show the palette name, not the index
			fmt.Fprintf(w, "\x1b[%d;2H%s %-12s %-8s \x1b[0m", row, style, f.name, weaponName(int(f.get())))
		} else if f.name == "kind" { // pickup kind by name
			fmt.Fprintf(w, "\x1b[%d;2H%s %-12s %-8s \x1b[0m", row, style, f.name, pickKindName(int(f.get())))
		} else {
			fmt.Fprintf(w, "\x1b[%d;2H%s %-12s %7.2f \x1b[0m", row, style, f.name, f.get())
		}
	}
}

// weaponName is the palette name for a weapon index (for the edit panel).
func weaponName(i int) string {
	if i >= 0 && i < len(gm.Weapons) {
		return gm.Weapons[i].Name
	}
	return "?"
}

// pickKindName labels a pickup kind (-1 = any) for the edit panel.
func pickKindName(k int) string {
	switch k {
	case -1:
		return "ANY"
	case gm.PickRepair:
		return "REPAIR"
	case gm.PickShield:
		return "SHIELD"
	case gm.PickRapid:
		return "RAPID"
	case gm.PickCloak:
		return "CLOAK"
	case gm.PickAmmo:
		return "AMMO"
	case gm.PickWeapon:
		return "WEAPON"
	}
	return "?"
}

// adjustRule steps one victory-condition field; -1 means "default" for each, with
// the sentinels stepping naturally (time: default->endless->15s..., target:
// default->1->2..., lives: default->infinite->1...).
func adjustRule(r *gm.MapRules, idx, dir int) {
	switch idx {
	case 0: // mode: -1 (auto) .. last ruleset
		r.Mode = clampI(r.Mode+dir, -1, len(gm.Rulesets)-1)
	case 1: // time seconds: -1 default, 0 endless, then 15s steps
		switch {
		case dir > 0 && r.TimeLimit < 0:
			r.TimeLimit = 0
		case dir > 0:
			r.TimeLimit += 15
		case dir < 0 && r.TimeLimit <= 0:
			r.TimeLimit = -1
		default:
			if r.TimeLimit -= 15; r.TimeLimit < 0 {
				r.TimeLimit = 0
			}
		}
	case 2: // target: -1 default, then >=1
		switch {
		case dir > 0 && r.Target < 1:
			r.Target = 1
		case dir > 0:
			r.Target++
		case dir < 0 && r.Target <= 1:
			r.Target = -1
		default:
			r.Target--
		}
	case 3: // lives: -1 default, 0 infinite, then >=1
		r.Lives = clampI(r.Lives+dir, -1, 99)
	case 4: // bots: -1 default, 0 none, then >=1 fill bots
		r.Bots = clampI(r.Bots+dir, -1, 16)
	}
}

// drawRulesPanel shows the map's victory-condition fields; "default" defers to the
// mode, and the header notes the mode the map will actually play (incl. auto).
func drawRulesPanel(w *bufio.Writer, rows int, m gm.Map, sel int) {
	r := m.Rules
	if r == nil {
		return
	}
	modeS := "AUTO (" + gm.EffectiveMode(m).String() + ")"
	if r.Mode >= 0 && r.Mode < len(gm.Rulesets) {
		modeS = gm.Mode(r.Mode).String()
	}
	timeS := "default"
	if r.TimeLimit == 0 {
		timeS = "endless"
	} else if r.TimeLimit > 0 {
		timeS = fmt.Sprintf("%.0fs", r.TimeLimit)
	}
	targetS := "default"
	if r.Target > 0 {
		targetS = fmt.Sprintf("%d", r.Target)
	}
	livesS := "default"
	if r.Lives == 0 {
		livesS = "infinite"
	} else if r.Lives > 0 {
		livesS = fmt.Sprintf("%d", r.Lives)
	}
	botsS := "default"
	if r.Bots == 0 {
		botsS = "none"
	} else if r.Bots > 0 {
		botsS = fmt.Sprintf("%d", r.Bots)
	}
	vals := []string{modeS, timeS, targetS, livesS, botsS}

	row := 3
	fmt.Fprintf(w, "\x1b[%d;2H\x1b[1;97mMAP RULES (victory)\x1b[0m", row)
	for i, name := range ruleFields {
		row++
		if row >= rows {
			break
		}
		style := "\x1b[0;37m"
		if i == sel {
			style = "\x1b[1;30;46m"
		}
		fmt.Fprintf(w, "\x1b[%d;2H%s %-8s %-18s \x1b[0m", row, style, name, vals[i])
	}
}

// drawNameEntry overlays the map-name text field.
func drawNameEntry(w *bufio.Writer, cols, rows int, name string) {
	row := rows / 2
	t := "NAME THIS MAP (type, ENTER to save, BKSP edits)"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;97m%s\x1b[0m", row-1, (cols-len(t))/2+1, t)
	field := "[ " + name + "_ ]"
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;30;46m %s \x1b[0m", row+1, (cols-len(field))/2, field)
}

// ---------------------------------------------------------------------------
// save / load (usermaps/)
// ---------------------------------------------------------------------------

// saveEditorMap validates and writes a map to usermaps/<slug>.json, returning a
// status message. Saved maps are loaded into the offline pool at the next launch.
func saveEditorMap(m gm.Map) string {
	if gm.FatalIssues(gm.ValidateMap(m)) {
		return "NOT saved: map has errors (see mapcheck)"
	}
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return "NOT saved: name required"
	}
	data, err := gm.MapJSON(m)
	if err != nil {
		return "save failed: " + err.Error()
	}
	dir := authorMapsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "save failed: " + err.Error()
	}
	fn := filepath.Join(dir, slug(name)+".json")
	if err := os.WriteFile(fn, data, 0o644); err != nil {
		return "save failed: " + err.Error()
	}
	return "saved " + filepath.Base(fn) + " (playable next launch)"
}

// listUserMaps returns the saved author-map filenames (base names) for the load list.
func listUserMaps() []string {
	files, _ := filepath.Glob(filepath.Join(authorMapsDir(), "*.json"))
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.Base(f))
	}
	return out
}

// loadUserMap reads and parses a saved author map by base filename.
func loadUserMap(base string) (gm.Map, error) {
	data, err := os.ReadFile(filepath.Join(authorMapsDir(), base))
	if err != nil {
		return gm.Map{}, err
	}
	return gm.ParseMapJSON(data)
}

// slug makes a filename-safe, lowercase fragment from a map name.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "untitled"
	}
	return b.String()
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
