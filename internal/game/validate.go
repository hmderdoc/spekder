package game

import (
	"fmt"
	"math"
)

// MapIssue is one problem found while validating an authored map. Entity is the
// index into Map.Entities, or -1 for a map-level issue. Fatal marks a map that
// won't work correctly; non-fatal issues are likely-mistakes worth surfacing.
// The map editor and the mapcheck CLI both consume these.
type MapIssue struct {
	Entity int
	Field  string
	Msg    string
	Fatal  bool
}

func (i MapIssue) String() string {
	where := "map"
	if i.Entity >= 0 {
		where = fmt.Sprintf("entity[%d]", i.Entity)
	}
	sev := "warn"
	if i.Fatal {
		sev = "ERROR"
	}
	if i.Field != "" {
		where += "." + i.Field
	}
	return fmt.Sprintf("%-5s %s: %s", sev, where, i.Msg)
}

// FatalIssues reports whether any issue in the list is fatal.
func FatalIssues(issues []MapIssue) bool {
	for _, i := range issues {
		if i.Fatal {
			return true
		}
	}
	return false
}

// ValidateMap checks an authored map against the schema rules and returns every
// problem found (empty slice = clean). Arenas are square: Size is the half-extent
// (0 = default). This is the single source of truth for "is this map valid",
// shared by the loader, the mapcheck CLI, and (later) the editor.
func ValidateMap(m Map) []MapIssue {
	var out []MapIssue
	mapIssue := func(field, msg string, fatal bool) {
		out = append(out, MapIssue{Entity: -1, Field: field, Msg: msg, Fatal: fatal})
	}
	entIssue := func(i int, field, msg string, fatal bool) {
		out = append(out, MapIssue{Entity: i, Field: field, Msg: msg, Fatal: fatal})
	}

	if m.Name == "" {
		mapIssue("name", "map has no name (used for selection and the wire)", true)
	}
	if m.Version != 0 && m.Version > SchemaVersion {
		mapIssue("version", fmt.Sprintf("schema version %d is newer than supported (%d)", m.Version, SchemaVersion), false)
	}
	if m.Size < 0 {
		mapIssue("size", "size (arena half-extent) must be >= 0 (0 = default)", true)
	}
	a := m.Size
	if a <= 0 {
		a = ArenaA
	}
	inBounds := func(p V3) bool { return math.Abs(p.X) <= a && math.Abs(p.Z) <= a }

	if len(m.Spawns) == 0 {
		mapIssue("spawns", "no spawn points; tanks fall back to random placement", false)
	}
	for i, s := range m.Spawns {
		if !inBounds(s) {
			mapIssue("spawns", fmt.Sprintf("spawn %d %v is outside the arena (half-extent %.1f)", i, []float64{s.X, s.Z}, a), false)
		}
	}
	for i, p := range m.Pickups {
		if !inBounds(p) {
			mapIssue("pickups", fmt.Sprintf("pickup spot %d %v is outside the arena", i, []float64{p.X, p.Z}), false)
		}
	}

	for i := range m.Entities {
		e := m.Entities[i]
		if e.Half.X <= 0 || e.Half.Y <= 0 || e.Half.Z <= 0 {
			entIssue(i, "half", "half-extent must be > 0 on every axis (zero = no footprint / invisible)", true)
		}
		if !inBounds(e.Pos) {
			entIssue(i, "pos", "entity is outside the arena", false)
		}
		if t := e.Turret; t != nil {
			if t.Range <= 0 {
				entIssue(i, "turret.range", "range must be > 0", true)
			}
			if t.Dmg < 0 {
				entIssue(i, "turret.dmg", "dmg must be >= 0 (0 = default)", true)
			}
			if t.FireDelay < 0 {
				entIssue(i, "turret.fireDelay", "fireDelay must be >= 0 (0 = default)", true)
			}
			if t.TurnRate < 0 {
				entIssue(i, "turret.turnRate", "turnRate must be >= 0 (0 = default)", true)
			}
		}
		if h := e.Hazard; h != nil && h.DPS <= 0 {
			entIssue(i, "hazard.dps", "dps must be > 0", true)
		}
		if tp := e.Teleport; tp != nil {
			if tp.Cooldown < 0 {
				entIssue(i, "teleport.cooldown", "cooldown must be >= 0", true)
			}
			if !inBounds(tp.Dest) {
				entIssue(i, "teleport.dest", "destination is outside the arena", false)
			}
		}
		if d := e.Destruct; d != nil && d.MaxHP <= 0 {
			entIssue(i, "destruct.maxHp", "maxHp must be > 0", true)
		}
		if r := e.Respawn; r != nil {
			if r.Delay < 0 {
				entIssue(i, "respawn.delay", "delay must be >= 0", true)
			}
			if e.Destruct == nil {
				entIssue(i, "respawn", "respawn has no effect without destruct (nothing destroys it)", false)
			}
		}
		if b := e.Bounce; b != nil && b.Power <= 0 {
			entIssue(i, "bounce.power", "power must be > 0", true)
		}
		if f := e.Flag; f != nil && (f.Team < -1 || f.Team > 1) {
			entIssue(i, "flag.team", "team must be -1 (neutral), 0, or 1", true)
		}
	}
	return out
}
