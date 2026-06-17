// Package game is the authoritative tank-combat simulation, shared by the arena
// server (which owns and broadcasts it) and the door (which runs it locally in
// offline mode and reconstructs it from network state online). It contains NO
// rendering or I/O — just world state and the tick update.
package game

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed maps/*.json maps/campaign/*.json
var mapFS embed.FS

// CampaignMaps is the numbered FLAG RUN campaign set, embedded separately from
// Maps so the levels never appear in rotation, the lobby, or the map picker -
// the campaign runner plays them in order by appending one at a time.
var CampaignMaps []Map

// Box is a solid, collidable obstacle. Prop is decorative scenery. Ramp is a
// drive-up sloped surface (rises toward Dir: 0=+X 1=-X 2=+Z 3=-Z).
type Box struct {
	Pos, Half V3
	Color     [3]float64
}
type Prop struct {
	Kind  string
	Pos   V3
	H     float64
	Color [3]float64
}
type Ramp struct {
	Pos   V3
	Half  V3
	H     float64
	Dir   int
	Color [3]float64
}

// Entity is an authored, placeable map object assembled from a fixed palette of
// behavior traits (turret, hazard, teleporter, destructible, respawn). It is the
// static template stored in a Map; per match the World instantiates a runtime
// copy (NewEntities) whose dynamic state (HP/dead/facing/cooldowns) rides
// MsgState to clients. A single object composes any mix of traits - e.g. a
// turret that is also Destruct+Respawn is a shootable gun emplacement that
// rebuilds itself. nil trait pointer = trait absent.
type Entity struct {
	Kind   string     // archetype / render selector: "turret","wall","hazard","teleporter"
	Pos    V3         // center
	Half   V3         // box half-extent (collision + default visual)
	Color  [3]float64 // base tint
	Yaw    float64    // facing: authored = initial; runtime = current (turret tracks)
	Pitch  float64    // gun elevation (turret): runtime, + = aim up
	Solid  bool       // collides like an obstacle while alive
	Weapon int        // turret weapon index (into Weapons); 0 = cannon

	Turret   *TurretTrait
	Hazard   *HazardTrait
	Teleport *TeleportTrait
	Destruct *DestructTrait
	Respawn  *RespawnTrait
	Bounce   *BounceTrait
	Flag     *FlagTrait
	Zone     *ZoneTrait
	Trigger  *TriggerTrait

	// --- event-driven behavior (authored; see EVENTS.md) ---
	Tag       string     // author name, referenced by actions/conditions as #tag
	Watch     []float64  // HP% thresholds that emit hp_below when crossed (needs Destruct)
	Behaviors []Behavior // rules this entity subscribes to / emits

	// --- runtime instance state (set in the World copy, not authored) ---
	HP       int     // current hit points (Destruct); 0/unused otherwise
	Dead     bool    // destroyed; awaiting respawn or gone for good
	cooldown float64 // turret fire / teleport debounce timer
	respawnT float64 // sec until respawn while Dead (Respawn trait)
	bDone    []bool  // per-behavior Once latch (parallel to Behaviors)
	wHit     []bool  // per-threshold latch (parallel to Watch)
	inside   []bool  // trigger: which tanks are currently inside the footprint
	mvPath   string  // move: path being followed ("" = not moving)
	mvSpeed  float64 // move: units/sec
	mvDist   float64 // move: distance travelled along the path
	mvOn     bool    // move: actively advancing along mvPath
}

// TriggerTrait makes an entity a sensor volume that emits `entered`/`exited` signals
// as tanks cross its footprint (Pos/Half). Inert: not solid, no damage; author
// behaviors decide what entering/leaving does.
type TriggerTrait struct{}

// TurretTrait makes an entity track and shoot the nearest live enemy tank in
// range, firing the same projectiles tanks use.
type TurretTrait struct {
	Range     float64 // engagement radius
	FireDelay float64 // sec between shots
	Dmg       int     // projectile damage (0 -> default projDmg)
	TurnRate  float64 // rad/sec the barrel tracks toward its target
}

// HazardTrait damages any tank standing within the entity's footprint (lava,
// spikes). DPS is applied continuously while inside.
type HazardTrait struct {
	DPS float64
}

// TeleportTrait warps a tank that drives into the footprint to Dest, then
// debounces for Cooldown sec so it doesn't immediately bounce back.
type TeleportTrait struct {
	Dest     V3
	Cooldown float64
}

// DestructTrait gives an entity hit points; projectiles whittle it down and it
// is destroyed at 0 (pair with RespawnTrait to have it return).
type DestructTrait struct {
	MaxHP int
}

// RespawnTrait makes a destroyed entity come back after Delay sec.
type RespawnTrait struct {
	Delay float64
}

// BounceTrait launches a tank that touches the entity's footprint straight up
// with a fixed velocity (a trampoline / jump pad). Power is the launch speed in
// units/sec; standing on it re-launches each time you come back down.
type BounceTrait struct {
	Power float64
}

// FlagTrait marks an entity as an objective flag spawn (Phase B): the ruleset
// instantiates a runtime flag here at match start. Team -1 is a neutral Flag Run
// pickup; 0/1 is a CTF team flag homed at this spot. The entity itself is an
// inert placement marker - it doesn't render or collide; the runtime flag does.
type FlagTrait struct {
	Team int
}

// ZoneTrait marks a King-of-the-Hill control zone spawn. Capture is the seconds
// of uncontested presence needed to flip control (0 -> default). Inert marker
// like flag; the runtime zone (w.zones) does the contest/hold scoring and render.
type ZoneTrait struct {
	Capture float64
}

// SchemaVersion is the current map-file format version. Authored maps may set
// "version"; 0 (absent) is treated as 1 for legacy files.
const SchemaVersion = 4 // v4 added event behaviors (vars/logic, entity tag/watch/behaviors)

// Map is a static arena layout. Size is the arena half-extent (0 = default);
// arenas are square. Pickups reserves power-up spawn spots. Entities are
// authored trait-objects (turrets, hazards, ...) instantiated each match.
type Map struct {
	Version   int
	Name      string
	Size      float64
	Obstacles []Box
	Ramps     []Ramp
	Scenery   []Prop
	Spawns    []V3
	Pickups   []MapPickup
	Entities  []Entity
	Rules     *MapRules      // optional per-map victory conditions (nil = implied by objectives)
	Vars      map[string]int // initial blackboard values (event system; see EVENTS.md)
	Logic     []Behavior     // map-level "director" rules (source = world, -1)
	Paths     []Path         // named waypoint paths (for the move action / escort)
	Actors    []Actor        // named tank templates with behaviors (mobile bosses)
}

// Path is a named ordered list of waypoints an entity can be moved along (the
// payload/escort building block; referenced by the move action).
type Path struct {
	Name   string
	Points []V3
}

// Actor is a named tank template the spawn action can instantiate: a mobile,
// behavior-carrying bot (e.g. a roaming boss). Spawn it with `spawn @<name>`.
type Actor struct {
	Name      string
	Vehicle   int
	Body      int
	MaxHP     int // 0 = chassis default
	Watch     []float64
	Behaviors []Behavior
}

// MapPickup is an authored power-up spot: where a drop appears, and (v3) what it
// is. Kind < 0 means "any" (the old random behavior). Weapon is the granted
// weapon when Kind == PickWeapon (0 = random from DropWeapons).
type MapPickup struct {
	Pos    V3
	Kind   int
	Weapon int
}

// MapRules lets a map override how it's played: its mode and the win numbers.
// Each field uses -1 to mean "use the mode's default", so a v1 map (no Rules)
// behaves exactly as before. Set by the editor's RULES panel.
type MapRules struct {
	Mode       int     // -1 = auto (NaturalMode); else a mode index
	TimeLimit  float64 // -1 = default; 0 = endless; >0 = match seconds
	Target     int     // -1 = default; else the win count (frags/captures/hold-points)
	Lives      int     // -1 = default; 0 = infinite; >0 = lives per tank
	Bots       int     // -1 = default/session count; >=0 = exact fill-bot count (scripted maps)
	MaxPlayers int     // <=0 = uncapped; else the most combatants (humans+bots) the map is sized for
}

// MapCapacity returns a map's combatant cap (0 = uncapped). Rotation, votes,
// and session creation use it to keep big sessions off small maps.
func MapCapacity(m Map) int {
	if m.Rules != nil && m.Rules.MaxPlayers > 0 {
		return m.Rules.MaxPlayers
	}
	return 0
}

// NewEntities returns a fresh runtime copy of the map's authored entities with
// instance state initialized (HP from Destruct, alive). Called at match start.
// Trait pointers are shared with the template - they are read-only params; only
// the value fields (HP/Dead/cooldown/respawnT/Yaw) are mutated at runtime.
func (m Map) NewEntities() []Entity {
	out := make([]Entity, len(m.Entities))
	for i, e := range m.Entities {
		out[i] = e // value copy
		out[i].Dead, out[i].cooldown, out[i].respawnT = false, 0, 0
		// Clone the trait pointers behaviors may tune (setstat), so runtime changes
		// stay match-local and don't mutate the shared template.
		if e.Turret != nil {
			t := *e.Turret
			out[i].Turret = &t
		}
		if e.Hazard != nil {
			h := *e.Hazard
			out[i].Hazard = &h
		}
		if e.Destruct != nil {
			d := *e.Destruct
			out[i].Destruct = &d
			out[i].HP = e.Destruct.MaxHP
		}
		out[i].bDone = make([]bool, len(e.Behaviors))
		out[i].wHit = make([]bool, len(e.Watch))
	}
	return out
}

// MapHalf is the play boundary (just inside the walls) for a map.
func MapHalf(m Map) float64 {
	a := m.Size
	if a <= 0 {
		a = ArenaA
	}
	return a - 0.7
}

// Maps is the indexed, embedded map set plus any usermaps loaded at startup.
// Indexes are LOCAL to each process: online, the server transmits the full
// active map over the wire (MsgMap, on join and map change) and lobby votes
// index the server's own transmitted list - so door and server builds do NOT
// need matching map sets. The embedded set drives offline play.
var Maps []Map

type jbox struct {
	Pos   [3]float64 `json:"pos"`
	Half  [3]float64 `json:"half"`
	Color [3]float64 `json:"color"`
}
type jprop struct {
	Kind  string     `json:"kind"`
	Pos   [3]float64 `json:"pos"`
	H     float64    `json:"h"`
	Color [3]float64 `json:"color"`
}
type jramp struct {
	Pos   [3]float64 `json:"pos"`
	Half  [3]float64 `json:"half"`
	H     float64    `json:"h"`
	Dir   string     `json:"dir"`
	Color [3]float64 `json:"color"`
}
type jturret struct {
	Range     float64 `json:"range"`
	FireDelay float64 `json:"fireDelay"`
	Dmg       int     `json:"dmg"`
	TurnRate  float64 `json:"turnRate"`
}
type jhazard struct {
	DPS float64 `json:"dps"`
}
type jteleport struct {
	Dest     [3]float64 `json:"dest"`
	Cooldown float64    `json:"cooldown"`
}
type jdestruct struct {
	MaxHP int `json:"maxHp"`
}
type jrespawn struct {
	Delay float64 `json:"delay"`
}
type jbounce struct {
	Power float64 `json:"power"`
}
type jflag struct {
	Team int `json:"team"`
}
type jzone struct {
	Capture float64 `json:"capture"`
}
type jtrigger struct{}
type jpath struct {
	Name   string       `json:"name"`
	Points [][2]float64 `json:"points"`
}
type jactor struct {
	Name      string     `json:"name"`
	Vehicle   int        `json:"vehicle,omitempty"`
	Body      int        `json:"body,omitempty"`
	MaxHP     int        `json:"maxHp,omitempty"`
	Watch     []float64  `json:"watch,omitempty"`
	Behaviors []Behavior `json:"behaviors,omitempty"`
}
type jentity struct {
	Kind      string     `json:"kind"`
	Pos       [3]float64 `json:"pos"`
	Half      [3]float64 `json:"half"`
	Color     [3]float64 `json:"color"`
	Yaw       float64    `json:"yaw"`
	Solid     bool       `json:"solid"`
	Weapon    int        `json:"weapon,omitempty"`
	Turret    *jturret   `json:"turret"`
	Hazard    *jhazard   `json:"hazard"`
	Teleport  *jteleport `json:"teleport"`
	Destruct  *jdestruct `json:"destruct"`
	Respawn   *jrespawn  `json:"respawn"`
	Bounce    *jbounce   `json:"bounce"`
	Flag      *jflag     `json:"flag"`
	Zone      *jzone     `json:"zone"`
	Trigger   *jtrigger  `json:"trigger,omitempty"`
	Tag       string     `json:"tag,omitempty"`
	Watch     []float64  `json:"watch,omitempty"`
	Behaviors []Behavior `json:"behaviors,omitempty"`
}
type jmap struct {
	Version   int            `json:"version"`
	Name      string         `json:"name"`
	Size      float64        `json:"size"`
	Obstacles []jbox         `json:"obstacles"`
	Ramps     []jramp        `json:"ramps"`
	Scenery   []jprop        `json:"scenery"`
	Spawns    [][2]float64   `json:"spawns"`
	Pickups   [][2]float64   `json:"pickups,omitempty"`     // v1/v2 legacy: untyped spots (read-only)
	PickSpots []jpickup      `json:"pickupSpots,omitempty"` // v3: typed pickup spots
	Entities  []jentity      `json:"entities"`
	Rules     *jrules        `json:"rules,omitempty"`
	Vars      map[string]int `json:"vars,omitempty"`
	Logic     []Behavior     `json:"logic,omitempty"`
	Paths     []jpath        `json:"paths,omitempty"`
	Actors    []jactor       `json:"actors,omitempty"`
}

// jpickup is a v3 typed pickup spot. Kind < 0 = any (random); Weapon is the
// granted weapon when Kind is the weapon-drop kind.
type jpickup struct {
	Pos    [2]float64 `json:"pos"`
	Kind   int        `json:"kind"`
	Weapon int        `json:"weapon,omitempty"`
}

type jrules struct {
	Mode       int     `json:"mode"`
	TimeLimit  float64 `json:"timeLimit"`
	Target     int     `json:"target"`
	Lives      int     `json:"lives"`
	Bots       *int    `json:"bots,omitempty"`       // pointer: absent (old maps) = default (-1)
	MaxPlayers int     `json:"maxPlayers,omitempty"` // 0/absent = uncapped
}

func (je jentity) toEntity() Entity {
	e := Entity{Kind: je.Kind, Pos: v3(je.Pos), Half: v3(je.Half), Color: je.Color, Yaw: je.Yaw, Solid: je.Solid, Weapon: je.Weapon}
	if je.Turret != nil {
		e.Turret = &TurretTrait{Range: je.Turret.Range, FireDelay: je.Turret.FireDelay, Dmg: je.Turret.Dmg, TurnRate: je.Turret.TurnRate}
	}
	if je.Hazard != nil {
		e.Hazard = &HazardTrait{DPS: je.Hazard.DPS}
	}
	if je.Teleport != nil {
		e.Teleport = &TeleportTrait{Dest: v3(je.Teleport.Dest), Cooldown: je.Teleport.Cooldown}
	}
	if je.Destruct != nil {
		e.Destruct = &DestructTrait{MaxHP: je.Destruct.MaxHP}
	}
	if je.Respawn != nil {
		e.Respawn = &RespawnTrait{Delay: je.Respawn.Delay}
	}
	if je.Bounce != nil {
		e.Bounce = &BounceTrait{Power: je.Bounce.Power}
	}
	if je.Flag != nil {
		e.Flag = &FlagTrait{Team: je.Flag.Team}
	}
	if je.Zone != nil {
		e.Zone = &ZoneTrait{Capture: je.Zone.Capture}
	}
	if je.Trigger != nil {
		e.Trigger = &TriggerTrait{}
	}
	e.Tag, e.Watch, e.Behaviors = je.Tag, je.Watch, je.Behaviors
	return e
}

func rampDir(s string) int {
	switch s {
	case "-x":
		return 1
	case "+z":
		return 2
	case "-z":
		return 3
	default: // "+x"
		return 0
	}
}

func init() {
	ents, err := mapFS.ReadDir("maps")
	if err == nil {
		var names []string
		for _, e := range ents {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names) // stable index order across builds
		for _, n := range names {
			data, err := mapFS.ReadFile("maps/" + n)
			if err != nil {
				continue
			}
			var jm jmap
			if json.Unmarshal(data, &jm) != nil {
				continue
			}
			Maps = append(Maps, jm.toMap())
		}
	}
	if len(Maps) == 0 {
		Maps = []Map{{Name: "OPEN", Spawns: []V3{{0, 0, -16}, {0, 0, 16}}}}
	}
	if ents, err := mapFS.ReadDir("maps/campaign"); err == nil {
		var names []string
		for _, e := range ents {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names) // level order = lexical file order
		for _, n := range names {
			data, err := mapFS.ReadFile("maps/campaign/" + n)
			if err != nil {
				continue
			}
			var jm jmap
			if json.Unmarshal(data, &jm) != nil {
				continue
			}
			CampaignMaps = append(CampaignMaps, jm.toMap())
		}
	}
}

// LoadMapDir appends author maps (*.json) from a directory to Maps (server-side;
// these get sent to clients over the wire, so the door needn't have them). Maps
// that fail to parse or have fatal validation issues are skipped with a reason
// on stderr (so the sysop sees why), and the count of loaded maps is returned.
func LoadMapDir(dir string) int {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	n := 0
	for _, fn := range files {
		data, err := os.ReadFile(fn)
		if err != nil {
			continue
		}
		var jm jmap
		if json.Unmarshal(data, &jm) != nil || jm.Name == "" {
			fmt.Fprintf(os.Stderr, "spekder: skipping %s: not a valid map JSON\n", fn)
			continue
		}
		m := jm.toMap()
		issues := ValidateMap(m)
		for _, is := range issues {
			fmt.Fprintf(os.Stderr, "spekder: %s: %s\n", fn, is)
		}
		if FatalIssues(issues) {
			fmt.Fprintf(os.Stderr, "spekder: skipping %s: fatal validation errors\n", fn)
			continue
		}
		Maps = append(Maps, m)
		n++
	}
	return n
}

// UpsertMap adds m to the pool (so it joins rotation/selection), replacing any
// existing map with the same name; returns its index. Used by the arena server to
// accept a published map live without a restart. Not concurrency-safe: the caller
// must hold whatever lock guards World/Maps access.
func UpsertMap(m Map) int {
	for i := range Maps {
		if Maps[i].Name == m.Name {
			Maps[i] = m
			return i
		}
	}
	Maps = append(Maps, m)
	return len(Maps) - 1
}

// ParseMapJSON decodes one map file's bytes into a Map. Exported so tools (the
// mapcheck CLI, the future editor) can load author maps outside the embed path.
func ParseMapJSON(data []byte) (Map, error) {
	var jm jmap
	if err := json.Unmarshal(data, &jm); err != nil {
		return Map{}, err
	}
	return jm.toMap(), nil
}

func v3(a [3]float64) V3   { return V3{a[0], a[1], a[2]} }
func v3xz(a [2]float64) V3 { return V3{a[0], 0, a[1]} }

func (jm jmap) toMap() Map {
	ver := jm.Version
	if ver == 0 {
		ver = 1 // legacy files predate the version field
	}
	m := Map{Version: ver, Name: jm.Name, Size: jm.Size}
	for _, b := range jm.Obstacles {
		m.Obstacles = append(m.Obstacles, Box{Pos: v3(b.Pos), Half: v3(b.Half), Color: b.Color})
	}
	for _, r := range jm.Ramps {
		m.Ramps = append(m.Ramps, Ramp{Pos: v3(r.Pos), Half: v3(r.Half), H: r.H, Dir: rampDir(r.Dir), Color: r.Color})
	}
	for _, p := range jm.Scenery {
		m.Scenery = append(m.Scenery, Prop{Kind: p.Kind, Pos: v3(p.Pos), H: p.H, Color: p.Color})
	}
	for _, s := range jm.Spawns {
		m.Spawns = append(m.Spawns, v3xz(s))
	}
	for _, s := range jm.Pickups { // legacy untyped spots -> "any" kind
		m.Pickups = append(m.Pickups, MapPickup{Pos: v3xz(s), Kind: -1})
	}
	for _, s := range jm.PickSpots { // v3 typed spots
		m.Pickups = append(m.Pickups, MapPickup{Pos: v3xz(s.Pos), Kind: s.Kind, Weapon: s.Weapon})
	}
	for _, e := range jm.Entities {
		m.Entities = append(m.Entities, e.toEntity())
	}
	if jm.Rules != nil {
		bots := -1
		if jm.Rules.Bots != nil {
			bots = *jm.Rules.Bots
		}
		m.Rules = &MapRules{Mode: jm.Rules.Mode, TimeLimit: jm.Rules.TimeLimit, Target: jm.Rules.Target, Lives: jm.Rules.Lives, Bots: bots, MaxPlayers: jm.Rules.MaxPlayers}
	}
	m.Vars, m.Logic = jm.Vars, jm.Logic
	for _, p := range jm.Paths {
		pts := make([]V3, len(p.Points))
		for i, wp := range p.Points {
			pts[i] = v3xz(wp)
		}
		m.Paths = append(m.Paths, Path{Name: p.Name, Points: pts})
	}
	for _, a := range jm.Actors {
		m.Actors = append(m.Actors, Actor{Name: a.Name, Vehicle: a.Vehicle, Body: a.Body, MaxHP: a.MaxHP, Watch: a.Watch, Behaviors: a.Behaviors})
	}
	return m
}

func rampDirName(d int) string {
	switch d {
	case 1:
		return "-x"
	case 2:
		return "+z"
	case 3:
		return "-z"
	default:
		return "+x"
	}
}

func j3(v V3) [3]float64 { return [3]float64{v.X, v.Y, v.Z} }
func j2(v V3) [2]float64 { return [2]float64{v.X, v.Z} }

func (e Entity) toJEntity() jentity {
	je := jentity{Kind: e.Kind, Pos: j3(e.Pos), Half: j3(e.Half), Color: e.Color, Yaw: e.Yaw, Solid: e.Solid, Weapon: e.Weapon}
	if e.Turret != nil {
		je.Turret = &jturret{Range: e.Turret.Range, FireDelay: e.Turret.FireDelay, Dmg: e.Turret.Dmg, TurnRate: e.Turret.TurnRate}
	}
	if e.Hazard != nil {
		je.Hazard = &jhazard{DPS: e.Hazard.DPS}
	}
	if e.Teleport != nil {
		je.Teleport = &jteleport{Dest: j3(e.Teleport.Dest), Cooldown: e.Teleport.Cooldown}
	}
	if e.Destruct != nil {
		je.Destruct = &jdestruct{MaxHP: e.Destruct.MaxHP}
	}
	if e.Respawn != nil {
		je.Respawn = &jrespawn{Delay: e.Respawn.Delay}
	}
	if e.Bounce != nil {
		je.Bounce = &jbounce{Power: e.Bounce.Power}
	}
	if e.Flag != nil {
		je.Flag = &jflag{Team: e.Flag.Team}
	}
	if e.Zone != nil {
		je.Zone = &jzone{Capture: e.Zone.Capture}
	}
	if e.Trigger != nil {
		je.Trigger = &jtrigger{}
	}
	je.Tag, je.Watch, je.Behaviors = e.Tag, e.Watch, e.Behaviors
	return je
}

func (m Map) toJmap() jmap {
	ver := m.Version
	if ver == 0 {
		ver = SchemaVersion
	}
	jm := jmap{Version: ver, Name: m.Name, Size: m.Size}
	for _, b := range m.Obstacles {
		jm.Obstacles = append(jm.Obstacles, jbox{Pos: j3(b.Pos), Half: j3(b.Half), Color: b.Color})
	}
	for _, r := range m.Ramps {
		jm.Ramps = append(jm.Ramps, jramp{Pos: j3(r.Pos), Half: j3(r.Half), H: r.H, Dir: rampDirName(r.Dir), Color: r.Color})
	}
	for _, p := range m.Scenery {
		jm.Scenery = append(jm.Scenery, jprop{Kind: p.Kind, Pos: j3(p.Pos), H: p.H, Color: p.Color})
	}
	for _, s := range m.Spawns {
		jm.Spawns = append(jm.Spawns, j2(s))
	}
	for _, s := range m.Pickups { // always write the v3 typed form
		jm.PickSpots = append(jm.PickSpots, jpickup{Pos: j2(s.Pos), Kind: s.Kind, Weapon: s.Weapon})
	}
	for _, e := range m.Entities {
		jm.Entities = append(jm.Entities, e.toJEntity())
	}
	if m.Rules != nil {
		jm.Rules = &jrules{Mode: m.Rules.Mode, TimeLimit: m.Rules.TimeLimit, Target: m.Rules.Target, Lives: m.Rules.Lives, MaxPlayers: m.Rules.MaxPlayers}
		if m.Rules.Bots >= 0 {
			b := m.Rules.Bots
			jm.Rules.Bots = &b
		}
	}
	jm.Vars, jm.Logic = m.Vars, m.Logic
	for _, p := range m.Paths {
		wps := make([][2]float64, len(p.Points))
		for i, pt := range p.Points {
			wps[i] = j2(pt)
		}
		jm.Paths = append(jm.Paths, jpath{Name: p.Name, Points: wps})
	}
	for _, a := range m.Actors {
		jm.Actors = append(jm.Actors, jactor{Name: a.Name, Vehicle: a.Vehicle, Body: a.Body, MaxHP: a.MaxHP, Watch: a.Watch, Behaviors: a.Behaviors})
	}
	return jm
}

// MapJSON serializes a Map to indented JSON (the inverse of ParseMapJSON), for
// the editor's save and any map-export tooling.
func MapJSON(m Map) ([]byte, error) {
	return json.MarshalIndent(m.toJmap(), "", "  ")
}

// --- tuning knobs (exported ones are also needed by the renderer) ---
const (
	tankSpeed        = 6.0
	hullTurnRate     = 1.9
	turretRate       = 2.6   // bot turret tracking speed
	playerTurretRate = 1.3   // player aim speed (slower = finer aim, less overshoot)
	pitchRate        = 1.1   // player gun-elevation speed (rad/sec)
	pitchMax         = 0.70  // max elevation (aim up), ~40 deg
	pitchMin         = -0.50 // max depression (aim down), ~-29 deg

	// Aim assist (lock-on-sweep): while the player is turning/elevating the turret,
	// if the aim passes within these capture radii of a valid target on both axes,
	// it LOCKS onto the target (snap + hold) so you can fire - catching small/fast-
	// passing targets that discrete key steps overshoot. Holding still keeps the
	// lock; turning for assistLockBreak sec releases it; Recenter / target-loss clear it.
	assistCaptureYaw   = 0.22 // rad (~12.5 deg) lock-on radius
	assistCapturePitch = 0.22
	assistLockBreak    = 0.40 // sec of sustained turn to break a lock
	assistBreakCool    = 0.50 // sec after a break before assist re-acquires
	fireDelay          = 0.55
	jumpSpeed          = 8.5  // upward launch velocity (units/sec)
	gravity            = 24.0 // downward acceleration (units/sec^2)
	climbSpeed         = 9.0  // insect wall-scaling rise rate (units/sec) while gripping a face
	projSpeed          = 24.0
	projLife           = 2.4
	projDmg            = 34
	tankMaxHP          = 100
	hitRadius          = 1.15
	tankBodyTop        = 1.9 // top of a tank's hittable body above its feet (×vehicle scale)

	ArenaA = 22.0 // default playfield half-extent (maps may override via Size)

	respawnDelay       = 3.0
	playerRespawnDelay = 5.0 // humans wait a touch longer so the kill-cam replay can run
	spawnGuardTime     = 1.6
	EyeHeight          = 1.35

	botFireRange = 26.0
	botAimTol    = 0.12
	botKeepDist  = 7.0
	botFireDelay = 1.2

	pickupSeekRange = 12.0 // how far a SeekPickups bot will divert for a power-up

	turretAimHeight = 0.9 // aim point above a target tank's feet (body center)
)

// Mode indexes the Ruleset table; a "mode" IS the data at Rulesets[Mode]. The
// wire syncs the index, so a new mode is just a new table entry. Phase is the
// match lifecycle state.
type Mode int

const (
	ModeDeathmatch Mode = iota
	ModeFlagRun
	ModeCTF
	ModeSurvival
	ModeElimination
	ModeTeamKotH
	ModeFFAKotH
)

// WinKind / BotSpawn / ObjKind are fixed palettes a Ruleset composes (palette,
// not scripting). See PHASE_B.md.
type WinKind int

const (
	WinFrags       WinKind = iota // a tank reaches Count kills
	WinCaptures                   // a team reaches Count captures
	WinCollectAll                 // every neutral flag has been collected
	WinElimination                // one side eliminated (co-op: all humans out of lives)
	WinScore                      // a team/tank reaches Count hold-points (King of the Hill)
)

type BotSpawn int

const (
	BotFill  BotSpawn = iota // keep a fixed bot pool topped up
	BotWaves                 // survival: spawn escalating waves
)

type ObjKind int

const (
	ObjNone         ObjKind = iota
	ObjNeutralFlags         // scattered neutral flags (Flag Run)
	ObjTeamFlags            // one flag per team at its base (CTF)
	ObjZone                 // control zone(s) to hold (King of the Hill)
)

// WinCond is one early-end trigger; Count is its threshold (kills/captures).
type WinCond struct {
	Kind  WinKind
	Count int
}

// Ruleset is a game mode expressed as data (see PHASE_B.md). Timeout is implicit
// when TimeLimit>0: the clock expiring ends the match and the winner is resolved
// by scoring.
type Ruleset struct {
	Name      string
	Desc      string  // one-line blurb for the menu
	Teams     int     // 0 = free-for-all, 2 = two teams
	TimeLimit float64 // match seconds; 0 = endless
	Lives     int     // per-tank lives (0 = infinite respawn); wave bots are exempt
	Bots      BotSpawn
	Objective ObjKind
	Win       []WinCond
	CoOp      bool // no per-tank winner (result = progress/wave reached)
}

// Rulesets is the mode table, indexed by Mode. Server and door must share the
// build (the wire syncs the index - same contract as Maps). Append to add a mode.
var Rulesets = []Ruleset{
	ModeDeathmatch: {Name: "DEATHMATCH", Desc: "Solo vs bots: frag the most tanks before time runs out.",
		Teams: 0, TimeLimit: matchTime, Bots: BotFill, Objective: ObjNone,
		Win: []WinCond{{WinFrags, DMFragLimit}}},
	ModeFlagRun: {Name: "FLAG RUN", Desc: "Solo vs bots: grab every flag before the clock runs out.",
		Teams: 0, TimeLimit: matchTime, Bots: BotFill, Objective: ObjNeutralFlags,
		Win: []WinCond{{WinCollectAll, 0}}},
	ModeCTF: {Name: "CAPTURE THE FLAG", Desc: "Team up vs bots: steal their flag, defend yours.",
		Teams: 2, TimeLimit: matchTime, Bots: BotFill, Objective: ObjTeamFlags,
		Win: []WinCond{{WinCaptures, ctfCaptureLimit}}},
	ModeSurvival: {Name: "SURVIVAL", Desc: "Solo vs bots: endless waves of hunters.",
		Teams: 0, TimeLimit: 0, Lives: survivalLives, Bots: BotWaves, Objective: ObjNone,
		Win: []WinCond{{WinElimination, 0}}, CoOp: true},
	ModeElimination: {Name: "ELIMINATION", Desc: "Solo vs bots: 3 lives each, last tank standing wins.",
		Teams: 0, TimeLimit: matchTime, Lives: 3, Bots: BotFill, Objective: ObjNone,
		Win: []WinCond{{WinElimination, 0}}},
	ModeTeamKotH: {Name: "TEAM KOTH", Desc: "Two teams fight to hold the hill; first to the score wins.",
		Teams: 2, TimeLimit: matchTime, Bots: BotFill, Objective: ObjZone,
		Win: []WinCond{{WinScore, kothScoreLimit}}},
	ModeFFAKotH: {Name: "KING OF THE HILL", Desc: "Solo vs bots: hold the hill to score; first to the score wins.",
		Teams: 0, TimeLimit: matchTime, Bots: BotFill, Objective: ObjZone,
		Win: []WinCond{{WinScore, kothScoreLimit}}},
}

func (m Mode) String() string {
	if int(m) < 0 || int(m) >= len(Rulesets) {
		return "DEATHMATCH"
	}
	return Rulesets[m].Name
}

// rules returns the active mode's Ruleset (clamped defensively).
// RulesetFor returns a mode's Ruleset (clamped defensively). Exported so the
// door's HUD/menu can render purely from mode data.
func RulesetFor(m Mode) Ruleset {
	if int(m) < 0 || int(m) >= len(Rulesets) {
		return Rulesets[ModeDeathmatch]
	}
	return Rulesets[m]
}

// rules returns the effective ruleset: the base for the current mode, with the
// active map's MapRules overrides applied (time limit, win target, lives). Every
// win/time/lives check flows through here, so the overrides take effect globally.
func (w *World) rules() Ruleset {
	base := RulesetFor(w.Mode)
	mr := w.ActiveMap().Rules
	if mr == nil {
		return base
	}
	if mr.TimeLimit >= 0 {
		base.TimeLimit = mr.TimeLimit
	}
	if mr.Lives >= 0 {
		base.Lives = mr.Lives
	}
	if mr.Target > 0 && len(base.Win) > 0 {
		win := append([]WinCond(nil), base.Win...) // copy: never mutate the shared Rulesets table
		for i := range win {
			win[i].Count = mr.Target
		}
		base.Win = win
	}
	return base
}

type Phase int

const (
	PhaseCountdown Phase = iota
	PhaseActive
	PhaseEnded
	PhaseLobby // between matches (server only): vote for the next mode
)

const (
	countdownTime = 4.0   // sec of "get ready" before a match
	matchTime     = 180.0 // sec per match
	DMFragLimit   = 20    // deathmatch ends at this many kills
	endTime       = 7.0   // sec the scoreboard lingers before the next match
	lobbyTime     = 30.0  // sec of mode-vote lobby between matches (server only)
	lobbyFastFwd  = 1.5   // sec the lobby holds once every player has locked (ENTER)
	flagCount     = 8     // flags scattered in Flag Run
	flagPickupRad = 1.9   // how close you must drive to grab a flag
	tankHitFlash  = 0.15  // sec a tank flashes white after taking a hit
	stepUp        = 0.6   // max ledge/step a tank can mount without jumping
	survivalLives = 3     // Survival: lives per human
	survivalPool  = 12    // Survival: bot pool size for waves
	// Survival per-wave enemy HP multiplier (replaces the old chassis-tier ramp).
	// FIRST-PASS values - tune against play/balancesim.
	survivalHPPerWave = 0.12 // +12% enemy HP per wave past the first
	survivalHPMax     = 2.5  // cap the multiplier so late waves don't become sponges

	ctfCaptureLimit = 3    // CTF: captures to win a match
	ctfCaptureRad   = 2.6  // CTF: how close to your base to score a capture
	flagReturnTime  = 15.0 // CTF: sec a dropped flag waits before returning home
	ctfCaptureBonus = 5    // CTF: personal frags awarded for a capture

	zoneCaptureTime = 4.0 // KotH: default sec of uncontested presence to flip a zone
	kothScoreLimit  = 60  // KotH: hold-points (~seconds held) to win
	zoneFallbackR   = 4.0 // KotH: half-extent of the default center hill when none authored

	teleDebounce   = 0.8 // sec a tank is immune to teleporters after warping
	pickupRadius   = 1.9 // how close you drive to grab a power-up
	pickupInterval = 9.0 // sec between drop spawns
	pickupMax      = 4   // max drops on the map at once
	buffShieldTime = 6.0 // sec of invulnerability from a SHIELD
	buffRapidTime  = 8.0 // sec of rapid fire from a RAPID
	buffCloakTime  = 7.0 // sec of cloak from a CLOAK
	rapidFireMul   = 0.4 // FireDelay multiplier while rapid-fire is active
)

// Vehicle is a selectable tank class with its own handling/armor tradeoffs
// (Spectre-style). Scale sizes the rendered model.
type Vehicle struct {
	Name      string
	MaxHP     int
	Speed     float64
	HullTurn  float64
	AimTurn   float64 // player turret aim speed
	FireDelay float64
	Jump      float64
	Scale     float64
	AmmoMax   float64 // regenerating ammo capacity (burst budget)
	AmmoRegen float64 // ammo regained per second (soft rate limit)
	Desc      string  // one-line class summary for the selection screen
}

// Vehicles is the selectable class list (index = wire id).
var Vehicles = []Vehicle{
	{Name: "SCOUT", MaxHP: 70, Speed: 8.2, HullTurn: 2.4, AimTurn: 1.7, FireDelay: 0.42, Jump: 10.0, Scale: 0.82, AmmoMax: 6, AmmoRegen: 2.4,
		Desc: "Fast, fragile recon. Outruns trouble on light armor; pick your fights."},
	{Name: "TANK", MaxHP: 100, Speed: 6.0, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1.0, AmmoMax: 8, AmmoRegen: 1.8,
		Desc: "The all-rounder. Balanced armor, speed, and fire rate. No bad matchups."},
	{Name: "HEAVY", MaxHP: 150, Speed: 4.3, HullTurn: 1.3, AimTurn: 1.0, FireDelay: 0.85, Jump: 6.5, Scale: 1.22, AmmoMax: 12, AmmoRegen: 1.2,
		Desc: "Rolling fortress. Heavy armor and a deep magazine, but slow and ponderous."},
	{Name: "RANGER", MaxHP: 85, Speed: 7.0, HullTurn: 2.1, AimTurn: 1.5, FireDelay: 0.48, Jump: 9.0, Scale: 0.9, AmmoMax: 7, AmmoRegen: 2.0,
		Desc: "Quick skirmisher. Nearly scout-fast with a steadier gun and more armor."},
	{Name: "ARTILLERY", MaxHP: 60, Speed: 3.8, HullTurn: 1.1, AimTurn: 1.4, FireDelay: 0.5, Jump: 5.0, Scale: 1.12, AmmoMax: 14, AmmoRegen: 2.2,
		Desc: "Glass siege. Frail and slow, but a huge, fast-recharging magazine."},
}

func veh(i int) Vehicle {
	if i < 0 || i >= len(Vehicles) {
		i = 1 // TANK default
	}
	return Vehicles[i]
}

// veh returns a tank's effective stats: its custom build if set, else its own
// per-character row (bodyVeh, keyed by body - the chassis indirection is gone).
// A survival wave HP multiplier (hpScale) rides on top so escalating waves field
// tougher enemies without borrowing a heavier chassis.
func (t *Tank) veh() Vehicle {
	if t.custom != nil {
		return *t.custom
	}
	v := VehBody(t.body)
	if t.hpScale > 0 && t.hpScale != 1 {
		v.MaxHP = int(float64(v.MaxHP) * t.hpScale)
	}
	return v
}

// Difficulty indexes the BotProfile table. See DIFFICULTY.md.
type Difficulty int

const (
	DiffEasy Difficulty = iota
	DiffBeginner
	DiffNormal
	DiffHard
	DiffUltimate
)

// BotProfile is the tunable bot-AI parameter set for a difficulty tier. The tier
// sets the center; each bot rolls per-bot jitter around it at spawn (rollBotAI)
// so a tier is a band of varied opponents, not a uniform wall.
type BotProfile struct {
	Name         string
	Sight        float64 // target acquire/track range (0 = unlimited)
	ReactDelay   float64 // sec to react to a newly acquired target (can't aim/fire yet)
	TrackRate    float64 // turret tracking speed (rad/s)
	Wobble       float64 // per-shot random aim error (radians)
	FireDelayMul float64 // bot reload multiplier (>1 = slower)
	SeekPickups  bool    // diverts to grab nearby power-ups
}

// BotProfiles is the difficulty ladder, indexed by Difficulty. HARD is roughly
// today's lethality; NORMAL (the new-player default) is gentler; ULTIMATE is the
// one step above today. Tune in playtest - these are centers, not gospel.
var BotProfiles = []BotProfile{
	DiffEasy:     {Name: "EASY", Sight: 14, ReactDelay: 0.9, TrackRate: 1.2, Wobble: 0.20, FireDelayMul: 1.9},
	DiffBeginner: {Name: "BEGINNER", Sight: 19, ReactDelay: 0.6, TrackRate: 1.7, Wobble: 0.12, FireDelayMul: 1.5},
	DiffNormal:   {Name: "NORMAL", Sight: 26, ReactDelay: 0.35, TrackRate: 2.1, Wobble: 0.07, FireDelayMul: 1.2},
	DiffHard:     {Name: "HARD", Sight: 0, ReactDelay: 0.10, TrackRate: 2.6, Wobble: 0.02, FireDelayMul: 1.0},
	DiffUltimate: {Name: "ULTIMATE", Sight: 0, ReactDelay: 0.0, TrackRate: 3.3, Wobble: 0.0, FireDelayMul: 0.8, SeekPickups: true},
}

// ProfileFor returns a difficulty's BotProfile (clamped; default NORMAL).
func ProfileFor(d Difficulty) BotProfile {
	if int(d) < 0 || int(d) >= len(BotProfiles) {
		return BotProfiles[DiffNormal]
	}
	return BotProfiles[d]
}

func (d Difficulty) String() string { return ProfileFor(d).Name }

// ParseDifficulty resolves a tier name (case-insensitive) to a Difficulty; ok is
// false for an unknown name (caller should fall back to the default).
func ParseDifficulty(s string) (Difficulty, bool) {
	for i := range BotProfiles {
		if strings.EqualFold(BotProfiles[i].Name, s) {
			return Difficulty(i), true
		}
	}
	return DiffNormal, false
}

// Difficulties returns the tiers in ladder order (for the door's picker).
func Difficulties() []Difficulty {
	out := make([]Difficulty, len(BotProfiles))
	for i := range BotProfiles {
		out[i] = Difficulty(i)
	}
	return out
}

// BotPalette / PlayerPalette give tanks distinct colors by slot.
var BotPalette = [][3]float64{
	{0.78, 0.26, 0.26}, {0.72, 0.60, 0.22}, {0.30, 0.70, 0.35},
	{0.65, 0.35, 0.75}, {0.80, 0.45, 0.20}, {0.40, 0.55, 0.80},
}

// SelectColors is the player-pickable color palette (vehicle-select swatches and
// the free-color fallback when a pick is taken).
var SelectColors = [][3]float64{
	{0.30, 0.65, 0.90}, // blue
	{0.90, 0.80, 0.30}, // yellow
	{0.85, 0.40, 0.60}, // pink
	{0.35, 0.80, 0.45}, // green
	{0.92, 0.48, 0.22}, // orange
	{0.58, 0.45, 0.92}, // purple
	{0.30, 0.82, 0.82}, // teal
	{0.90, 0.30, 0.34}, // red
	{0.72, 0.72, 0.78}, // silver
	{0.60, 0.78, 0.30}, // lime
}

// colorClose reports whether two colors are visually near-identical.
func colorClose(a, b [3]float64) bool {
	d := (a[0]-b[0])*(a[0]-b[0]) + (a[1]-b[1])*(a[1]-b[1]) + (a[2]-b[2])*(a[2]-b[2])
	return d < 0.02
}

// freeColor resolves a requested color to one not already worn by another human
// player: the request if free, else the first free palette swatch, else the
// request. Bots don't reserve colors (only "another player" matters).
func (w *World) freeColor(want [3]float64) [3]float64 {
	taken := func(c [3]float64) bool {
		for i := range w.Tanks {
			t := &w.Tanks[i]
			if !t.Bot && !t.gone && colorClose(t.Color, c) {
				return true
			}
		}
		return false
	}
	if want != ([3]float64{}) && !taken(want) {
		return want
	}
	for _, c := range SelectColors {
		if !taken(c) {
			return c
		}
	}
	if want != ([3]float64{}) {
		return want
	}
	return SelectColors[0]
}

var PlayerPalette = [][3]float64{
	{0.30, 0.65, 0.90}, {0.90, 0.80, 0.30}, {0.85, 0.40, 0.60},
	{0.45, 0.85, 0.55}, {0.90, 0.55, 0.35}, {0.60, 0.60, 0.90},
}

type V3 struct{ X, Y, Z float64 }

func (a V3) Sub(b V3) V3 { return V3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func (a V3) Add(b V3) V3 { return V3{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }
func (a V3) Cross(b V3) V3 {
	return V3{a.Y*b.Z - a.Z*b.Y, a.Z*b.X - a.X*b.Z, a.X*b.Y - a.Y*b.X}
}
func (a V3) Dot(b V3) float64 { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func (a V3) Norm() V3 {
	l := math.Sqrt(a.Dot(a))
	if l < 1e-9 {
		return a
	}
	return V3{a.X / l, a.Y / l, a.Z / l}
}

// Input is one tank's button state for a tick (held flags). Vote is the lobby
// mode vote (mode index, or -1 for none); only read during PhaseLobby.
type Input struct {
	Throttle, Reverse, HullL, HullR, TurretL, TurretR, Fire, Jump bool
	StrafeL, StrafeR                                              bool // sidestep left/right without turning
	AimUp, AimDown                                                bool // elevate / depress the gun
	Recenter                                                      bool // snap turret to hull-forward + level
	Fire2                                                         bool // fire the secondary weapon (B)
	Drop                                                          bool // CTF: drop the carried flag where you stand
	Vote                                                          int
	Ready                                                         bool // lobby: this player has locked their vote (ENTER)
}

type Tank struct {
	Pos         V3
	HullYaw     float64
	TurretYaw   float64 // relative to hull
	TurretPitch float64 // gun elevation: + = aim up, - = aim down (radians)
	HP          int
	Color       [3]float64
	Name        string   // display name: human handle, or a bot callsign
	custom      *Vehicle // per-tank stat override (authored actors/bosses); nil = use the body's row
	body        int      // render body style (BodyTank/BodySpider/...); 0 = tank
	Bot         bool
	Dead        bool
	Kills       int
	Deaths      int

	cooldown   float64
	cooldown2  float64 // secondary-weapon recharge
	weapon2    int     // secondary weapon index (into Weapons); fired with the B key
	wp2Used    int     // charge-stock secondaries: charges consumed (0 = full); regens via wp2RegenT
	wp2RegenT  float64 // time until the next consumed charge regenerates
	pounceT    float64 // tiger POUNCE active window: a kill during it refunds the dash
	ammo       float64 // regenerating ammo pool (soft fire limit); max/regen per vehicle
	slowT      float64 // EffSlow remaining (sec)
	slowMag    float64 // EffSlow magnitude (fraction of speed removed)
	boostT     float64 // EffSpeed remaining (sec)
	boostMag   float64 // EffSpeed magnitude (fraction of speed added)
	slipT      float64 // EffSlip remaining (sec): no steering, helpless slide
	dmgDownT   float64 // EffDamageDown remaining (sec): outgoing damage cut
	dmgDownMag float64 // fraction of outgoing damage removed while dmgDownT>0
	shellT     float64 // turtle shell mode remaining (sec): immobile + invulnerable

	// Minotaur barrier (Reinhardt-style): a held frontal shield with its own HP
	// that absorbs damage from the front, regenerates while lowered, and shatters
	// into a redeploy cooldown when depleted.
	shieldHP     float64 // barrier health (0..minoShieldMax)
	shieldUp     bool    // barrier currently deployed (B held)
	shieldBroken float64 // post-shatter cooldown remaining (sec); >0 = can't deploy

	// Elephant: a passive, omnidirectional shield buffer that soaks damage from
	// any side and recharges when not being hit (regenPause gates it), plus the
	// recharge timer on its trunk hook.
	bufferHP float64
	hookT    float64

	healFlash float64 // brief timer after being healed by an ally (drives the mend halo)

	// one damage-over-time slot (poison/burn; the latest application wins):
	// remaining time, HP/sec, fractional carry, the shooter (kill credit; leech
	// destination), whether ticks leech back to the shooter, and the feed label.
	dotT, dotPS, dotDebt float64
	dotFrom              int
	dotLeech             bool
	dotCause             KillCause

	regenDebt  float64 // passive HP regen: fractional carry between ticks
	regenPause float64 // sec until regen resumes after taking damage
	hpScale    float64 // survival wave HP multiplier (0/1 = none); see spawnWave
	respawn    float64
	guard      float64
	vy         float64 // vertical velocity (jump/gravity)
	hitFlash   float64 // brief flash timer after taking damage
	vote       int     // lobby vote: map index, or -1 for none
	ready      bool    // lobby: locked their vote (ENTER) -> counts toward fast-forward
	lives      int     // Survival: respawns remaining (humans)
	Team       int     // CTF: 0 or 1 (-1 = none in non-team modes)
	Party      string  // party name (client team-grouping hint; "" = solo)
	Carrying   int     // CTF: index of the enemy flag being carried, or -1
	shieldT    float64 // power-up: invulnerability remaining (sec)
	rapidT     float64 // power-up: rapid-fire remaining (sec)
	cloakT     float64 // power-up: cloak/invisibility remaining (sec)
	gone       bool    // player left; slot inert and reusable

	hazardDebt float64 // hazard-trait: fractional HP damage carried between ticks
	teleT      float64 // teleporter debounce remaining (sec); 0 = can teleport
	holdScore  int     // King of the Hill (FFA): hold-points accrued this match

	// per-bot AI, rolled from the active BotProfile at spawn (all 0 on humans, so
	// fire() wobble / reload-mul and AI gating are no-ops for players).
	aiSight, aiReact, aiTrack, aiWobble, aiFireMul, aiKeep float64
	aiSeek                                                 bool
	acquireT                                               float64 // reaction countdown after acquiring a target
	lastTgt                                                int     // last target index (to detect re-acquire); -1 = none
	roam                                                   V3      // wander destination when no enemy is in sight
	roamT                                                  float64 // time until a new wander destination is picked

	// grid-path cache (see nav.go): the route this bot is following, its
	// progress cursor, the goal cell it was computed for, the tick a repath
	// is due, and the grid signature it belongs to.
	navPath []int
	navAt   int
	navGoal int
	navTick int64
	navSig  uint64

	// aim-assist lock (human players): kind 0 none / 1 tank / 2 entity, idx into
	// that slice; lockBreak accumulates sustained turn input to release the lock;
	// lockCool suppresses re-acquire after a break so a held turn carries you off.
	lockKind  int
	lockIdx   int
	lockBreak float64
	lockCool  float64

	// event behaviors (mobile bosses / scripted actors): a tank can carry rules and
	// HP-watch thresholds just like an entity. Empty on ordinary tanks.
	Behaviors []Behavior
	Watch     []float64
	bDone     []bool
	wHit      []bool

	// per-match tallies for stats (accuracy, pickups, damage, support); ride
	// TankSnap for the door.
	shotsFired, shotsHit, pickups int
	dmgDealt, healDone            int

	lungeVX, lungeVZ float64 // transient forward leap velocity (melee charge), decays
}

// TankSnap is the renderable/transmittable view of a tank: exported, flat, no
// sim internals. ID is the stable world index (so the viewer matches by ID even
// as the snapshot omits vacated slots).
type TankSnap struct {
	ID                     int
	Pos                    V3
	HullYaw, TurretYaw     float64
	TurretPitch            float64 // gun elevation (+ up)
	HP                     int
	Color                  [3]float64
	Name                   string
	Dead, Bot, Shield, Hit bool
	Kills, Deaths          int
	Body                   int // render body style (BodyTank/BodySpider/...)
	ShotsFired             int // per-match tallies (stats: accuracy, pickups, damage, support)
	ShotsHit               int
	Pickups                int
	DmgDealt               int
	HealDone               int
	Lives                  int
	Team                   int     // CTF team (-1 in non-team modes)
	Carrying               bool    // CTF: carrying an enemy flag
	Cloak                  bool    // power-up: cloaked (hidden from enemies)
	Rapid                  bool    // power-up: rapid-fire active
	Shell                  bool    // turtle: tucked into its shell (immobile + invulnerable)
	Burning                bool    // taking burn/drain damage-over-time (ember tint)
	Poisoned               bool    // taking poison damage-over-time (sickly-green tint)
	Bleeding               bool    // taking bleed damage-over-time (red tint)
	Healing                bool    // just got healed by an ally (mend halo)
	ShieldUp               bool    // minotaur: frontal barrier deployed
	ShieldFrac             float64 // minotaur: barrier health 0..1 (HUD gauge + barrier fade)
	RespawnIn              float64
	Reload                 float64 // 0 = ready to fire, ->1 = just fired
	Ammo                   float64 // regenerating ammo, 0..1 of capacity (HUD gauge)
	Reload2                float64 // secondary recharge 0..1 (0=ready); cooldown weapons only
	Charges                int     // charge-weapon: remaining stock
	MaxCharges             int     // charge-weapon: capacity (0 = cooldown weapon)
	Slip                   bool    // EffSlip: no steering, helpless slide (client predicts it)
	HoldScore              int     // King of the Hill (FFA): hold-points
}

// PickKind enumerates power-up drop types.
const (
	PickRepair = iota // instant heal to full HP
	PickShield        // timed invulnerability
	PickRapid         // timed faster fire
	PickCloak         // timed invisibility
	PickAmmo          // instant ammo-pool refill
	PickWeapon        // swaps the player's secondary weapon (carries Weapon)
	pickKinds         // count (keep last)
)

// Pickup is a power-up drop sitting on the map.
type Pickup struct {
	Pos    V3
	Kind   int
	Weapon int // for PickWeapon: which weapon it grants (secondary slot)
}

// PickupSnap is the renderable/transmittable view of a pickup.
type PickupSnap struct {
	Pos    V3
	Kind   int
	Weapon int
}

// EntitySnap is the per-tick dynamic view of a map entity, positionally aligned
// to the active map's authored Entities (index i in the snap == map entity i).
// The client merges it with the static template it already holds from MsgMap:
// the template gives shape/kind/traits, the snap gives current HP/dead/facing.
type EntitySnap struct {
	HP    int
	Dead  bool
	Yaw   float64
	Pitch float64 // turret gun elevation (+ up)
	Pos   V3      // current position (dynamic for moved/payload entities)
}

type Projectile struct {
	Pos     V3
	vel     V3
	life    float64
	owner   int        // firing tank index; <0 = a map entity (e.g. a turret), no kill credit
	dmg     int        // 0 -> default projDmg
	eff     EffectKind // payload applied on hit (EffDamage = ordinary damage)
	mag     float64    // effect magnitude (heal amount, slow %, knockback force...)
	dur     float64    // effect duration (sec) for timed effects
	blast   float64    // splash radius (0 = direct hit only)
	affects Target     // who the effect applies to (foes / allies / both)
	grav    float64    // lobbed shots: downward accel (0 = straight)
	mine    bool       // dropped mine: stationary, triggers on a nearby foe
	armT    float64    // mine: arming countdown before it can trigger
	fx      bool       // visual-only spark (explosion debris); never collides
	vis     byte       // render kind (Vis*) carried to the client
	cause   KillCause  // kill-feed label for a lethal hit (zero = CauseCannon)
}

// Shot visual kinds, carried to the renderer so each projectile draws distinctly.
const (
	VisBolt    byte = iota // straight bolt (cannon, slow, etc.)
	VisGrenade             // arcing lob
	VisMine                // dropped mine
	VisBeam                // hitscan beam segment
	VisSpark               // explosion debris
	VisFlame               // fire-breath particle
)

// ShotSnap is one projectile to draw: position + visual kind + firer (so the client
// can tint it toward the owner's accent color; -1 for FX / environment).
type ShotSnap struct {
	Pos   V3
	Vis   byte
	Owner int
}

func visForDelivery(d Delivery) byte {
	switch d {
	case DeliverLob:
		return VisGrenade
	case DeliverMine:
		return VisMine
	case DeliverBeam:
		return VisBeam
	default:
		return VisBolt
	}
}

// ---------------------------------------------------------------------------
// Weapons & effects (data-driven; see WEAPONS.md). A weapon is a palette entry
// referenced by index (synced like Rulesets/Maps); a projectile carries the
// weapon's effect payload, resolved server-side on hit.
// ---------------------------------------------------------------------------

type Delivery int

const (
	DeliverBolt  Delivery = iota // straight projectile (today's shot)
	DeliverLob                   // arced/lobbed (grenade)        [W4]
	DeliverMine                  // dropped, proximity/timer fire [W4]
	DeliverBeam                  // hitscan (laser)               [W4]
	DeliverMelee                 // instant radial strike around the firer (no projectile)
	DeliverCone                  // forward cone AOE (fire breath); no projectile
)

type EffectKind int

const (
	EffDamage     EffectKind = iota // ordinary damage (uses WeaponDef.Damage)
	EffHeal                         // restore target HP
	EffSlow                         // reduce target move speed (timed)
	EffSpeed                        // boost target move speed (timed)
	EffShield                       // grant target invulnerability (timed)
	EffShieldBust                   // strip a target's shield
	EffKnockback                    // shove the target along the shot
	EffDamageUp                     // boost target's outgoing damage (timed) [W2+]
	EffDamageDown                   // cut target's outgoing damage (timed)   [W2+]
	EffTeleport                     // relocate the target                    [W4]
	EffPoison                       // damage over time (Mag HP/sec for Dur)
	EffDrain                        // damage over time that leeches the HP to the shooter
	EffBleed                        // damage over time from a wound (no leech; red tint)
	EffPull                         // yank the target in toward the shooter (the elephant's hook)
	EffSlip                         // no steering, helpless forward slide (timed)  [W2+]
)

// Target selects who a weapon's effect applies to.
type Target int

const (
	TargetFoes   Target = iota // enemies only (damage, debuffs)
	TargetAllies               // teammates/self only (heal, buffs)
	TargetBoth
)

type Effect struct {
	Kind EffectKind
	Mag  float64
	Dur  float64
}

// WeaponDef is one entry in the weapon palette.
type WeaponDef struct {
	Name     string
	Delivery Delivery
	Damage   int     // 0 = default projDmg (for EffDamage); for other effects, bonus damage
	Speed    float64 // 0 = use projSpeed
	Arc      float64 // gravity for lobbed shots (0 = flat)        [W4]
	Life     float64 // 0 = use projLife
	Cooldown float64 // 0 = use the vehicle's FireDelay
	Blast    float64 // splash radius (0 = direct hit)             [W4]
	Cost     float64 // ammo drawn per shot (0 = 1); the regen pool soft-limits fire
	Effect   Effect
	Affects  Target
	Glyph    byte      // wire-cheap render hint                   [W4]
	Cause    KillCause // kill-feed label (zero = CauseCannon)

	// Charge-stock secondaries (crab claw): the B-weapon holds a small stock of
	// charges that regenerate over time, instead of a single shared cooldown.
	Charges     int     // >0 = charge-stock weapon (max charges; 0 = plain cooldown)
	ChargeRegen float64 // sec to regenerate one consumed charge
}

// W4 delivery tuning.
const (
	mineArm     = 0.6  // sec before a dropped mine can trigger
	mineLife    = 30.0 // sec a mine persists if untriggered
	mineTrigger = 3.0  // a foe within this radius sets a mine off
	lobGravity  = 18.0 // default downward accel for lobbed shots
	lobLoft     = 6.0  // upward velocity added to a lob, so a level aim still arcs
	blastPush   = 3.0  // outward shove at the center of a damage blast
)

// Weapon palette indices (the wire identity; server and door share this build).
const (
	wepCannon   = iota // default damage bolt
	wepSlower          // drags the target's speed
	wepMedic           // heals an ally
	wepKnocker         // shoves the target back
	wepBuster          // strips a target's shield
	wepGrenade         // lobbed, blast-radius damage
	wepMine            // dropped, proximity blast
	wepLaser           // hitscan beam
	wepHealBomb        // lobbed blast that heals allies in radius (butterfly secondary)
	wepPound           // melee: instant radial strike that hits everyone in range (gorilla)
	wepFlame           // fire breath: forward cone AOE (t-rex)
	wepVenom           // poison spit: bite + damage-over-time (serpent)
	wepTusks           // melee gore: heavy radial strike + shove (elephant)
	wepAegis           // shield spray: forward cone that shields allies (elephant trunk)
	wepTalon           // fast light bolt, strafing-run cadence (falcon)
	wepGust            // wing blast: forward cone knockback (falcon)
	wepAura            // radial pulse: heals allies in range, stings foes (stag)
	wepSwift           // ally speed boost bolt; stings foes (stag)
	wepSnap            // close-range bite (turtle; its B became the shell, so the
	//                    primary has to carry its lethality)
	wepHammer  // heavy two-handed melee swing (minotaur; B raises the barrier)
	wepScratch // fast claw swipe that leaves a bleeding wound (tiger)
	wepHook    // trunk hook: a ranged grab that reels a foe in to melee (elephant)
	wepGun     // humanoid sidearm: fast, accurate bolt (the cannon was a tank-era artifact)
	wepSlash   // mantis lunge-strike: melee slash that pairs with a forward leap
	wepSpit    // insect acid spray: rapid cheap bolts + light poison (uses the deep magazine)
	// Secondary-weapons overhaul (Phase 1): per-character themed B-weapons.
	wepSpine  // mantis spine: light bolt that leaves a bleed
	wepSting  // scorpion sting: melee strike with a poison payload
	wepVSpray // serpent spray: slowing cone
	wepWeb    // insect web: lobbed heavy-slow glob
	wepInk    // octopod ink: slowing cone (Phase 2: self-cloak)
	wepRoar   // t-rex roar: melee debuff that cuts a foe's outgoing damage
	wepBanana // gorilla banana: lobbed peel that makes a foe slip
	wepSmoke  // tank smoke: lobbed slowing cloud (default body secondary)
	wepPounce // tiger pounce: melee leap-strike (Phase 2: dash + kill-reset)
	wepClaw   // crab claw: heavy melee with knockback (Phase 2: 2-charge stock)
	wepSand   // crab sand: fast cheap slowing cone (crab primary)
)

// Weapons is the built-in weapon palette. Referenced by index. CANNON preserves
// today's bolt; the rest carry effect payloads (resolved in applyShotHit) and/or
// delivery kinds (resolved in fireWeapon / stepProjectiles).
var Weapons = []WeaponDef{
	{Name: "CANNON", Delivery: DeliverBolt, Damage: 24, Cost: 1, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: 'o'},                                   // tank-only now; nerfed from the implicit projDmg (34) that made every cannon body top-tier
	{Name: "SLOWER", Delivery: DeliverBolt, Damage: 12, Cooldown: 1.0, Cost: 2, Effect: Effect{Kind: EffSlow, Mag: 0.55, Dur: 2.5}, Affects: TargetFoes, Glyph: '~'}, // octopod primary: chip + slow (first-pass dmg, tune in sim)
	{Name: "MEDIC", Delivery: DeliverBolt, Damage: 8, Cooldown: 1.2, Cost: 2, Effect: Effect{Kind: EffHeal, Mag: 25}, Affects: TargetAllies, Glyph: '+'},
	{Name: "KNOCKER", Delivery: DeliverBolt, Cooldown: 1.1, Cost: 2, Effect: Effect{Kind: EffKnockback, Mag: 4}, Affects: TargetFoes, Glyph: '*'},
	{Name: "BUSTER", Delivery: DeliverBolt, Cooldown: 1.5, Cost: 2, Effect: Effect{Kind: EffShieldBust}, Affects: TargetFoes, Glyph: 'x'},
	{Name: "GRENADE", Delivery: DeliverLob, Damage: 32, Speed: 20, Arc: lobGravity, Blast: 4, Cooldown: 1.3, Cost: 3, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: 'g'},
	{Name: "MINE", Delivery: DeliverMine, Damage: 45, Blast: 4, Cooldown: 2.0, Cost: 3, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: 'm'},
	{Name: "LASER", Delivery: DeliverBeam, Damage: 18, Life: 28, Cooldown: 0.5, Cost: 2, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: '='},
	{Name: "HEALBOMB", Delivery: DeliverLob, Speed: 20, Arc: lobGravity, Blast: 5, Cooldown: 1.6, Cost: 3, Effect: Effect{Kind: EffHeal, Mag: 30}, Affects: TargetAllies, Glyph: '+'},
	{Name: "POUND", Delivery: DeliverMelee, Damage: 38, Blast: 4.5, Cooldown: 0.7, Cost: 2, Effect: Effect{Kind: EffKnockback, Mag: 5}, Affects: TargetFoes, Glyph: '*', Cause: CauseMelee},
	// FLAME is the burn of the design notes: an initial bite plus a lingering
	// drain that leeches the burned HP back to the breather.
	{Name: "FLAME", Delivery: DeliverCone, Damage: 15, Blast: 9, Cooldown: 0.2, Cost: 1, Effect: Effect{Kind: EffDrain, Mag: 4, Dur: 3}, Affects: TargetFoes, Glyph: '^', Cause: CauseFire},
	{Name: "VENOM", Delivery: DeliverBolt, Damage: 6, Cooldown: 0.8, Cost: 1, Effect: Effect{Kind: EffPoison, Mag: 5, Dur: 4}, Affects: TargetFoes, Glyph: 'v', Cause: CausePoison},
	{Name: "TUSKS", Delivery: DeliverMelee, Damage: 30, Blast: 3.2, Cooldown: 0.9, Cost: 2, Effect: Effect{Kind: EffKnockback, Mag: 2.5}, Affects: TargetFoes, Glyph: '*', Cause: CauseMelee},
	{Name: "AEGIS", Delivery: DeliverCone, Blast: 7, Cooldown: 7.0, Cost: 3, Effect: Effect{Kind: EffShield, Dur: 2.5}, Affects: TargetAllies, Glyph: '+'},
	{Name: "TALON", Delivery: DeliverBolt, Damage: 10, Speed: 30, Cooldown: 0.28, Cost: 1, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: '\''},
	{Name: "GUST", Delivery: DeliverCone, Damage: 6, Blast: 7, Cooldown: 2.5, Cost: 2, Effect: Effect{Kind: EffKnockback, Mag: 5}, Affects: TargetFoes, Glyph: '~', Cause: CauseMelee},
	{Name: "AURA", Delivery: DeliverMelee, Damage: 6, Blast: 5, Cooldown: 1.0, Cost: 2, Effect: Effect{Kind: EffHeal, Mag: 14}, Affects: TargetAllies, Glyph: '+', Cause: CauseMelee},
	{Name: "SWIFT", Delivery: DeliverBolt, Damage: 6, Cooldown: 1.2, Cost: 2, Effect: Effect{Kind: EffSpeed, Mag: 0.45, Dur: 3}, Affects: TargetAllies, Glyph: '>', Cause: CauseMelee},
	{Name: "SNAP", Delivery: DeliverMelee, Damage: 16, Blast: 2.4, Cooldown: 0.8, Cost: 1, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: '*', Cause: CauseMelee},
	{Name: "HAMMER", Delivery: DeliverMelee, Damage: 45, Blast: 4.0, Cooldown: 1.0, Cost: 2, Effect: Effect{Kind: EffKnockback, Mag: 2}, Affects: TargetFoes, Glyph: '*', Cause: CauseMelee},
	{Name: "SCRATCH", Delivery: DeliverMelee, Damage: 12, Blast: 2.6, Cooldown: 0.55, Cost: 1, Effect: Effect{Kind: EffBleed, Mag: 6, Dur: 4}, Affects: TargetFoes, Glyph: '*', Cause: CauseBleed},
	// HOOK: a hitscan grab. Low cooldown here (the real gate is hookRecharge, a
	// separate timer) so reeling a foe in doesn't lock out the follow-up gore.
	{Name: "HOOK", Delivery: DeliverBeam, Damage: 8, Life: 22, Cooldown: 0.3, Cost: 1, Effect: Effect{Kind: EffPull, Mag: hookPullDist}, Affects: TargetFoes, Glyph: '=', Cause: CauseMelee},
	// Cannon-replacement kit (prototype): the humanoid carries a proper firearm,
	// the mantis lunges in for a melee slash, and the insect sprays its deep
	// magazine. Numbers are first-pass and meant to be tuned against balancesim.
	{Name: "GUN", Delivery: DeliverBolt, Damage: 18, Speed: 28, Cooldown: 0.40, Cost: 1, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: 'o'},
	{Name: "SLASH", Delivery: DeliverMelee, Damage: 24, Blast: 3.0, Cooldown: 0.5, Cost: 1, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: '*', Cause: CauseMelee},
	{Name: "SPIT", Delivery: DeliverBolt, Damage: 7, Speed: 26, Cooldown: 0.22, Cost: 1, Effect: Effect{Kind: EffPoison, Mag: 3, Dur: 2}, Affects: TargetFoes, Glyph: ':', Cause: CausePoison},
	// Secondary-weapons overhaul (Phase 1): first-pass stats, tuned later via balancesim.
	{Name: "SPINE", Delivery: DeliverBolt, Damage: 8, Speed: 28, Cooldown: 1.0, Cost: 2, Effect: Effect{Kind: EffBleed, Mag: 4, Dur: 3}, Affects: TargetFoes, Glyph: '\'', Cause: CauseBleed},
	{Name: "STING", Delivery: DeliverMelee, Damage: 12, Blast: 2.6, Cooldown: 1.3, Cost: 2, Effect: Effect{Kind: EffPoison, Mag: 6, Dur: 4}, Affects: TargetFoes, Glyph: '*', Cause: CausePoison},
	{Name: "SPRAY", Delivery: DeliverCone, Damage: 6, Blast: 7, Cooldown: 2.2, Cost: 2, Effect: Effect{Kind: EffSlow, Mag: 0.45, Dur: 2.5}, Affects: TargetFoes, Glyph: '~', Cause: CausePoison},
	{Name: "WEB", Delivery: DeliverLob, Speed: 20, Arc: lobGravity, Blast: 4, Cooldown: 2.5, Cost: 3, Effect: Effect{Kind: EffSlow, Mag: 0.7, Dur: 3}, Affects: TargetFoes, Glyph: '#'},
	// Phase 2 adds its special mechanic (INK self-cloak / POUNCE dash+kill-reset / CLAW 2-charge stock); for now it behaves as a plain cone/melee.
	{Name: "INK", Delivery: DeliverCone, Blast: 8, Cooldown: 3.0, Cost: 3, Effect: Effect{Kind: EffSlow, Mag: 0.5, Dur: 2.5}, Affects: TargetFoes, Glyph: '~'},
	{Name: "ROAR", Delivery: DeliverMelee, Blast: 6, Cooldown: 6.0, Cost: 3, Effect: Effect{Kind: EffDamageDown, Mag: 0.35, Dur: 4}, Affects: TargetFoes, Glyph: '!', Cause: CauseMelee},
	{Name: "BANANA", Delivery: DeliverLob, Damage: 4, Speed: 18, Arc: lobGravity, Blast: 3.5, Cooldown: 2.5, Cost: 2, Effect: Effect{Kind: EffSlip, Dur: 1.2}, Affects: TargetFoes, Glyph: '('},
	{Name: "SMOKE", Delivery: DeliverLob, Speed: 16, Arc: lobGravity, Blast: 6, Cooldown: 4.0, Cost: 2, Effect: Effect{Kind: EffSlow, Mag: 0.4, Dur: 3}, Affects: TargetFoes, Glyph: '%'},
	// Phase 2 adds its special mechanic (INK self-cloak / POUNCE dash+kill-reset / CLAW 2-charge stock); for now it behaves as a plain cone/melee.
	{Name: "POUNCE", Delivery: DeliverMelee, Damage: 18, Blast: 2.6, Cooldown: 1.5, Cost: 2, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: '*', Cause: CauseMelee},
	// Phase 2 adds its special mechanic (INK self-cloak / POUNCE dash+kill-reset / CLAW 2-charge stock); for now it behaves as a plain cone/melee.
	{Name: "CLAW", Delivery: DeliverMelee, Damage: 34, Blast: 3.0, Cooldown: 0.8, Cost: 2, Effect: Effect{Kind: EffKnockback, Mag: 2}, Affects: TargetFoes, Glyph: '*', Cause: CauseMelee, Charges: 2, ChargeRegen: 3.0},
	{Name: "SAND", Delivery: DeliverCone, Damage: 0, Blast: 8, Cooldown: 0.5, Cost: 1, Effect: Effect{Kind: EffSlow, Mag: 0.4, Dur: 1.8}, Affects: TargetFoes, Glyph: ':'},
}

// Flag is a Flag Run pickup, or (in CTF) a team flag that can be carried,
// dropped, returned, and captured.
type Flag struct {
	Pos   V3
	Taken bool // Flag Run: collected

	// CTF fields
	Home      V3      // base position this flag returns to
	Team      int     // owning/defending team (CTF); -1 in Flag Run
	Carrier   int     // tank index carrying it, or -1
	atHome    bool    // currently sitting at its base
	dropTimer float64 // sec until a dropped flag auto-returns home
}

// FlagSnap is the renderable/transmittable view of a flag. Team is -1 for
// neutral Flag Run pickups; Carried marks a CTF flag being carried.
type FlagSnap struct {
	Pos     V3
	Home    V3
	Team    int
	Carried bool
}

// Zone is a King-of-the-Hill control zone (runtime). Owner is the controlling
// team (team mode) or tank index (FFA), -1 = none. Prog is capture progress by
// `cont` toward Cap; hold is a fractional accumulator for awarding hold-points.
type Zone struct {
	Pos   V3
	Half  V3
	Cap   float64
	Owner int
	Prog  float64
	cont  int
	hold  float64
}

// ZoneSnap is the renderable/transmittable view of a control zone. Color is the
// controller's display color (grey when neutral), resolved server-side so the
// client needn't know the mode; Prog is 0..1 capture progress.
type ZoneSnap struct {
	Pos   V3
	Half  V3
	Prog  float64
	Color [3]float64
}

// MatchSnap is the transmittable match state (lifecycle, mode, clock, winner,
// KillCause labels how a tank died, for the kill feed / death banner.
type KillCause int

const (
	CauseCannon  KillCause = iota // a tank's main gun
	CauseTurret                   // a map turret entity
	CauseHazard                   // a hazard pad / environment
	CauseSuicide                  // self / unknown
	CauseMelee                    // a melee strike (gorilla pound, etc.)
	CauseFire                     // fire breath (t-rex)
	CausePoison                   // venom / burn damage-over-time
	CauseBleed                    // a bleeding wound (tiger's scratch)
)

// Word is the "...with a X" / "the X" fragment for the kill feed.
func (c KillCause) Word() string {
	switch c {
	case CauseTurret:
		return "a turret"
	case CauseHazard:
		return "a hazard"
	case CauseSuicide:
		return "the void"
	case CauseMelee:
		return "bare hands"
	case CauseFire:
		return "fire breath"
	case CausePoison:
		return "venom"
	case CauseBleed:
		return "blood loss"
	default:
		return "a cannon"
	}
}

// KillEvent is one kill this tick: who killed whom and how. Killer is -1 for
// environment kills (hazard / turret). Travels in MatchSnap so clients can show
// "X killed you", "KILLED Y", and a leaderboard +1.
type KillEvent struct {
	Killer int // tank index, or -1 (environment)
	Victim int // tank index
	Cause  KillCause
}

// and Flag Run progress).
type MatchSnap struct {
	Mode       Mode
	Phase      Phase
	Timer      float64
	WinnerID   int
	FlagsLeft  int
	FlagsTotal int
	Votes      []int       // lobby vote tally per map index (len = len(Maps)); mode is implied
	Kills      []KillEvent // kills that occurred this tick (kill feed / death banner)
	MapIdx     int         // active map index
	Wave       int         // Survival: current wave
	TeamScore  [2]int      // CTF: captures per team
	WinnerTeam int         // CTF: winning team (0/1), -1 = tie/none
	Events     []string    // author messages this tick (toast banners)
	Ready      int         // lobby: humans who have locked their vote (ENTER)
}

// Match returns the current match state for the snapshot/wire.
func (w *World) Match() MatchSnap {
	left := 0
	for i := range w.flags {
		if !w.flags[i].Taken {
			left++
		}
	}
	votes := make([]int, len(Maps)) // lobby votes are per map (mode implied by the map)
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if !t.Bot && !t.gone && t.vote >= 0 && t.vote < len(votes) {
			votes[t.vote]++
		}
	}
	return MatchSnap{
		Mode: w.Mode, Phase: w.Phase, Timer: w.Timer, WinnerID: w.WinnerID,
		FlagsLeft: left, FlagsTotal: len(w.flags), Votes: votes, Kills: w.kills, MapIdx: w.MapIdx, Wave: w.wave,
		TeamScore: w.teamScore, WinnerTeam: w.winnerTeam, Events: w.events, Ready: w.lobbyReadyCount(),
	}
}

type World struct {
	Tanks    []Tank
	Shots    []Projectile
	Mode     Mode
	Phase    Phase
	Timer    float64 // seconds left in the current phase
	WinnerID int     // tank index of the match winner (-1 = none/tie), valid in PhaseEnded
	flags    []Flag  // Flag Run pickups / CTF team flags
	Lobby    bool    // server arenas enable the between-match vote lobby + rotation
	rotIdx   int     // rotation cursor (fallback when no votes)
	MapIdx   int     // active map (index into Maps)
	wave     int     // Survival: current wave number
	pinned   bool    // map locked (no rotation) - offline testing/preview

	teamScore  [2]int // CTF: captures per team
	winnerTeam int    // CTF: winning team set at match end (-1 none)

	pickups     []Pickup // active power-up drops on the map
	pickupTimer float64  // sec until the next drop spawns

	entities []Entity // runtime trait-objects, instanced from ActiveMap().Entities
	zones    []Zone   // King of the Hill control zones (when the ruleset uses them)

	bots      BotProfile   // active difficulty profile (bot AI reads it; default NORMAL)
	assistAim bool         // sticky aim assist for human players (default on)
	kills     []KillEvent  // kills recorded this tick (consumed into MatchSnap)
	spawnQ    []Projectile // shots queued mid-step (explosion FX), appended after

	nav   *navGrid // bot pathfinding grid (lazy; re-baked when solids change)
	ticks int64    // simulation tick counter (drives nav repath staggering)

	// campLives, when >0, overrides the lives humans get at match start (the
	// FLAG RUN campaign carries a life pool across levels while the map's own
	// Rules.Lives=1 makes its bots stay dead).
	campLives int

	// demoHero (-1 = none) marks one bot as the attract demo's player stand-in:
	// it alone collects Flag Run flags (the rest defend, as they would against
	// a human), and it renders as the player's default tank.
	demoHero int

	// event-driven behavior runtime (see behavior.go / EVENTS.md)
	tickT     float64        // accumulator for the periodic `tick` signal
	vars      map[string]int // per-match blackboard
	logic     []Behavior     // active map's director rules
	logicDone []bool         // Once latch for director rules
	bus       []Signal       // signals queued this tick
	delayed   []delayedSig   // pending delayed emits
	events    []string       // author messages this tick (toasts -> MatchSnap.Events)
	started   bool           // whether the `start` signal has fired this match
	voteLog   []VoteEvent    // lobby vote commits since the last drain (server -> chat toasts)
}

// VoteEvent records a player committing a lobby vote, so the server can announce
// "<who> voted for <map>" as a chat toast. Drained each tick by the arena.
type VoteEvent struct {
	Who    string
	MapIdx int
}

// DrainVoteLog returns and clears the vote commits accumulated since the last
// call. The arena reads it every tick (so none are lost) and turns each into a
// chat notification.
func (w *World) DrainVoteLog() []VoteEvent {
	out := w.voteLog
	w.voteLog = nil
	return out
}

// SetAimAssist toggles sticky aim assist for human players.
func (w *World) SetAimAssist(on bool) { w.assistAim = on }

// SetCampaignLives sets the life pool humans start the next match with,
// overriding the map's Rules.Lives for humans only (campaign carry-over).
func (w *World) SetCampaignLives(n int) { w.campLives = n }

// SetDemoHero marks bot i as the attract demo's player stand-in (see demoHero).
func (w *World) SetDemoHero(i int) { w.demoHero = i }

// DemoHero returns the stand-in's tank index (-1 = none).
func (w *World) DemoHero() int { return w.demoHero }

// SetDifficulty sets the active bot profile; takes effect at the next match start
// (when bots re-roll their per-bot AI). Offline play sets this from the user's
// chosen tier; the arena server sets it from its config.
func (w *World) SetDifficulty(d Difficulty) { w.bots = ProfileFor(d) }

// rollBotLook gives a bot a fresh character (chassis + body + matching secondary),
// so the field changes appearance between matches like the player can re-pick.
func (w *World) rollBotLook(i int) {
	t := &w.Tanks[i]
	if i == w.demoHero { // the demo's player stand-in looks like the player default
		t.body = BodyTank
		t.custom = nil
		t.weapon2 = defaultSecondary(BodyTank)
		return
	}
	t.body = botBodies[rand.Intn(len(botBodies))] // each character owns its stats
	t.custom = nil
	t.weapon2 = defaultSecondary(t.body)
}

// SetBotBody locks bot i to a specific character (body + its paired chassis),
// overriding the random roll, and resets HP/ammo/secondary plus any body-specific
// join state to the new character. For the balance simulator's controlled rosters:
// startMatch re-rolls bot looks, so call this AFTER SkipCountdown/startMatch.
// respawns() preserves t.body, so the override holds for the whole match.
func (w *World) SetBotBody(i, body int) {
	if i < 0 || i >= len(w.Tanks) || !w.Tanks[i].Bot {
		return
	}
	t := &w.Tanks[i]
	t.body = body
	t.custom = nil
	t.weapon2 = defaultSecondary(body)
	t.HP = t.veh().MaxHP
	t.ammo = t.veh().AmmoMax
	t.shieldHP, t.bufferHP = 0, 0
	if body == BodyMinotaur {
		t.shieldHP = minoShieldMax // join with a full barrier
	}
	if body == BodyElephant {
		t.bufferHP = elephantBufferMax // join with a full shield buffer
	}
}

// assignHealers gives each team exactly one butterfly healer (a strong team pick that
// never gets chosen randomly - it's left out of botBodies since it's useless without
// allies). Called after teams are assigned + looks rolled, so there's one per side.
func (w *World) assignHealers() {
	for team := 0; team < 2; team++ {
		var bots []int
		has := false
		for i := range w.Tanks {
			t := &w.Tanks[i]
			if !t.Bot || t.gone || t.Team != team {
				continue
			}
			bots = append(bots, i)
			if t.body == BodyButterfly {
				has = true
			}
		}
		if has || len(bots) < 2 { // already covered, or too few to spare one
			continue
		}
		t := &w.Tanks[bots[rand.Intn(len(bots))]]
		t.body = BodyButterfly
		t.custom, t.weapon2 = nil, wepHealBomb
		t.HP, t.ammo = VehBody(t.body).MaxHP, VehBody(t.body).AmmoMax
	}
}

// mostHurtAlly returns the most-wounded living teammate worth healing, or -1.
func (w *World) mostHurtAlly(self int) int {
	if w.rules().Teams != 2 {
		return -1 // no allies to heal outside team modes
	}
	s := &w.Tanks[self]
	best, bestPct := -1, 0.92 // only bother below ~92% HP
	for j := range w.Tanks {
		t := &w.Tanks[j]
		if j == self || t.Dead || t.gone || t.Team != s.Team {
			continue
		}
		max := t.veh().MaxHP
		if max <= 0 {
			max = 1
		}
		if pct := float64(t.HP) / float64(max); pct < bestPct {
			bestPct, best = pct, j
		}
	}
	return best
}

// nearestAlly returns the closest living teammate (-1 outside team modes / none).
func (w *World) nearestAlly(self int) int {
	if w.rules().Teams != 2 {
		return -1
	}
	s := &w.Tanks[self]
	best, bestD := -1, math.MaxFloat64
	for j := range w.Tanks {
		t := &w.Tanks[j]
		if j == self || t.Dead || t.gone || t.Team != s.Team {
			continue
		}
		d := t.Pos.Sub(s.Pos)
		if dd := d.X*d.X + d.Z*d.Z; dd < bestD {
			bestD, best = dd, j
		}
	}
	return best
}

// botHealerAI drives a butterfly bot to mend its most-wounded ally (the medic bolt
// heals on hit); with no one hurt it trails the squad rather than wandering off.
func (w *World) botHealerAI(i int, dt float64) {
	b := &w.Tanks[i]
	v := b.veh()
	ally := w.mostHurtAlly(i)
	if ally < 0 {
		if mate := w.nearestAlly(i); mate >= 0 { // stick with the team
			d := w.Tanks[mate].Pos.Sub(b.Pos)
			ang := math.Atan2(d.X, d.Z)
			b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, w.Tanks[mate].Pos, ang)), v.HullTurn*dt)
			if math.Hypot(d.X, d.Z) > 6 {
				w.driveForward(i, dt, 0.7)
			}
			w.botVertical(i, dt, false)
			return
		}
		w.botWander(i, dt)
		return
	}
	a := &w.Tanks[ally]
	d := a.Pos.Sub(b.Pos)
	dist := math.Hypot(d.X, d.Z)
	ang := math.Atan2(d.X, d.Z)
	b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, a.Pos, ang)), v.HullTurn*dt)
	wantPitch := clampPitch(math.Atan2((a.Pos.Y+turretAimHeight)-(b.Pos.Y+EyeHeight), dist))
	b.TurretYaw = turnToward(b.TurretYaw, angDiff(ang, b.HullYaw), b.aiTrack*dt)
	b.TurretPitch = turnToward(b.TurretPitch, wantPitch, b.aiTrack*dt)
	approach, radial := 9.0, false // butterfly: close to heal-beam range, then hold
	if b.body == BodyStag {
		approach, radial = 3.5, true // the aura is a radial pulse: get alongside
	}
	if dist > approach {
		w.driveForward(i, dt, 0.8)
	}
	w.botVertical(i, dt, w.wantHop(b))
	if radial {
		if dist < 4.5 && b.cooldown <= 0 {
			w.fire(i) // aura pulse -> heals everyone alongside
		} else if b.weapon2 == wepSwift && b.cooldown2 <= 0 && dist > 6 && dist < 18 &&
			math.Abs(angDiff(b.HullYaw+b.TurretYaw, ang)) < botAimTol &&
			math.Abs(wantPitch-b.TurretPitch) < botAimTol {
			w.fireWeapon(i, &Weapons[wepSwift], true) // too far to pulse: speed the ally instead
		}
	} else if dist < botFireRange && math.Abs(angDiff(b.HullYaw+b.TurretYaw, ang)) < botAimTol &&
		math.Abs(wantPitch-b.TurretPitch) < botAimTol && b.cooldown <= 0 {
		w.fire(i) // medic bolt -> heals the ally
	}
}

// rollBotAI rolls a bot's per-bot AI params around the active profile's centers,
// so bots within a tier vary (engagement distance, react/track, wobble, cadence).
func (w *World) rollBotAI(i int) {
	p := w.bots
	jit := func(c, frac float64) float64 { return c * (1 + (rand.Float64()*2-1)*frac) }
	b := &w.Tanks[i]
	b.aiTrack = jit(p.TrackRate, 0.15)
	b.aiReact = jit(p.ReactDelay, 0.35)
	b.aiWobble = jit(p.Wobble, 0.35)
	b.aiFireMul = jit(p.FireDelayMul, 0.10)
	b.aiKeep = jit(botKeepDist, 0.30)
	if p.Sight <= 0 {
		b.aiSight = 0 // unlimited
	} else {
		b.aiSight = jit(p.Sight, 0.15)
	}
	b.aiSeek = p.SeekPickups
	b.acquireT, b.lastTgt = 0, -1
}

// Entities returns per-tick dynamic snapshots of the world's entities, aligned
// to ActiveMap().Entities by index (for the wire and the server's own renderer).
func (w *World) Entities() []EntitySnap {
	out := make([]EntitySnap, len(w.entities))
	for i := range w.entities {
		out[i] = EntitySnap{HP: w.entities[i].HP, Dead: w.entities[i].Dead, Yaw: w.entities[i].Yaw, Pitch: w.entities[i].Pitch, Pos: w.entities[i].Pos}
	}
	return out
}

// Zones returns per-tick snapshots of control zones, with the controller's color
// resolved (team shade in team modes, the owning tank's color in FFA, grey when
// neutral) so the client renders without knowing the mode.
func (w *World) Zones() []ZoneSnap {
	teamMode := w.rules().Teams == 2
	out := make([]ZoneSnap, len(w.zones))
	for i := range w.zones {
		z := &w.zones[i]
		col := [3]float64{0.55, 0.55, 0.6} // neutral grey
		if z.Owner >= 0 {
			if teamMode {
				col = teamColor(z.Owner, 0)
			} else if z.Owner < len(w.Tanks) {
				col = w.Tanks[z.Owner].Color
			}
		}
		prog := 0.0
		if z.Cap > 0 {
			prog = z.Prog / z.Cap
		}
		out[i] = ZoneSnap{Pos: z.Pos, Half: z.Half, Prog: prog, Color: col}
	}
	return out
}

// resetEntities re-instantiates the active map's entities for a new match.
func (w *World) resetEntities() {
	w.entities = w.ActiveMap().NewEntities()
	w.resetBehaviors() // re-seed blackboard + director rules, clear the bus
}

// ActiveMap returns the world's current map.
func (w *World) ActiveMap() Map {
	if len(Maps) == 0 {
		return Map{}
	}
	return Maps[w.MapIdx%len(Maps)]
}

// FindMap returns the index of the map whose name matches (case-insensitive), or
// -1. Useful for forcing a specific map (e.g. a testing/preview hook).
func FindMap(name string) int {
	for i := range Maps {
		if strings.EqualFold(Maps[i].Name, name) {
			return i
		}
	}
	return -1
}

// PinMap forces the active map to idx and stops the per-match rotation, so the
// world stays on one layout. Used by offline testing/preview; ignored if out of
// range.
func (w *World) PinMap(idx int) {
	if idx < 0 || idx >= len(Maps) {
		return
	}
	w.MapIdx, w.pinned = idx, true
}

// collide pushes a point out of the world's live collidables (static obstacles
// plus alive solid entities), so a destructible wall blocks until destroyed.
func (w *World) collide(p *V3) {
	CollideBoxes(w.collidables(), p)
	CollideRamps(w.ActiveMap().Ramps, p)
}

// CollideRamps blocks horizontal movement into the SOLID body of a ramp - the
// wedge beneath its sloped surface - so a ramp is a solid wedge, not a tent you
// can walk under or through the back of. Walking UP the incline or standing on
// top is unaffected: there your feet sit at/above the local surface, so the
// block (which only fires when you're below the surface) never triggers. Shared
// by the server/offline sim and the net predictor.
func CollideRamps(ramps []Ramp, p *V3) {
	const rad = 1.0
	for i := range ramps {
		r := ramps[i]
		minx, maxx := r.Pos.X-r.Half.X-rad, r.Pos.X+r.Half.X+rad
		minz, maxz := r.Pos.Z-r.Half.Z-rad, r.Pos.Z+r.Half.Z+rad
		if p.X <= minx || p.X >= maxx || p.Z <= minz || p.Z >= maxz {
			continue
		}
		// Surface height at the nearest point of the ramp footprint to us.
		cx := math.Max(r.Pos.X-r.Half.X, math.Min(p.X, r.Pos.X+r.Half.X))
		cz := math.Max(r.Pos.Z-r.Half.Z, math.Min(p.Z, r.Pos.Z+r.Half.Z))
		rh, ok := rampHeight(r, cx, cz)
		if !ok || p.Y >= rh-stepUp {
			continue // on/above the surface (walking up or standing): not the wedge
		}
		dxl, dxr, dzl, dzr := p.X-minx, maxx-p.X, p.Z-minz, maxz-p.Z
		mn, axis := dxl, 0
		if dxr < mn {
			mn, axis = dxr, 1
		}
		if dzl < mn {
			mn, axis = dzl, 2
		}
		if dzr < mn {
			mn, axis = dzr, 3
		}
		switch axis {
		case 0:
			p.X = minx
		case 1:
			p.X = maxx
		case 2:
			p.Z = minz
		case 3:
			p.Z = maxz
		}
	}
}

// collidables is the set a tank/shot collides with this tick: the map's static
// obstacles plus a box per alive, solid entity. Returns the map slice directly
// when there are no solid entities (the common case, no allocation).
func (w *World) collidables() []Box {
	m := w.ActiveMap()
	boxes := m.Obstacles
	allocated := false
	for i := range w.entities {
		e := &w.entities[i]
		if !e.Solid || e.Dead {
			continue
		}
		if !allocated { // copy-on-first-write so we never mutate the embedded map
			boxes = append([]Box(nil), m.Obstacles...)
			allocated = true
		}
		boxes = append(boxes, Box{Pos: e.Pos, Half: e.Half, Color: e.Color})
	}
	return boxes
}

// SolidBoxes returns collision boxes for the alive, solid entities of a map
// given their per-tick state (ents aligned to m.Entities by index). The net
// client builds this from the snapshot so its predictor matches the server's
// dynamic collision (a destroyed wall stops blocking immediately).
func SolidBoxes(m Map, ents []EntitySnap) []Box {
	var out []Box
	for i := range m.Entities {
		e := m.Entities[i]
		if !e.Solid {
			continue
		}
		if i < len(ents) && ents[i].Dead {
			continue
		}
		out = append(out, Box{Pos: e.Pos, Half: e.Half, Color: e.Color})
	}
	return out
}

// CollideMap pushes a point out of a map's static obstacles only (no entities).
func CollideMap(m Map, p *V3) { CollideBoxes(m.Obstacles, p) }

// CollideBoxes pushes a point out of any box's SIDE (horizontal AABB inflated by
// the tank radius). If the point is at/above a box's top (within stepUp), it
// isn't blocked — you ride over/stand on the top. Shared by the server and the
// client predictor.
func CollideBoxes(boxes []Box, p *V3) {
	const rad = 1.0
	for _, b := range boxes {
		if p.Y >= b.Pos.Y+b.Half.Y-stepUp {
			continue // on top of / above it: no side block
		}
		minx, maxx := b.Pos.X-b.Half.X-rad, b.Pos.X+b.Half.X+rad
		minz, maxz := b.Pos.Z-b.Half.Z-rad, b.Pos.Z+b.Half.Z+rad
		if p.X <= minx || p.X >= maxx || p.Z <= minz || p.Z >= maxz {
			continue
		}
		dxl, dxr, dzl, dzr := p.X-minx, maxx-p.X, p.Z-minz, maxz-p.Z
		mn, axis := dxl, 0
		if dxr < mn {
			mn, axis = dxr, 1
		}
		if dzl < mn {
			mn, axis = dzl, 2
		}
		if dzr < mn {
			mn, axis = dzr, 3
		}
		switch axis {
		case 0:
			p.X = minx
		case 1:
			p.X = maxx
		case 2:
			p.Z = minz
		case 3:
			p.Z = maxz
		}
	}
}

// obstacleSide reports whether p is pressed against an obstacle's vertical face -
// inside its inflated footprint and below its climbable top - the exact condition
// CollideBoxes uses to push a tank out. The insect uses this to scale the face
// instead of bouncing off it. Arena border walls (clampArena) and ramps are not
// collidables here, so you scale props, not your way out of the arena.
func (w *World) obstacleSide(p V3) bool {
	const rad = 1.0
	for _, b := range w.collidables() {
		if p.Y >= b.Pos.Y+b.Half.Y-stepUp {
			continue // at/above the top: that's the surface, not a face
		}
		if math.Abs(p.X-b.Pos.X) < b.Half.X+rad && math.Abs(p.Z-b.Pos.Z) < b.Half.Z+rad {
			return true
		}
	}
	return false
}

// climbCrest returns the resting spot atop the obstacle whose face p is scaling:
// the box top, with p's XZ pulled just inside the footprint so the climber stands
// ON the surface instead of bobbing at the lip (where the ground is still the
// floor and gravity drags it back off). ok=false if p isn't pressing a face.
func (w *World) climbCrest(p V3) (V3, bool) {
	const rad, inset = 1.0, 0.4
	for _, b := range w.collidables() {
		if p.Y >= b.Pos.Y+b.Half.Y-stepUp {
			continue
		}
		if math.Abs(p.X-b.Pos.X) < b.Half.X+rad && math.Abs(p.Z-b.Pos.Z) < b.Half.Z+rad {
			x := math.Max(b.Pos.X-b.Half.X+inset, math.Min(p.X, b.Pos.X+b.Half.X-inset))
			z := math.Max(b.Pos.Z-b.Half.Z+inset, math.Min(p.Z, b.Pos.Z+b.Half.Z-inset))
			return V3{x, b.Pos.Y + b.Half.Y, z}, true
		}
	}
	return V3{}, false
}

// GroundHeight returns the surface height under (x,z) for a tank whose feet are
// at feetY: the top of any box/ramp it's over and can reach (feet within stepUp
// of the surface), else 0 (the floor). This is how tanks stand on objects.
func GroundHeight(m Map, x, z, feetY float64) float64 {
	return GroundBoxes(m.Obstacles, m.Ramps, x, z, feetY)
}

// GroundBoxes is GroundHeight over an explicit box set (so the caller can fold in
// dynamic solid entities), plus the map's ramps.
func GroundBoxes(boxes []Box, ramps []Ramp, x, z, feetY float64) float64 {
	h := 0.0
	for _, b := range boxes {
		if math.Abs(x-b.Pos.X) < b.Half.X && math.Abs(z-b.Pos.Z) < b.Half.Z {
			if top := b.Pos.Y + b.Half.Y; feetY >= top-stepUp && top > h {
				h = top
			}
		}
	}
	for _, r := range ramps {
		if rh, ok := rampHeight(r, x, z); ok && feetY >= rh-stepUp && rh > h {
			h = rh
		}
	}
	return h
}

// ground is GroundHeight over the world's live collidables (obstacles + alive
// solid entities) so tanks can stand on a solid crate/turret base.
func (w *World) ground(x, z, feetY float64) float64 {
	return GroundBoxes(w.collidables(), w.ActiveMap().Ramps, x, z, feetY)
}

// zoneAir is how far above the local surface still counts as "on the hill" - a
// jump's worth, so jumping or a low hover while holding doesn't drop the capture.
const zoneAir = 1.6

// inZone reports whether a tank at p is holding zone z: inside the XZ footprint
// AND standing on the ground surface there (within stepUp below to a jump above) -
// NOT flying high over it, NOT at the base below it. Because an elevated hill's
// footprint sits on top of its platform, the only place you can be inside it is
// up on the platform, so capturing it requires climbing; a flat or low hill (or
// the auto-placed default hill on any terrain) captures from its surface as
// before. Anchoring to the live surface makes it robust to any platform height
// (the authored Pos.Y band wrongly froze maps whose dais was a hair too tall).
func (w *World) inZone(z *Zone, p V3) bool {
	if math.Abs(p.X-z.Pos.X) >= z.Half.X || math.Abs(p.Z-z.Pos.Z) >= z.Half.Z {
		return false
	}
	// Probe the terrain TOP at this XZ (large feetY) - the hill's standing surface,
	// independent of where the tank currently is. You hold the hill only if your
	// feet are at that surface (a stepUp below to a jump above); below it you're
	// inside/at the base of the platform (not on the hill), above it you're flying.
	surf := w.ground(p.X, p.Z, 1e9)
	return p.Y >= surf-stepUp && p.Y <= surf+zoneAir
}

// rampHeight returns the ramp surface height at (x,z) and whether (x,z) is on it.
func rampHeight(r Ramp, x, z float64) (float64, bool) {
	if math.Abs(x-r.Pos.X) > r.Half.X || math.Abs(z-r.Pos.Z) > r.Half.Z {
		return 0, false
	}
	var frac float64
	switch r.Dir {
	case 0: // rises toward +X
		frac = (x - (r.Pos.X - r.Half.X)) / (2 * r.Half.X)
	case 1: // -X
		frac = ((r.Pos.X + r.Half.X) - x) / (2 * r.Half.X)
	case 2: // +Z
		frac = (z - (r.Pos.Z - r.Half.Z)) / (2 * r.Half.Z)
	default: // -Z
		frac = ((r.Pos.Z + r.Half.Z) - z) / (2 * r.Half.Z)
	}
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	return frac * r.H, true
}

// rampMouthXZ / rampTopXZ give a ramp's low (ground) and high (platform) ends in
// XZ, by rise direction. Dir: 0=+X, 1=-X, 2=+Z, 3=-Z (the high side).
func rampMouthXZ(r Ramp) (float64, float64) {
	switch r.Dir {
	case 0:
		return r.Pos.X - r.Half.X, r.Pos.Z
	case 1:
		return r.Pos.X + r.Half.X, r.Pos.Z
	case 2:
		return r.Pos.X, r.Pos.Z - r.Half.Z
	default:
		return r.Pos.X, r.Pos.Z + r.Half.Z
	}
}

func rampTopXZ(r Ramp) (float64, float64) {
	switch r.Dir {
	case 0:
		return r.Pos.X + r.Half.X, r.Pos.Z
	case 1:
		return r.Pos.X - r.Half.X, r.Pos.Z
	case 2:
		return r.Pos.X, r.Pos.Z + r.Half.Z
	default:
		return r.Pos.X, r.Pos.Z - r.Half.Z
	}
}

// rampApproach steers a bot up to an elevated hill: it picks the ramp leading up
// to the zone whose mouth is nearest the bot, and returns the mouth to drive to
// (to get onto the ramp) or, once the bot is on the ramp, its high end (to climb
// onto the platform). Returns false if the hill has no ascending ramp (e.g. a
// stepped pyramid - the bot just walks up via stepUp).
func (w *World) rampApproach(z *Zone, p V3) (V3, bool) {
	ramps := w.ActiveMap().Ramps
	best, bestD := -1, math.MaxFloat64
	for i := range ramps {
		tx, tz := rampTopXZ(ramps[i])
		if math.Abs(tx-z.Pos.X) > z.Half.X+3 || math.Abs(tz-z.Pos.Z) > z.Half.Z+3 {
			continue // this ramp doesn't lead up to this hill
		}
		mx, mz := rampMouthXZ(ramps[i])
		if d := (mx-p.X)*(mx-p.X) + (mz-p.Z)*(mz-p.Z); d < bestD {
			bestD, best = d, i
		}
	}
	if best < 0 {
		return V3{}, false
	}
	r := ramps[best]
	if math.Abs(p.X-r.Pos.X) < r.Half.X && math.Abs(p.Z-r.Pos.Z) < r.Half.Z {
		tx, tz := rampTopXZ(r) // already on the ramp: climb toward the platform
		return V3{X: tx, Z: tz}, true
	}
	mx, mz := rampMouthXZ(r) // get onto the ramp at its base first
	return V3{X: mx, Z: mz}, true
}

// NewWorld creates a world for the given mode, seeded with numBots AI tanks
// (indices 0..numBots-1). Human players are added afterward with AddPlayer. The
// match starts in a countdown.
func NewWorld(numBots int, mode Mode) *World {
	w := &World{Mode: mode, Phase: PhaseCountdown, Timer: countdownTime, WinnerID: -1,
		demoHero: -1, bots: ProfileFor(DiffNormal), assistAim: true} // gentler default; setters override
	if len(Maps) > 0 {
		w.MapIdx = randomMapIdx(mode, numBots+1) // a map suited to the mode and session size
	}
	for b := 0; b < numBots; b++ {
		body := botBodies[rand.Intn(len(botBodies))] // each character owns its stats
		w.Tanks = append(w.Tanks, Tank{
			Bot: true, HP: VehBody(body).MaxHP, ammo: VehBody(body).AmmoMax, guard: spawnGuardTime, vote: -1,
			body:  body,
			Color: BotPalette[b%len(BotPalette)], Name: botName(b), Team: -1, Carrying: -1, weapon2: wepGrenade,
		})
		w.Tanks[b].Pos = w.spawnPoint(b)
		w.rollBotAI(b)
	}
	return w
}

// BotCallsigns name the AI tanks (better feedback than "yellow tank"). Beyond the
// pool, names get a numeric suffix so they stay unique.
var BotCallsigns = []string{
	"RAZOR", "VIPER", "GHOST", "NOMAD", "HAVOC", "ROOK", "ZEALOT", "BISHOP",
	"OMEN", "TANGO", "DELTA", "WIDOW", "CINDER", "JACKAL", "ONYX", "REBEL",
}

func botName(i int) string {
	n := BotCallsigns[i%len(BotCallsigns)]
	if i >= len(BotCallsigns) {
		n += strconv.Itoa(i/len(BotCallsigns) + 1)
	}
	return n
}

// AddPlayer inserts a human tank (reusing a vacated slot if any) and returns its
// index. color may be the zero value to auto-pick from PlayerPalette. body is the
// render silhouette and the source of the tank's sim stats (VehBody).
func (w *World) AddPlayer(color [3]float64, name string, body int) int {
	color = w.freeColor(color) // honor the pick unless another player already wears it
	if name == "" {
		name = "PLAYER"
	}
	mk := func(i int) Tank {
		eff := VehBody(body)
		t := Tank{HP: eff.MaxHP, ammo: eff.AmmoMax, Color: color, Name: name, guard: spawnGuardTime, vote: -1, body: body, lives: survivalLives, Team: -1, Carrying: -1, weapon2: defaultSecondary(body)}
		if body == BodyMinotaur {
			t.shieldHP = minoShieldMax // join with a full barrier
		}
		if body == BodyElephant {
			t.bufferHP = elephantBufferMax // join with a full shield buffer
		}
		t.Pos = w.spawnPoint(i)
		t.HullYaw = w.faceTarget(i)
		return t
	}
	for i := range w.Tanks {
		if w.Tanks[i].gone && !w.Tanks[i].Bot { // reuse vacated human slots only (bot pool is reserved)
			w.Tanks[i] = mk(i)
			return i
		}
	}
	i := len(w.Tanks)
	w.Tanks = append(w.Tanks, Tank{})
	w.Tanks[i] = mk(i)
	return i
}

// SetPlayerLoadout swaps a human tank's character (body/color/secondary).
// The body-derived fields take effect on the next respawn (respawns() refreshes
// HP/ammo from the new body), so the natural use is to change while dead. It
// never touches live HP, so it can't be abused as a mid-fight heal.
func (w *World) SetPlayerLoadout(i int, color [3]float64, body int) {
	if i < 0 || i >= len(w.Tanks) {
		return
	}
	t := &w.Tanks[i]
	if t.gone || t.Bot {
		return
	}
	t.body = body
	t.custom = nil
	t.Color = w.freeColor(color)
	t.weapon2 = defaultSecondary(body)
	switch body {
	case BodyMinotaur:
		t.shieldHP = minoShieldMax
	case BodyElephant:
		t.bufferHP = elephantBufferMax
	}
}

// RemovePlayer marks a tank's slot vacated (skipped everywhere, reusable).
func (w *World) RemovePlayer(i int) {
	if i >= 0 && i < len(w.Tanks) {
		w.Tanks[i].gone = true
		w.Tanks[i].Dead = true
	}
}

func (w *World) humanCount() int {
	n := 0
	for i := range w.Tanks {
		if !w.Tanks[i].Bot && !w.Tanks[i].gone {
			n++
		}
	}
	return n
}

func fwd(yaw float64) V3           { return V3{math.Sin(yaw), 0, math.Cos(yaw)} }
func angDiff(a, b float64) float64 { return math.Atan2(math.Sin(a-b), math.Cos(a-b)) }

// aimDir is the unit firing direction for a yaw + gun pitch (+pitch = up).
func aimDir(yaw, pitch float64) V3 {
	cp := math.Cos(pitch)
	return V3{math.Sin(yaw) * cp, math.Sin(pitch), math.Cos(yaw) * cp}
}

func clampPitch(p float64) float64 {
	if p > pitchMax {
		return pitchMax
	}
	if p < pitchMin {
		return pitchMin
	}
	return p
}

func turnToward(cur, target, step float64) float64 {
	d := angDiff(target, cur)
	if math.Abs(d) <= step {
		return target
	}
	if d > 0 {
		return cur + step
	}
	return cur - step
}

func clampArena(p *V3, half float64) {
	if p.X > half {
		p.X = half
	} else if p.X < -half {
		p.X = -half
	}
	if p.Z > half {
		p.Z = half
	} else if p.Z < -half {
		p.Z = -half
	}
}

// half is the active map's play boundary.
func (w *World) half() float64 { return MapHalf(w.ActiveMap()) }

// fire shoots a tank's primary weapon (the cannon for now).
func (w *World) fire(owner int) {
	t := &w.Tanks[owner]
	if t.shieldUp { // can't swing while bracing the barrier (covers bots too)
		return
	}
	if t.body == BodyElephant { // smart primary: gore up close, else hook them in
		w.elephantFire(owner)
		return
	}
	def := &Weapons[wepCannon]
	if bd := bodyDefFor(t.body); bd.weapon >= 0 { // beasts fire their signature
		def = &Weapons[bd.weapon]
	}
	w.fireWeapon(owner, def, false)
}

// elephantFire gores with TUSKS when a foe is already in reach, otherwise fires
// the trunk HOOK to reel one in. The hook is gated by its own recharge (hookT,
// not the shared fire cooldown) so a grab never locks out the follow-up gore.
func (w *World) elephantFire(owner int) {
	t := &w.Tanks[owner]
	if w.foeInRange(owner, Weapons[wepTusks].Blast+1.0) {
		w.fireWeapon(owner, &Weapons[wepTusks], false)
		return
	}
	if t.hookT <= 0 {
		w.fireWeapon(owner, &Weapons[wepHook], false)
		t.hookT = hookRecharge
	}
}

// foeInRange reports whether the nearest opponent is within r units.
func (w *World) foeInRange(i int, r float64) bool {
	tgt := w.nearestEnemy(i)
	if tgt < 0 {
		return false
	}
	d := w.Tanks[tgt].Pos.Sub(w.Tanks[i].Pos)
	return d.X*d.X+d.Z*d.Z <= r*r
}

const lungeSpeed = 16.0 // forward leap speed for melee chargers (decays via stepLunge)

const (
	pounceWindow = 1.0 // tiger POUNCE: window after the dash in which a kill refunds it
	inkCloakDur  = 2.0 // octopod INK: self-cloak duration the ink screen grants the caster
)

// fireSecondary fires tank i's B-weapon, honoring charge-stock weapons and any
// on-cast self effect. Shared by the player path and the bot AI so both behave
// identically.
func (w *World) fireSecondary(i int) {
	t := &w.Tanks[i]
	if t.weapon2 <= 0 || t.weapon2 >= len(Weapons) {
		return
	}
	def := &Weapons[t.weapon2]
	if def.Charges > 0 { // charge-stock (crab claw: one per claw, regenerating)
		if t.wp2Used >= def.Charges || t.cooldown2 > 0 {
			return
		}
		w.fireWeapon(i, def, true)
		t.wp2Used++
		if t.wp2RegenT <= 0 {
			t.wp2RegenT = def.ChargeRegen
		}
	} else {
		if t.cooldown2 > 0 {
			return
		}
		w.fireWeapon(i, def, true)
	}
	w.onSecondaryCast(i, t.weapon2)
}

// onSecondaryCast applies a secondary's on-cast self effect (dash, cloak).
func (w *World) onSecondaryCast(i, wpn int) {
	t := &w.Tanks[i]
	switch wpn {
	case wepPounce: // tiger: dash forward; a kill during the window refunds it
		// TODO(feel): damage along the dash path for a truer Swift-Strike
		f := fwd(t.HullYaw)
		t.lungeVX, t.lungeVZ = f.X*lungeSpeed, f.Z*lungeSpeed
		t.pounceT = pounceWindow
	case wepInk: // octopod: the ink screen also hides the octopod (escape)
		t.cloakT = inkCloakDur
	}
}

// BodySizeScale enlarges certain characters beyond the normalized size - both the
// rendered model AND the hit footprint - so e.g. the T-Rex towers and is a
// correspondingly bigger target (fair: big and powerful, but easy to hit).
func BodySizeScale(body int) float64 {
	switch body {
	case BodyTrex:
		return 1.3 // towering, but 1.75 read too large; ~75% of that
	case BodyQuad:
		return 1.9 // the tiger read far too small; nearly double so it has presence
	case BodyInsect:
		return 1.5 // bumped up from the tiny base size, then trimmed to 75%
	case BodyCrab:
		return 1.5 // wide armored shell should LOOK the part
	case BodyElephant:
		return 1.35 // it should tower like the trex does
	case BodyMinotaur:
		return 1.2 // a big bruiser, broader than a humanoid
	}
	return 1
}

// areaHit applies an area strike's payload to one eligible target. Knockback
// weapons shove radially out from the strike (and still deal their damage);
// everything else rides applyShotHit, so cones and melee strikes can carry any
// effect in the palette (fire breath, shield spray, heal pulse). dx/dz/dist is
// the target's offset from the firer.
func (w *World) areaHit(s *Projectile, ti int, dx, dz, dist float64) {
	if w.absorbedByShield(s, ti) {
		return
	}
	if s.eff == EffKnockback {
		t := &w.Tanks[ti]
		if s.mag > 0 && dist > 0.01 {
			t.Pos.X += dx / dist * s.mag
			t.Pos.Z += dz / dist * s.mag
			clampArena(&t.Pos, w.half())
			w.collide(&t.Pos)
		}
		t.hitFlash = tankHitFlash
		if s.dmg > 0 {
			w.hurt(ti, s.dmg, s.owner, s.cause)
		}
		return
	}
	w.applyShotHit(s, ti)
}

// coneStrike applies a weapon's payload to every eligible tank within a forward
// cone (the T-Rex's fire breath, the elephant's shield spray) and spawns a
// forward plume. dir is the 3D aim direction.
func (w *World) coneStrike(s *Projectile, dir V3) {
	o := &w.Tanks[s.owner]
	rng := s.blast
	if rng <= 0 {
		rng = 9
	}
	n := math.Hypot(dir.X, dir.Z)
	if n < 1e-6 {
		return
	}
	fx, fz := dir.X/n, dir.Z/n // horizontal facing
	const cosHalf = 0.6        // ~53deg half-angle cone
	for ti := range w.Tanks {
		if w.Tanks[ti].cloakT > 0 {
			continue // can't aim a cone at what you can't see
		}
		if !w.shotCanAffect(s, ti) {
			continue
		}
		t := &w.Tanks[ti]
		dx, dz := t.Pos.X-o.Pos.X, t.Pos.Z-o.Pos.Z
		d2 := dx*dx + dz*dz
		if d2 > rng*rng {
			continue
		}
		dist := math.Sqrt(d2)
		if dist < 0.01 {
			dist = 0.01
		}
		if (dx*fx+dz*fz)/dist < cosHalf { // outside the forward cone
			continue
		}
		w.areaHit(s, ti, dx, dz, dist)
	}
	if s.eff == EffShield { // the sprayer coats itself too (still worth firing in FFA)
		if self := s.dur * 0.6; o.shieldT < self {
			o.shieldT = self
		}
	}
	at := V3{o.Pos.X, o.Pos.Y + 1.4*BodySizeScale(o.body), o.Pos.Z}
	if s.affects == TargetAllies {
		w.spawnSprayFX(at, fx, fz) // support plume: sparks, not fire
	} else {
		w.spawnFlameFX(at, fx, fz)
	}
}

// spawnFlameFX puffs a forward plume of fire particles.
func (w *World) spawnFlameFX(from V3, fx, fz float64) {
	base := math.Atan2(fx, fz)
	for i := 0; i < 12; i++ {
		a := base + (rand.Float64()*2-1)*0.5
		sp := 7 + rand.Float64()*7
		w.spawnQ = append(w.spawnQ, Projectile{
			Pos:   from,
			vel:   V3{X: math.Sin(a) * sp, Y: 0.4 + rand.Float64()*1.6, Z: math.Cos(a) * sp},
			life:  0.25 + rand.Float64()*0.25,
			owner: -1,
			fx:    true,
			vis:   VisFlame,
		})
	}
}

// spawnSprayFX puffs a forward plume of spark particles (a support cone's
// mist - the elephant's shield spray - so it doesn't read as fire).
func (w *World) spawnSprayFX(from V3, fx, fz float64) {
	base := math.Atan2(fx, fz)
	for i := 0; i < 10; i++ {
		a := base + (rand.Float64()*2-1)*0.55
		sp := 6 + rand.Float64()*6
		w.spawnQ = append(w.spawnQ, Projectile{
			Pos:   from,
			vel:   V3{X: math.Sin(a) * sp, Y: 1 + rand.Float64()*2, Z: math.Cos(a) * sp},
			life:  0.25 + rand.Float64()*0.2,
			owner: -1,
			fx:    true,
			vis:   VisSpark,
		})
	}
}

// meleeStrike applies a weapon's payload to every eligible tank within its Blast
// radius of the firer at once (the gorilla's pound shoves and hurts, the stag's
// aura heals the pack), with a ground-burst FX.
func (w *World) meleeStrike(s *Projectile) {
	o := &w.Tanks[s.owner]
	rng := s.blast
	if rng <= 0 {
		rng = 4
	}
	for ti := range w.Tanks {
		if w.Tanks[ti].cloakT > 0 {
			continue
		}
		if !w.shotCanAffect(s, ti) {
			continue
		}
		t := &w.Tanks[ti]
		dx, dz := t.Pos.X-o.Pos.X, t.Pos.Z-o.Pos.Z
		d2 := dx*dx + dz*dz
		if d2 > rng*rng {
			continue
		}
		w.areaHit(s, ti, dx, dz, math.Sqrt(d2))
	}
	w.spawnBlastFX(V3{o.Pos.X, o.Pos.Y + 0.3, o.Pos.Z})
}

// leapLand resolves a leaping bruiser's touchdown: anyone hostile right next
// to the impact takes a small hit and a shove. Only a real leap slams (a
// standing hop with the lunge spent does nothing).
func (w *World) leapLand(i int) {
	t := &w.Tanks[i]
	if math.Hypot(t.lungeVX, t.lungeVZ) < lungeSpeed*0.25 {
		return
	}
	const rad, dmg, shove = 2.8, 12, 1.2
	for j := range w.Tanks {
		v := &w.Tanks[j]
		if j == i || v.Dead || v.gone || v.guard > 0 || v.shieldT > 0 || v.shellT > 0 {
			continue
		}
		if w.rules().Teams == 2 && v.Team == t.Team {
			continue
		}
		dx, dz := v.Pos.X-t.Pos.X, v.Pos.Z-t.Pos.Z
		dd := math.Hypot(dx, dz)
		if dd > rad {
			continue
		}
		if dd > 0.01 {
			v.Pos.X += dx / dd * shove
			v.Pos.Z += dz / dd * shove
			clampArena(&v.Pos, w.half())
			w.collide(&v.Pos)
		}
		w.hurt(j, dmg, i, CauseCannon)
	}
}

// stepLunge advances and damps a tank's transient forward-leap velocity.
func (w *World) stepLunge(t *Tank, dt float64) {
	if t.lungeVX == 0 && t.lungeVZ == 0 {
		return
	}
	t.Pos.X += t.lungeVX * dt
	t.Pos.Z += t.lungeVZ * dt
	clampArena(&t.Pos, w.half())
	w.collide(&t.Pos)
	if damp := 1 - 3.0*dt; damp > 0 {
		t.lungeVX *= damp
		t.lungeVZ *= damp
	} else {
		t.lungeVX, t.lungeVZ = 0, 0
	}
	if math.Abs(t.lungeVX) < 0.05 && math.Abs(t.lungeVZ) < 0.05 {
		t.lungeVX, t.lungeVZ = 0, 0
	}
}

// fireWeapon spawns a projectile carrying the given weapon's payload and sets the
// firer's cooldown (primary or, when secondary, the B-key cooldown). Driving
// everything off the WeaponDef means new weapons need no new code here - only a
// palette entry (and effect handling in applyShotHit).
func (w *World) fireWeapon(owner int, def *WeaponDef, secondary bool) {
	t := &w.Tanks[owner]
	cost := def.Cost
	if cost <= 0 {
		cost = 1
	}
	if t.ammo < cost { // out of the regen pool: throttled until it recharges
		return
	}
	t.ammo -= cost
	t.shotsFired++
	yaw, pitch := t.HullYaw+t.TurretYaw, t.TurretPitch
	if t.aiWobble > 0 { // bots miss by their tier+jitter; humans have aiWobble 0
		yaw += (rand.Float64()*2 - 1) * t.aiWobble
		pitch += (rand.Float64()*2 - 1) * t.aiWobble
	}
	d := aimDir(yaw, pitch)
	speed, life := def.Speed, def.Life
	if speed == 0 {
		speed = projSpeed
	}
	if life == 0 {
		life = projLife
	}
	p := Projectile{
		owner: owner,
		dmg:   def.Damage,
		eff:   def.Effect.Kind, mag: def.Effect.Mag, dur: def.Effect.Dur,
		blast:   def.Blast,
		affects: def.Affects,
		vis:     visForDelivery(def.Delivery),
		cause:   def.Cause,
	}
	// Spawn from the body's muzzle origin (rotated by facing), so a scorpion fires
	// from its tail, a humanoid from its hand, a tank from its barrel.
	mz := bodyDefFor(t.body).muzzle
	sy, cy := math.Sin(yaw), math.Cos(yaw)
	muzzle := V3{t.Pos.X + mz.X*cy + mz.Z*sy, t.Pos.Y + mz.Y, t.Pos.Z - mz.X*sy + mz.Z*cy}
	switch def.Delivery {
	case DeliverMine: // dropped at the firer's feet; arms, then triggers on a foe
		p.Pos = V3{X: t.Pos.X, Y: t.Pos.Y + 0.2, Z: t.Pos.Z}
		p.mine, p.armT, p.life = true, mineArm, mineLife
		w.Shots = append(w.Shots, p)
	case DeliverLob: // arced throw (gravity + loft); detonates on ground/contact
		p.Pos, p.vel, p.life = muzzle, V3{d.X * speed, d.Y*speed + lobLoft, d.Z * speed}, life
		if p.grav = def.Arc; p.grav == 0 {
			p.grav = lobGravity
		}
		w.Shots = append(w.Shots, p)
	case DeliverBeam: // hitscan: resolve instantly along the ray, draw a beam
		w.fireBeam(&p, muzzle, d, def)
	case DeliverMelee: // instant radial strike; no projectile travels
		w.meleeStrike(&p)
	case DeliverCone: // forward cone (fire breath, shield spray); no projectile travels
		w.coneStrike(&p, d)
	default: // DeliverBolt
		p.Pos, p.vel, p.life = muzzle, V3{d.X * speed, d.Y * speed, d.Z * speed}, life
		w.Shots = append(w.Shots, p)
	}
	delay := def.Cooldown
	if delay == 0 {
		delay = t.veh().FireDelay
	}
	if t.rapidT > 0 {
		delay *= rapidFireMul
	}
	if t.aiFireMul > 0 { // bot reload cadence (humans aiFireMul 0 -> unchanged)
		delay *= t.aiFireMul
	}
	if secondary {
		t.cooldown2 = delay
	} else {
		t.cooldown = delay
	}
}

// fireBeam resolves a hitscan weapon instantly along the aim ray - the first foe,
// destructible/solid entity, or obstacle stops it - applies the payload at that
// point, and draws a brief beam of debris segments.
func (w *World) fireBeam(p *Projectile, muzzle, d V3, def *WeaponDef) {
	rng := def.Life
	if rng == 0 {
		rng = 40
	}
	stopT := rng
	for t := 0.5; t <= rng; t += 0.5 {
		pt := V3{X: muzzle.X + d.X*t, Y: muzzle.Y + d.Y*t, Z: muzzle.Z + d.Z*t}
		if math.Abs(pt.X) > w.half() || math.Abs(pt.Z) > w.half() || w.hitObstacle(pt) {
			stopT = t
			break
		}
		found := -1
		for ti := range w.Tanks {
			if !w.shotCanAffect(p, ti) {
				continue
			}
			tk := &w.Tanks[ti]
			sz := BodySizeScale(tk.body)
			dx, dz := tk.Pos.X-pt.X, tk.Pos.Z-pt.Z
			dyLow, dyHigh := tk.Pos.Y-0.3, tk.Pos.Y+tankBodyTop*VehBody(tk.body).Scale*sz
			r := hitRadius * sz
			if dx*dx+dz*dz < r*r && pt.Y >= dyLow && pt.Y <= dyHigh {
				found = ti
				break
			}
		}
		if found >= 0 {
			stopT = t
			w.shotImpact(p, pt, found)
			break
		}
		p.Pos = pt
		if w.shotHitsEntity(p) { // solid/destructible map entity blocks/takes the beam
			stopT = t
			if p.blast > 0 {
				w.detonate(p, pt)
			}
			break
		}
	}
	w.spawnBeamFX(muzzle, V3{X: muzzle.X + d.X*stopT, Y: muzzle.Y + d.Y*stopT, Z: muzzle.Z + d.Z*stopT}, p.owner)
}

// spawnBeamFX lays a short-lived line of segments from a to b (the beam visual).
func (w *World) spawnBeamFX(a, b V3, owner int) {
	dx, dy, dz := b.X-a.X, b.Y-a.Y, b.Z-a.Z
	n := int(math.Hypot(dx, dz) / 0.6)
	if n < 1 {
		n = 1
	}
	if n > 50 {
		n = 50
	}
	for i := 0; i <= n; i++ {
		f := float64(i) / float64(n)
		w.Shots = append(w.Shots, Projectile{
			Pos: V3{X: a.X + dx*f, Y: a.Y + dy*f, Z: a.Z + dz*f}, life: 0.12, owner: owner, fx: true, vis: VisBeam,
		})
	}
}

// Update advances the world by dt. inputs maps a human tank index to its held
// buttons this tick (absent => idle); bots are driven by AI. The match
// lifecycle (countdown -> active -> ended -> next countdown) gates simulation.
func (w *World) Update(dt float64, inputs map[int]Input) {
	w.kills = w.kills[:0]   // kill feed is per-tick; Match() reads this tick's kills
	w.events = w.events[:0] // author toasts are per-tick too
	w.Timer -= dt
	switch w.Phase {
	case PhaseCountdown:
		if w.Timer <= 0 {
			w.startMatch()
		}
		return // world frozen during the count-in
	case PhaseEnded:
		if w.Timer <= 0 {
			w.afterEnded()
		}
		return // frozen scoreboard
	case PhaseLobby:
		w.applyVotes(inputs)
		if w.lobbyAllReady() && w.Timer > lobbyFastFwd {
			w.Timer = lobbyFastFwd // everyone locked in: skip the rest of the wait
		}
		if w.Timer <= 0 {
			mapIdx, mode := w.pickNextPairing()
			w.startCountdownMap(mode, mapIdx)
		}
		return // frozen lobby
	}
	w.simulate(dt, inputs)
	w.checkEnd()
}

// afterEnded transitions out of the scoreboard: server arenas open the vote
// lobby; offline/solo just counts in the same mode again.
func (w *World) afterEnded() {
	if w.Lobby {
		for i := range w.Tanks {
			w.Tanks[i].vote = -1
			w.Tanks[i].ready = false
		}
		w.Phase, w.Timer = PhaseLobby, lobbyTime
		return
	}
	w.startCountdown(w.Mode)
}

// SkipCountdown jumps a counting-in world straight to active play. The world
// is frozen during the count-in, so this is state-identical to waiting it out.
// Used by attract/demo displays, which should open on action, not a frozen grid.
func (w *World) SkipCountdown() {
	if w.Phase == PhaseCountdown {
		w.startMatch()
	}
}

func (w *World) startCountdown(mode Mode) {
	if len(Maps) > 1 && !w.pinned {
		// Rotate within the maps suited to this mode and session size (CTF
		// cycles the CTF maps, KotH the hill maps, DM the generic arenas).
		if pool := modePool(mode, w.activeCount()); len(pool) > 0 {
			next := pool[0]
			for _, i := range pool {
				if i > w.MapIdx {
					next = i
					break
				}
			}
			w.MapIdx = next
		}
	}
	w.Mode, w.Phase, w.Timer, w.WinnerID = mode, PhaseCountdown, countdownTime, -1
}

// startCountdownMap begins the count-in on a specific map+mode (the lobby's voted
// pairing), bypassing the rotation in startCountdown.
func (w *World) startCountdownMap(mode Mode, mapIdx int) {
	if mapIdx >= 0 && mapIdx < len(Maps) {
		w.MapIdx = mapIdx
	}
	w.Mode, w.Phase, w.Timer, w.WinnerID = mode, PhaseCountdown, countdownTime, -1
}

// NaturalMode is the mode a map is built for: CTF if it has team flags, King of
// the Hill if it has a zone, Flag Run for neutral flags, else Deathmatch. Lobby
// pairings vote on a map; this picks the mode that map implies.
func NaturalMode(m Map) Mode {
	hasZone, teamFlag, neutralFlag := false, false, false
	for _, e := range m.Entities {
		if e.Zone != nil {
			hasZone = true
		}
		if e.Flag != nil {
			if e.Flag.Team >= 0 {
				teamFlag = true
			} else {
				neutralFlag = true
			}
		}
	}
	switch {
	case teamFlag:
		return ModeCTF
	case hasZone:
		return ModeFFAKotH
	case neutralFlag:
		return ModeFlagRun
	default:
		return ModeDeathmatch
	}
}

// EffectiveMode is the mode a map plays in: its explicit Rules.Mode if set, else
// the mode implied by its objectives (NaturalMode).
func EffectiveMode(m Map) Mode {
	if m.Rules != nil && m.Rules.Mode >= 0 && m.Rules.Mode < len(Rulesets) {
		return Mode(m.Rules.Mode)
	}
	return NaturalMode(m)
}

// pickNextPairing chooses the next (map, mode) from per-map votes: the most-voted
// map wins (mode = its NaturalMode); with no votes it advances the rotation so an
// idle arena still cycles. A lone human's vote therefore decides the pairing.
// Scripted reports whether a map is event-scripted (boss/escort/...) - it has a
// director, paths, or entity behaviors. Such maps are reached deliberately (the
// map picker or an explicit vote), not via random rotation where they'd surprise
// a normal match.
func (m Map) Scripted() bool {
	if len(m.Logic) > 0 || len(m.Paths) > 0 || len(m.Actors) > 0 {
		return true
	}
	for i := range m.Entities {
		if len(m.Entities[i].Behaviors) > 0 || len(m.Entities[i].Watch) > 0 {
			return true
		}
	}
	return false
}

// fitsPlayers reports whether map mi accommodates n combatants (uncapped maps
// always do).
func fitsPlayers(mi, n int) bool {
	c := MapCapacity(Maps[mi])
	return c == 0 || n <= c
}

// activeCount is the number of present combatants (humans + bots).
func (w *World) activeCount() int {
	n := 0
	for i := range w.Tanks {
		if !w.Tanks[i].gone {
			n++
		}
	}
	return n
}

// hasObjectives reports whether a map authors flags or zones (i.e. was built
// for an objective mode rather than as a generic arena).
func hasObjectives(m Map) bool {
	for i := range m.Entities {
		if m.Entities[i].Flag != nil || m.Entities[i].Zone != nil {
			return true
		}
	}
	return false
}

// modePool returns the map indexes suited to playing `mode` with n combatants:
// the maps authored FOR that mode when any exist (CTF rotates the CTF maps,
// KotH the hill maps), else the generic arenas (no authored objectives, no
// pinned mode), which play any mode via the procedural fallbacks.
func modePool(mode Mode, n int) []int {
	var authored, generic []int
	for i := range Maps {
		m := &Maps[i]
		if m.Scripted() || !fitsPlayers(i, n) {
			continue
		}
		switch {
		case hasObjectives(*m) || (m.Rules != nil && m.Rules.Mode >= 0):
			if EffectiveMode(*m) == mode {
				authored = append(authored, i)
			}
		default:
			generic = append(generic, i)
		}
	}
	if len(authored) > 0 {
		return authored
	}
	return generic
}

// randomMapIdx returns a random map suited to the mode and player count,
// loosening to any non-scripted map (then any map) if nothing fits.
func randomMapIdx(mode Mode, n int) int {
	if pool := modePool(mode, n); len(pool) > 0 {
		return pool[rand.Intn(len(pool))]
	}
	var loose []int
	for i := range Maps {
		if !Maps[i].Scripted() {
			loose = append(loose, i)
		}
	}
	if len(loose) == 0 {
		return rand.Intn(len(Maps))
	}
	return loose[rand.Intn(len(loose))]
}

// nextRotation advances deterministically from `from` to the next non-scripted
// map that fits n combatants (ignoring the cap if nothing fits).
func nextRotation(from, n int) int {
	for off := 1; off <= len(Maps); off++ {
		if i := (from + off) % len(Maps); !Maps[i].Scripted() && fitsPlayers(i, n) {
			return i
		}
	}
	for off := 1; off <= len(Maps); off++ {
		if i := (from + off) % len(Maps); !Maps[i].Scripted() {
			return i
		}
	}
	return (from + 1) % len(Maps)
}

func (w *World) pickNextPairing() (mapIdx int, mode Mode) {
	if len(Maps) == 0 {
		return 0, w.Mode
	}
	active := w.activeCount()
	best, bestN := -1, 0
	for mi := 0; mi < len(Maps); mi++ {
		if !fitsPlayers(mi, active) {
			continue // sized below the current arena population: not votable
		}
		n := 0
		for i := range w.Tanks {
			t := &w.Tanks[i]
			if !t.Bot && !t.gone && t.vote == mi {
				n++
			}
		}
		if n > bestN {
			bestN, best = n, mi
		}
	}
	if best < 0 {
		best = nextRotation(w.MapIdx, active) // no votes: rotate to the next fitting map
	}
	return best, EffectiveMode(Maps[best])
}

func (w *World) applyVotes(inputs map[int]Input) {
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.Bot || t.gone {
			continue
		}
		in, ok := inputs[i]
		if !ok {
			continue
		}
		if in.Vote != t.vote { // a committed vote changed: announce it (server -> chat)
			t.vote = in.Vote
			if t.vote >= 0 && t.vote < len(Maps) {
				w.voteLog = append(w.voteLog, VoteEvent{Who: t.Name, MapIdx: t.vote})
			}
		}
		t.ready = in.Ready
	}
}

// lobbyAllReady reports whether every present human has locked a valid vote, so
// the lobby can fast-forward instead of waiting out the full timer.
func (w *World) lobbyAllReady() bool {
	humans := 0
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.Bot || t.gone {
			continue
		}
		humans++
		if !t.ready || t.vote < 0 {
			return false
		}
	}
	return humans > 0
}

// lobbyReadyCount is how many present humans have locked (for the UI).
func (w *World) lobbyReadyCount() int {
	n := 0
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if !t.Bot && !t.gone && t.ready && t.vote >= 0 {
			n++
		}
	}
	return n
}

// HumanCount returns the number of connected (non-bot, present) players.
func (w *World) HumanCount() int {
	n := 0
	for i := range w.Tanks {
		if !w.Tanks[i].Bot && !w.Tanks[i].gone {
			n++
		}
	}
	return n
}

// ForceLobby drops a running bot-only arena into a short vote lobby so a freshly
// arrived first human can pick the next map+mode instead of waiting out the bots.
// No-op unless the lobby is enabled and a match is currently active.
func (w *World) ForceLobby() {
	if !w.Lobby || w.Phase != PhaseActive {
		return
	}
	for i := range w.Tanks {
		w.Tanks[i].vote = -1
		w.Tanks[i].ready = false
	}
	w.Phase, w.Timer = PhaseLobby, lobbyTime
}

// startMatch begins active play: full reset of scores, health, and positions.
func (w *World) startMatch() {
	r := w.rules()
	timer := r.TimeLimit
	if timer <= 0 {
		timer = matchTime // endless modes still carry a clock for HUD/pacing
	}
	w.Phase, w.Timer, w.WinnerID, w.Shots = PhaseActive, timer, -1, nil
	w.teamScore, w.winnerTeam = [2]int{}, -1
	if r.Teams == 2 {
		w.assignTeams() // teams first, so spawnPoint can use the team bases
	}
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone {
			continue
		}
		if t.Bot && r.Bots != BotWaves { // re-pick a character each match (not Survival waves)
			w.rollBotLook(i)
		}
		t.Kills, t.Deaths, t.Carrying = 0, 0, -1
		t.HP, t.Dead, t.vy = t.veh().MaxHP, false, 0
		t.ammo = t.veh().AmmoMax
		t.shieldT, t.rapidT, t.cloakT = 0, 0, 0
		t.shellT, t.boostT, t.dotT, t.dotDebt, t.regenDebt, t.regenPause = 0, 0, 0, 0, 0, 0
		t.shieldUp, t.shieldBroken = false, 0
		t.shieldHP, t.bufferHP, t.hookT = 0, 0, 0
		if t.body == BodyMinotaur {
			t.shieldHP = minoShieldMax // a minotaur spawns with a full barrier
		}
		if t.body == BodyElephant {
			t.bufferHP = elephantBufferMax // and an elephant with a full shield buffer
		}
		t.guard = spawnGuardTime
		t.holdScore = 0
		t.Pos = w.spawnPoint(i)
		t.HullYaw = w.faceTarget(i)
		t.TurretYaw, t.TurretPitch = 0, 0
		if r.Lives > 0 && !(r.Bots == BotWaves && t.Bot) {
			t.lives = r.Lives // wave bots are exempt (infinite); everyone else gets lives
			if !t.Bot && w.campLives > 0 {
				t.lives = w.campLives // campaign: the run's life pool, not the map's
			}
		}
		if t.Bot {
			w.rollBotAI(i) // re-roll per-bot variation each match
		}
	}
	if r.Teams == 2 && r.Bots != BotWaves {
		w.assignHealers() // one butterfly medic per team
	}
	w.pickups, w.pickupTimer = nil, pickupInterval
	w.resetEntities()
	w.flags, w.zones = nil, nil
	switch r.Objective {
	case ObjNeutralFlags:
		w.setupNeutralFlags()
	case ObjTeamFlags:
		w.setupTeamFlags()
	case ObjZone:
		w.setupZones()
	}
	if r.Bots == BotWaves {
		w.setupSurvival()
	}
}

// ctfBase returns the home/base position for a CTF team (opposite Z ends).
func (w *World) ctfBase(team int) V3 {
	z := -(w.half() - 3)
	if team == 1 {
		z = w.half() - 3
	}
	return V3{0, 0, z}
}

// assignTeams splits active tanks into two CTF teams (humans together on team 0,
// bots balanced) and recolors each tank with a team-tinted shade.
// SetPlayerParty records a joining player's party (a team-grouping hint sent in
// HELLO); same-party humans are kept on the same side by assignTeams.
func (w *World) SetPlayerParty(i int, party string) {
	if i >= 0 && i < len(w.Tanks) {
		w.Tanks[i].Party = party
	}
}

func (w *World) assignTeams() {
	var count [2]int
	// Group the humans: each party is a group, and ALL unpartied callers share one
	// "solo" group (key "") so strangers play together, not carved apart. Splitting
	// only happens once there are two or more groups - i.e. once someone forms a
	// party to opt into a side. With no parties at all, every human lands on team 0
	// (co-op vs bots), the friendly default. One game per server means at most two
	// parties form, so the groups fall naturally onto the two teams.
	groups := map[string][]int{}
	var order []string // group keys in first-seen order (stable assignment)
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || t.Bot {
			continue
		}
		key := t.Party // "" = the shared solo pool
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	if len(order) <= 1 {
		// No party formed (everyone solo, or a single party with no one else):
		// keep all humans together on team 0, vs bots. No human-vs-human split.
		for _, key := range order {
			for _, i := range groups[key] {
				w.Tanks[i].Team = 0
				count[0]++
			}
		}
	} else {
		// A party exists: hand the groups out to the smaller side, largest first,
		// so each group (party or the solo pool) stays whole on one team.
		sort.SliceStable(order, func(a, b int) bool { return len(groups[order[a]]) > len(groups[order[b]]) })
		for _, key := range order {
			team := 1
			if count[0] <= count[1] {
				team = 0
			}
			for _, i := range groups[key] {
				w.Tanks[i].Team = team
				count[team]++
			}
		}
	}
	// Bots fill whichever team is smaller, keeping sides even.
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || !t.Bot {
			continue
		}
		team := 1
		if count[0] <= count[1] {
			team = 0
		}
		t.Team = team
		count[team]++
	}
	// Recolor by team with a per-slot brightness wobble so individuals differ.
	var shade [2]int
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || t.Team < 0 {
			continue
		}
		t.Color = teamColor(t.Team, shade[t.Team])
		shade[t.Team]++
	}
}

// teamColor returns a red (team 0) or blue (team 1) shade varied by slot.
func teamColor(team, k int) [3]float64 {
	d := float64(k%3) * 0.12
	if team == 0 {
		return [3]float64{0.85 - d*0.3, 0.30 - d*0.1, 0.28 - d*0.1}
	}
	return [3]float64{0.28 - d*0.1, 0.45 - d*0.1, 0.88 - d*0.3}
}

// setupNeutralFlags places the Flag Run flags: at authored `flag` entities
// (Team < 0) if the map defines any, else scattered procedurally as before.
func (w *World) setupNeutralFlags() {
	for _, e := range w.ActiveMap().Entities {
		if e.Flag != nil && e.Flag.Team < 0 {
			w.flags = append(w.flags, Flag{Pos: V3{e.Pos.X, 0, e.Pos.Z}, Team: -1, Carrier: -1})
		}
	}
	if len(w.flags) > 0 {
		return // authored placement wins
	}
	for i := 0; i < flagCount; i++ {
		x := (rand.Float64()*2 - 1) * (w.half() - 2)
		z := (rand.Float64()*2 - 1) * (w.half() - 2)
		w.flags = append(w.flags, Flag{Pos: V3{x, 0, z}, Team: -1, Carrier: -1})
	}
}

// setupTeamFlags places the CTF flags: one homed at each authored team `flag`
// entity if the map defines both teams', else procedurally at the team bases.
func (w *World) setupTeamFlags() {
	var home [2]V3
	var have [2]bool
	for _, e := range w.ActiveMap().Entities {
		if e.Flag != nil && (e.Flag.Team == 0 || e.Flag.Team == 1) {
			home[e.Flag.Team], have[e.Flag.Team] = V3{e.Pos.X, 0, e.Pos.Z}, true
		}
	}
	for team := 0; team < 2; team++ {
		h := w.ctfBase(team)
		if have[team] {
			h = home[team]
		}
		w.flags = append(w.flags, Flag{Pos: h, Home: h, Team: team, Carrier: -1, atHome: true})
	}
}

// setupZones places King-of-the-Hill control zones: at authored `zone` entities
// if the map defines any, else a single default hill at the arena center (so the
// mode is playable on any map).
func (w *World) setupZones() {
	for _, e := range w.ActiveMap().Entities {
		if e.Zone == nil {
			continue
		}
		cap := e.Zone.Capture
		if cap <= 0 {
			cap = zoneCaptureTime
		}
		// Anchor Y to the actual surface under the hill (top of any platform/dais
		// it sits on), so the markers render up on the platform and capture reads
		// the real terrain - not the authored guess.
		pos := V3{e.Pos.X, w.ground(e.Pos.X, e.Pos.Z, 1e4), e.Pos.Z}
		w.zones = append(w.zones, Zone{Pos: pos, Half: e.Half, Cap: cap, Owner: -1, cont: -1})
	}
	if len(w.zones) == 0 {
		// No authored hill: drop one on open, reachable ground near center. Maps
		// not built for KotH can have an unclimbable central structure (a tall
		// dais, a no-ramp platform); a hill stuck on top of that would freeze the
		// match, so seek clear floor outward from the middle instead.
		spot := w.openHillSpot()
		w.zones = append(w.zones, Zone{Pos: spot, Half: V3{X: zoneFallbackR, Y: 1, Z: zoneFallbackR}, Cap: zoneCaptureTime, Owner: -1, cont: -1})
	}
}

// openHillSpot finds a clear ground location (no obstacle taller than a step) for
// the auto-placed default hill, spiralling outward from center so the fallback
// hill is always reachable on the flat.
func (w *World) openHillSpot() V3 {
	lim := w.half() - zoneFallbackR - 1
	for _, r := range []float64{0, 6, 10, 14, 18} {
		if r > lim {
			break
		}
		for k := 0; k < 8; k++ {
			ang := float64(k) * math.Pi / 4
			x, z := r*math.Cos(ang), r*math.Sin(ang)
			if w.ground(x, z, 1e4) <= stepUp { // floor-ish: standable without climbing
				return V3{X: x, Z: z}
			}
		}
	}
	return V3{} // nothing clear found: fall back to dead center
}

// stepZones advances each control zone: a single uncontested contender (a team
// in team mode, a lone tank in FFA) captures it over Cap seconds; the owner,
// present and uncontested, accrues a hold-point per second (into team score or
// the tank's holdScore). Contested or empty -> no progress, partial caps reset.
func (w *World) stepZones(dt float64) {
	teamMode := w.rules().Teams == 2
	for zi := range w.zones {
		z := &w.zones[zi]
		// Who is inside the footprint?
		contender, count := -1, 0
		var teamsIn [2]bool
		for ti := range w.Tanks {
			t := &w.Tanks[ti]
			if t.Dead || t.gone {
				continue
			}
			if !w.inZone(z, t.Pos) { // elevated hills require you to actually be up there
				continue
			}
			count++
			if teamMode {
				if t.Team == 0 || t.Team == 1 {
					teamsIn[t.Team] = true
				}
			} else {
				contender = ti // last one in; only meaningful when count == 1
			}
		}
		// Resolve the sole contender, or contested/empty.
		contested := false
		if teamMode {
			switch {
			case teamsIn[0] && teamsIn[1]:
				contested = true
			case teamsIn[0]:
				contender = 0
			case teamsIn[1]:
				contender = 1
			default:
				contender = -1
			}
		} else if count != 1 {
			contender = -1
			contested = count > 1
		}

		if contested || contender < 0 {
			// Partial capture decays rather than vanishing: with bots now
			// actively contesting the hill, a hard reset meant nobody could
			// ever finish a capture - progress survives brief interruptions
			// and the fights themselves decide who completes it.
			if z.Prog > 0 {
				if z.Prog -= dt * 0.5; z.Prog < 0 {
					z.Prog = 0
				}
			}
			if contested {
				z.cont = -1 // a new sole contender will resume, whoever it is
			}
			continue
		}
		if z.Owner == contender { // owner holds -> accrue points
			z.cont, z.Prog = -1, 0
			z.hold += dt
			for z.hold >= 1 {
				z.hold--
				if teamMode {
					w.teamScore[contender]++
				} else if contender < len(w.Tanks) {
					w.Tanks[contender].holdScore++
				}
			}
			continue
		}
		// Capturing from a different (or no) owner. A named rival restarts the
		// meter; resuming after a contested scuffle (cont reset to -1) keeps
		// whatever progress survived the decay.
		if z.cont != contender {
			if z.cont >= 0 {
				z.Prog = 0
			}
			z.cont = contender
		}
		z.Prog += dt
		if z.Prog >= z.Cap {
			z.Owner, z.Prog, z.hold = contender, 0, 0
			w.emit("captured", -1, contender) // subject = capturing team/tank
		}
	}
}

// enemyFlag returns the flag a tank on `team` is trying to steal (the other
// team's flag), or nil.
func (w *World) enemyFlag(team int) *Flag {
	for i := range w.flags {
		if w.flags[i].Team >= 0 && w.flags[i].Team != team {
			return &w.flags[i]
		}
	}
	return nil
}

// ownFlag returns the flag a tank on `team` must keep at base to score, or nil.
func (w *World) ownFlag(team int) *Flag {
	for i := range w.flags {
		if w.flags[i].Team == team {
			return &w.flags[i]
		}
	}
	return nil
}

// setupSurvival reserves a bot pool, gives humans lives, and starts wave 1.
func (w *World) setupSurvival() {
	bots := 0
	for i := range w.Tanks {
		if w.Tanks[i].Bot {
			bots++
		}
	}
	for bots < survivalPool {
		w.Tanks = append(w.Tanks, Tank{Bot: true, gone: true, Dead: true, vote: -1, HP: VehBody(BodyTank).MaxHP, ammo: VehBody(BodyTank).AmmoMax, Name: botName(len(w.Tanks)), Team: -1, Carrying: -1, weapon2: wepGrenade})
		bots++
	}
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.Bot {
			t.gone, t.Dead = true, true // pool starts empty; waves activate them
		} else if !t.gone {
			t.lives = survivalLives
		}
	}
	w.wave = 0
	w.spawnWave()
}

// survivalBodies is the creature rotation Survival draws its wave enemies from; the
// per-wave offset shifts the mix so successive waves look different.
var survivalBodies = []int{BodySpider, BodyInsect, BodyScorpion, BodyCrab, BodyQuad, BodySerpent, BodyOctopod, BodyMantis, BodyGorilla, BodyTrex}

// spawnWave activates the next, larger wave of bots (tougher vehicles later).
func (w *World) spawnWave() {
	w.wave++
	n := 2 + w.wave
	if n > survivalPool {
		n = survivalPool
	}
	// Survival escalation: enemies field their own per-character stats, scaled up
	// each wave (the chassis-tier ramp retired with the chassis system). Tunable -
	// this curve is a first pass and wants a balancesim/playtest tuning pass, since
	// the baseline shifted when enemies stopped borrowing a heavier chassis.
	hpScale := 1.0 + survivalHPPerWave*float64(w.wave-1)
	if hpScale > survivalHPMax {
		hpScale = survivalHPMax
	}
	act := 0
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if !t.Bot {
			continue
		}
		if act < n {
			t.gone, t.Dead = false, false
			t.body = survivalBodies[(w.wave-1+act)%len(survivalBodies)] // bestiary: a shifting mix per wave
			t.hpScale = hpScale
			t.HP = t.veh().MaxHP // per-character HP, scaled by the wave multiplier
			t.guard, t.cooldown, t.vy, t.TurretYaw = spawnGuardTime, 0, 0, 0
			t.Color = BotPalette[act%len(BotPalette)]
			t.Pos = w.spawnPoint(i)
			t.HullYaw = w.faceTarget(i)
			w.rollBotAI(i)
			act++
		} else {
			t.gone, t.Dead = true, true
		}
	}
}

func (w *World) activeBots() int {
	n := 0
	for i := range w.Tanks {
		if w.Tanks[i].Bot && !w.Tanks[i].gone && !w.Tanks[i].Dead {
			n++
		}
	}
	return n
}

// collectFlags lets live human tanks grab nearby untaken flags (Flag Run).
// In the attract demo the designated hero bot collects instead - one player
// stand-in running the level, exactly as a human would.
func (w *World) collectFlags() {
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if (t.Bot && ti != w.demoHero) || t.Dead || t.gone {
			continue
		}
		for fi := range w.flags {
			f := &w.flags[fi]
			if f.Taken {
				continue
			}
			dx, dz := f.Pos.X-t.Pos.X, f.Pos.Z-t.Pos.Z
			if dx*dx+dz*dz < flagPickupRad*flagPickupRad {
				f.Taken = true
			}
		}
	}
}

// stepCTF advances the team-flag state: carried flags follow their carrier,
// dropped flags tick toward an auto-return, friendly touches return a dropped
// flag, enemy touches pick it up, and a carrier at home base scores a capture.
func (w *World) stepCTF(dt float64) {
	// Move/return flags based on carrier state.
	for fi := range w.flags {
		f := &w.flags[fi]
		if f.Carrier >= 0 {
			c := &w.Tanks[f.Carrier]
			if c.Dead || c.gone { // safety: carrier vanished without a drop
				f.Carrier, f.atHome, f.dropTimer = -1, false, flagReturnTime
			} else {
				f.Pos = V3{c.Pos.X, c.Pos.Y, c.Pos.Z}
			}
		} else if !f.atHome {
			f.dropTimer -= dt
			if f.dropTimer <= 0 {
				f.Pos, f.atHome = f.Home, true
			}
		}
	}
	// Tank interactions with flags.
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Dead || t.gone || t.guard > 0 {
			continue
		}
		for fi := range w.flags {
			f := &w.flags[fi]
			if f.Carrier == ti {
				continue
			}
			dx, dz := f.Pos.X-t.Pos.X, f.Pos.Z-t.Pos.Z
			if dx*dx+dz*dz >= flagPickupRad*flagPickupRad {
				continue
			}
			if f.Team == t.Team {
				// Touching your own dropped flag returns it home instantly.
				if !f.atHome && f.Carrier < 0 {
					f.Pos, f.atHome, f.dropTimer = f.Home, true, 0
				}
			} else if f.Carrier < 0 && t.Carrying < 0 {
				// Grab the enemy flag (whether at home or dropped).
				f.Carrier, f.atHome = ti, false
				t.Carrying = fi
			}
		}
		// Capture: a carrier at their own base scores if their flag is home.
		if t.Carrying >= 0 {
			of := w.ownFlag(t.Team)
			if of != nil && of.atHome {
				dx, dz := of.Home.X-t.Pos.X, of.Home.Z-t.Pos.Z
				if dx*dx+dz*dz < ctfCaptureRad*ctfCaptureRad {
					cf := &w.flags[t.Carrying]
					cf.Carrier, cf.atHome, cf.Pos, cf.dropTimer = -1, true, cf.Home, 0
					t.Carrying = -1
					if t.Team == 0 || t.Team == 1 {
						w.teamScore[t.Team]++
					}
					t.Kills += ctfCaptureBonus
				}
			}
		}
	}
}

// Turtle shell mode: how long a shell-up lasts, and the secondary-cooldown
// recharge after popping out (manual or expiry) before the next shell-up.
const (
	shellDuration = 4.0
	shellRecharge = 6.0
	shellHealRate = 22.0 // HP/sec recovered while tucked into the shell
)

// regenHitPause is how long taking damage suppresses passive HP regen.
const regenHitPause = 3.0

// healFlashDur is how long the mend halo shows after an ally heal lands.
const healFlashDur = 0.6

// Minotaur barrier tuning.
const (
	minoShieldMax     = 220.0 // barrier HP pool
	minoShieldRegen   = 45.0  // HP/sec recovered while the barrier is lowered
	minoShieldBreakCD = 5.0   // sec before a shattered barrier can deploy again
	minoShieldArc     = 0.40  // cos of the half-angle it covers (~133 deg frontal)
	minoShieldMoveMul = 0.55  // movement speed while the barrier is up
)

// Elephant tuning: a regenerating damage buffer + the trunk hook.
const (
	elephantBufferMax   = 130.0 // passive shield buffer HP
	elephantBufferRegen = 28.0  // buffer HP/sec recovered when not recently hit
	hookRecharge        = 3.5   // sec between trunk-hook grabs
	hookPullDist        = 3.2   // how close a hooked foe is reeled in (into tusk reach)
)

// stepDot ticks tank i's damage-over-time slot (venom, burn): fractional HP
// accrues in dotDebt, whole points land through hurt (kill credit), and a
// leeching DoT (the T-Rex's burn) feeds the damage back to the shooter.
func (w *World) stepDot(i int, dt float64) {
	t := &w.Tanks[i]
	if t.dotT <= 0 {
		return
	}
	if t.Dead || t.gone || t.shieldT > 0 || t.shellT > 0 { // death/shield/shell purge it
		t.dotT, t.dotDebt = 0, 0
		return
	}
	t.dotT -= dt
	t.dotDebt += t.dotPS * dt
	if t.dotDebt < 1 {
		return
	}
	n := int(t.dotDebt)
	t.dotDebt -= float64(n)
	if t.dotLeech && t.dotFrom >= 0 && t.dotFrom < len(w.Tanks) {
		if o := &w.Tanks[t.dotFrom]; !o.Dead && !o.gone {
			if max := o.veh().MaxHP; o.HP < max {
				if o.HP += n; o.HP > max {
					o.HP = max
				}
			}
		}
	}
	// A DoT tick shouldn't strobe the white damage-flash several times a second
	// (it would drown the burn/poison tint); the colored tint is the feedback.
	preFlash := t.hitFlash
	w.hurt(i, n, t.dotFrom, t.dotCause)
	if !t.Dead {
		t.hitFlash = preFlash
	}
}

// stepRegen ticks a character's passive HP recharge (a bodyDef trait: fragile
// fast movers knit themselves back together; the armored bruisers don't).
// Taking damage pauses it (regenPause, set in hurt).
func (w *World) stepRegen(t *Tank, dt float64) {
	rate := bodyDefFor(t.body).hpRegen
	if rate <= 0 || t.Dead || t.gone || t.regenPause > 0 || t.dotT > 0 {
		return
	}
	max := t.veh().MaxHP
	if t.HP >= max {
		t.regenDebt = 0
		return
	}
	t.regenDebt += rate * dt
	if t.regenDebt < 1 {
		return
	}
	n := int(t.regenDebt)
	t.regenDebt -= float64(n)
	if t.HP += n; t.HP > max {
		t.HP = max
	}
}

// stepPickups ages buff timers, periodically spawns a new drop (up to the cap),
// and grants the effect to any live tank that drives over one. Mode-agnostic.
func (w *World) stepPickups(dt float64) {
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.shieldT > 0 {
			t.shieldT -= dt
		}
		if t.rapidT > 0 {
			t.rapidT -= dt
		}
		if t.cloakT > 0 {
			t.cloakT -= dt
		}
		if t.boostT > 0 {
			t.boostT -= dt
		}
		if t.regenPause > 0 {
			t.regenPause -= dt
		}
		if t.shellT > 0 {
			// Tucked in: heal fast (the shell is a recovery move, gated by the
			// pop-out recharge so it can't be spammed).
			if max := t.veh().MaxHP; t.HP < max {
				t.regenDebt += shellHealRate * dt
				if n := int(t.regenDebt); n > 0 {
					t.regenDebt -= float64(n)
					if t.HP += n; t.HP > max {
						t.HP = max
					}
				}
			}
			if t.shellT -= dt; t.shellT <= 0 { // shell time spent: pop out, recharge
				t.shellT, t.cooldown2 = 0, shellRecharge
			}
		}
		if t.body == BodyElephant && t.regenPause <= 0 && t.bufferHP < elephantBufferMax {
			if t.bufferHP += elephantBufferRegen * dt; t.bufferHP > elephantBufferMax {
				t.bufferHP = elephantBufferMax
			}
		}
		if t.body == BodyMinotaur { // barrier: count down a shatter, regen while lowered
			if t.shieldBroken > 0 {
				if t.shieldBroken -= dt; t.shieldBroken < 0 {
					t.shieldBroken = 0
				}
			} else if !t.shieldUp && t.shieldHP < minoShieldMax {
				if t.shieldHP += minoShieldRegen * dt; t.shieldHP > minoShieldMax {
					t.shieldHP = minoShieldMax
				}
			}
		}
		w.stepDot(i, dt)
		w.stepRegen(t, dt)
	}
	w.pickupTimer -= dt
	if w.pickupTimer <= 0 {
		w.pickupTimer = pickupInterval
		if len(w.pickups) < pickupMax {
			w.spawnPickup()
		}
	}
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Dead || t.gone {
			continue
		}
		for pi := 0; pi < len(w.pickups); {
			p := w.pickups[pi]
			dx, dz := p.Pos.X-t.Pos.X, p.Pos.Z-t.Pos.Z
			if dx*dx+dz*dz < pickupRadius*pickupRadius {
				w.applyPickup(t, p.Kind, p.Weapon)
				w.emit("picked", -1, ti) // subject = the tank that grabbed it
				w.pickups = append(w.pickups[:pi], w.pickups[pi+1:]...)
				continue // don't advance pi; the slice shifted
			}
			pi++
		}
	}
}

// stepEntities advances the world's trait-objects each tick: turrets track and
// shoot, hazards burn tanks standing in them, teleporters warp tanks that drive
// in, and destroyed entities with a Respawn trait tick back to life.
func (w *World) stepEntities(dt float64) {
	for i := range w.entities {
		e := &w.entities[i]
		if e.cooldown > 0 {
			e.cooldown -= dt
		}
		if e.Dead {
			if e.Respawn != nil && e.respawnT > 0 {
				e.respawnT -= dt
				if e.respawnT <= 0 {
					e.Dead = false
					if e.Destruct != nil {
						e.HP = e.Destruct.MaxHP
					}
				}
			}
			continue
		}
		if e.Turret != nil {
			w.stepTurret(e, dt)
		}
		if e.Hazard != nil {
			w.stepHazard(e, dt)
		}
		if e.Teleport != nil {
			w.stepTeleport(e)
		}
		if e.Bounce != nil {
			w.stepBounce(e)
		}
		if e.Trigger != nil {
			w.stepTrigger(e, i)
		}
		if e.mvOn {
			w.stepMove(e, i, dt)
		}
		// HP-threshold watchers: emit hp_below as the entity crosses each level.
		if len(e.Watch) > 0 && e.Destruct != nil {
			pct := float64(e.HP) / float64(e.Destruct.MaxHP) * 100
			for k, thr := range e.Watch {
				if !e.wHit[k] && pct <= thr {
					e.wHit[k] = true
					w.emit("hp_below", i, -1)
				}
			}
		}
	}
}

// stepHazard burns tanks standing within a hazard's footprint at its DPS. A
// per-tank fractional accumulator turns the float DPS*dt into whole-HP hits so
// integer HP still drains at the authored rate. Shield/spawn-guard protect you.
func (w *World) stepHazard(e *Entity, dt float64) {
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Dead || t.gone || t.guard > 0 || t.shieldT > 0 || t.shellT > 0 {
			continue
		}
		if math.Abs(t.Pos.X-e.Pos.X) >= e.Half.X || math.Abs(t.Pos.Z-e.Pos.Z) >= e.Half.Z {
			continue
		}
		if t.Pos.Y > e.Pos.Y+e.Half.Y+0.5 {
			continue // jumped clear / standing above it
		}
		t.hazardDebt += e.Hazard.DPS * dt
		if whole := int(t.hazardDebt); whole > 0 {
			t.hazardDebt -= float64(whole)
			w.hurt(ti, whole, -1, CauseHazard)
		}
	}
}

// stepTeleport warps the first live tank in a teleporter's footprint to Dest,
// arming the pad's Cooldown and a per-tank debounce so it can't immediately
// bounce back through the destination pad.
func (w *World) stepTeleport(e *Entity) {
	if e.cooldown > 0 {
		return
	}
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Dead || t.gone || t.teleT > 0 {
			continue
		}
		if math.Abs(t.Pos.X-e.Pos.X) >= e.Half.X || math.Abs(t.Pos.Z-e.Pos.Z) >= e.Half.Z {
			continue
		}
		d := e.Teleport.Dest
		t.Pos = V3{d.X, d.Y, d.Z}
		t.teleT = teleDebounce
		e.cooldown = e.Teleport.Cooldown
		break
	}
}

// stepBounce launches any tank resting on/near the pad straight up with the
// trait's fixed Power. The "near the surface and not already rising" gate both
// debounces (one launch per descent) and gives a trampoline its repeat bounce:
// you come back down, touch, and fire again. Both players and bots have vertical
// physics, so a trampoline launches either.
func (w *World) stepBounce(e *Entity) {
	padTop := e.Pos.Y + e.Half.Y
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Dead || t.gone {
			continue
		}
		if math.Abs(t.Pos.X-e.Pos.X) >= e.Half.X || math.Abs(t.Pos.Z-e.Pos.Z) >= e.Half.Z {
			continue
		}
		if t.Pos.Y <= padTop+0.25 && t.vy <= 0.1 {
			t.vy = e.Bounce.Power
		}
	}
}

// stepTrigger emits entered/exited (self-scoped) as tanks cross a sensor volume.
func (w *World) stepTrigger(e *Entity, i int) {
	if len(e.inside) < len(w.Tanks) {
		grown := make([]bool, len(w.Tanks))
		copy(grown, e.inside)
		e.inside = grown
	}
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		in := !t.Dead && !t.gone &&
			math.Abs(t.Pos.X-e.Pos.X) < e.Half.X && math.Abs(t.Pos.Z-e.Pos.Z) < e.Half.Z &&
			t.Pos.Y <= e.Pos.Y+e.Half.Y+0.5
		switch {
		case in && !e.inside[ti]:
			e.inside[ti] = true
			w.emit("entered", i, ti)
		case !in && e.inside[ti]:
			e.inside[ti] = false
			w.emit("exited", i, ti)
		}
	}
}

// stepMove advances a moving entity along its path; emits `arrived` (self-scoped)
// at the end. Position rides EntitySnap.Pos to clients.
func (w *World) stepMove(e *Entity, i int, dt float64) {
	pts := w.pathFor(e.mvPath)
	if len(pts) < 2 {
		e.mvOn = false
		return
	}
	e.mvDist += e.mvSpeed * dt
	pos, done := pathAt(pts, e.mvDist)
	e.Pos = pos
	if done {
		e.mvOn = false
		w.emit("arrived", i, -1)
	}
}

// pathFor returns a named path's waypoints from the active map (empty name = the
// first path, the common single-path case).
func (w *World) pathFor(name string) []V3 {
	ps := w.ActiveMap().Paths
	for i := range ps {
		if name == "" || strings.EqualFold(ps[i].Name, name) {
			return ps[i].Points
		}
	}
	return nil
}

// pathAt returns the point at cumulative distance d along a polyline, and whether
// d is at/past the end.
func pathAt(pts []V3, d float64) (V3, bool) {
	if d <= 0 {
		return pts[0], false
	}
	for i := 1; i < len(pts); i++ {
		seg := pts[i].Sub(pts[i-1])
		segLen := math.Sqrt(seg.X*seg.X + seg.Y*seg.Y + seg.Z*seg.Z)
		if d <= segLen || i == len(pts)-1 {
			if segLen <= 0 {
				return pts[i], i == len(pts)-1 && d >= 0
			}
			f := d / segLen
			if f >= 1 {
				return pts[i], i == len(pts)-1
			}
			return V3{pts[i-1].X + seg.X*f, pts[i-1].Y + seg.Y*f, pts[i-1].Z + seg.Z*f}, false
		}
		d -= segLen
	}
	return pts[len(pts)-1], true
}

// stepTurret aims a turret entity at the nearest eligible tank and fires when
// lined up and off cooldown. Cloaked tanks are invisible to it (like radar).
func (w *World) stepTurret(e *Entity, dt float64) {
	tr := e.Turret
	best, bestD2 := -1, tr.Range*tr.Range
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Dead || t.gone || t.guard > 0 || t.cloakT > 0 {
			continue
		}
		dx, dz := t.Pos.X-e.Pos.X, t.Pos.Z-e.Pos.Z
		if d2 := dx*dx + dz*dz; d2 < bestD2 {
			bestD2, best = d2, ti
		}
	}
	if best < 0 {
		return // nothing in range; hold current facing
	}
	t := &w.Tanks[best]
	want := math.Atan2(t.Pos.X-e.Pos.X, t.Pos.Z-e.Pos.Z)
	// Elevation toward the target's body center, from the muzzle height. Computed
	// (not clamped) so an elevated emplacement can depress onto ground tanks.
	muzzleY := e.Pos.Y + e.Half.Y + 0.3
	horiz := math.Sqrt(bestD2)
	wantPitch := math.Atan2((t.Pos.Y+turretAimHeight)-muzzleY, horiz)
	rate := tr.TurnRate
	if rate <= 0 {
		rate = turretRate
	}
	e.Yaw = turnToward(e.Yaw, want, rate*dt)
	e.Pitch = turnToward(e.Pitch, wantPitch, rate*dt)
	if e.cooldown <= 0 && math.Abs(angDiff(want, e.Yaw)) < botAimTol && math.Abs(wantPitch-e.Pitch) < botAimTol {
		w.fireEntity(e)
		delay := tr.FireDelay
		if delay <= 0 {
			delay = botFireDelay
		}
		e.cooldown = delay
	}
}

// fireEntity launches a projectile from a turret entity along its yaw+pitch
// (owner <0 = no kill credit; dmg from the trait, 0 -> default projDmg).
func (w *World) fireEntity(e *Entity) {
	def := &Weapons[wepCannon]
	if e.Weapon > 0 && e.Weapon < len(Weapons) {
		def = &Weapons[e.Weapon]
	}
	d := aimDir(e.Yaw, e.Pitch)
	muzzleY := e.Pos.Y + e.Half.Y + 0.3
	muzzle := V3{e.Pos.X + d.X*1.2, muzzleY + d.Y*1.2, e.Pos.Z + d.Z*1.2}
	// Damage: an explicit weapon brings its own number; the default cannon is only
	// a delivery template here (its own Damage - the tank-cannon nerf - is not a
	// turret's), so a default emplacement uses the trait's authored damage. Either
	// way the trait fills in when the weapon carries none, and 0 -> projDmg later.
	dmg := 0
	if e.Weapon > 0 && e.Weapon < len(Weapons) {
		dmg = def.Damage
	}
	if dmg == 0 && e.Turret != nil {
		dmg = e.Turret.Dmg
	}
	speed, life := def.Speed, def.Life
	if speed == 0 {
		speed = projSpeed
	}
	if life == 0 {
		life = projLife
	}
	p := Projectile{
		Pos: muzzle, life: life, owner: -1, dmg: dmg,
		eff: def.Effect.Kind, mag: def.Effect.Mag, dur: def.Effect.Dur,
		blast: def.Blast, affects: def.Affects, vis: visForDelivery(def.Delivery),
	}
	switch def.Delivery {
	case DeliverBeam: // a laser emplacement
		w.fireBeam(&p, muzzle, d, def)
	case DeliverLob: // an emplacement can lob (mines fall back to a bolt)
		p.vel = V3{d.X * speed, d.Y*speed + lobLoft, d.Z * speed}
		if p.grav = def.Arc; p.grav == 0 {
			p.grav = lobGravity
		}
		w.Shots = append(w.Shots, p)
	default:
		p.vel = V3{d.X * speed, d.Y * speed, d.Z * speed}
		w.Shots = append(w.Shots, p)
	}
}

// spawnPickup drops a random power-up at a free authored spawn spot (or a random
// open tile if the map defines none).
func (w *World) spawnPickup() {
	spot, ok := w.pickupSpot()
	if !ok {
		return
	}
	pos := spot.Pos
	pos.Y = GroundHeight(w.ActiveMap(), pos.X, pos.Z, pos.Y)
	kind := spot.Kind
	if kind < 0 || kind >= pickKinds { // authored "any" spot -> random kind
		kind = rand.Intn(pickKinds)
	}
	p := Pickup{Pos: pos, Kind: kind}
	if kind == PickWeapon {
		if spot.Weapon > 0 && spot.Weapon < len(Weapons) {
			p.Weapon = spot.Weapon // authored weapon
		} else {
			p.Weapon = DropWeapons[rand.Intn(len(DropWeapons))]
		}
	}
	w.pickups = append(w.pickups, p)
}

// pickupSpot returns a free location for a new drop: an unoccupied authored spot
// if the map lists any, otherwise a random open (non-blocked) tile.
func (w *World) pickupSpot() (MapPickup, bool) {
	occupied := func(x, z float64) bool {
		for _, p := range w.pickups {
			if p.Pos.X == x && p.Pos.Z == z {
				return true
			}
		}
		return false
	}
	if spots := w.ActiveMap().Pickups; len(spots) > 0 {
		for _, k := range rand.Perm(len(spots)) {
			if !occupied(spots[k].Pos.X, spots[k].Pos.Z) {
				return spots[k], true
			}
		}
		return MapPickup{}, false
	}
	for tries := 0; tries < 16; tries++ {
		x := (rand.Float64()*2 - 1) * (w.half() - 2)
		z := (rand.Float64()*2 - 1) * (w.half() - 2)
		if !w.blocked(V3{x, 0, z}) {
			return MapPickup{Pos: V3{x, 0, z}, Kind: -1}, true // -1 = any (random kind)
		}
	}
	return MapPickup{}, false
}

// applyPickup grants a power-up's effect to a tank.
func (w *World) applyPickup(t *Tank, kind, weapon int) {
	t.pickups++
	switch kind {
	case PickRepair:
		t.HP = t.veh().MaxHP
	case PickShield:
		t.shieldT = buffShieldTime
	case PickRapid:
		t.rapidT = buffRapidTime
	case PickCloak:
		t.cloakT = buffCloakTime
	case PickAmmo:
		t.ammo = t.veh().AmmoMax // instant refill of the regen pool
	case PickWeapon:
		if t.body == BodyTurtle {
			t.ammo = t.veh().AmmoMax // the turtle's B is its shell; take ammo instead
		} else if weapon > 0 && weapon < len(Weapons) {
			t.weapon2 = weapon // swap the secondary (B) weapon
		}
	}
}

// DropWeapons lists the weapons a PickWeapon drop can grant (everything but the
// default cannon).
var DropWeapons = []int{wepSlower, wepMedic, wepKnocker, wepBuster, wepGrenade, wepMine, wepLaser}

func (w *World) allFlagsTaken() bool {
	if len(w.flags) == 0 {
		return false
	}
	for i := range w.flags {
		if !w.flags[i].Taken {
			return false
		}
	}
	return true
}

func (w *World) checkEnd() {
	r := w.rules()
	done := r.TimeLimit > 0 && w.Timer <= 0 // implicit timeout
	for _, wc := range r.Win {
		if w.winCondMet(wc) {
			done = true
		}
	}
	if !done {
		return
	}
	if r.Teams == 2 {
		switch {
		case w.teamScore[0] > w.teamScore[1]:
			w.winnerTeam = 0
		case w.teamScore[1] > w.teamScore[0]:
			w.winnerTeam = 1
		default:
			w.winnerTeam = -1
		}
	}
	w.Phase, w.Timer, w.Shots = PhaseEnded, endTime, nil
	w.WinnerID = w.computeWinner()
}

// winCondMet evaluates one win condition against the current world state.
func (w *World) winCondMet(wc WinCond) bool {
	switch wc.Kind {
	case WinFrags:
		for i := range w.Tanks {
			if !w.Tanks[i].gone && w.Tanks[i].Kills >= wc.Count {
				return true
			}
		}
	case WinCaptures:
		return w.teamScore[0] >= wc.Count || w.teamScore[1] >= wc.Count
	case WinCollectAll:
		return w.allFlagsTaken()
	case WinElimination:
		return w.eliminationMet()
	case WinScore:
		if w.rules().Teams == 2 {
			return w.teamScore[0] >= wc.Count || w.teamScore[1] >= wc.Count
		}
		for i := range w.Tanks {
			if !w.Tanks[i].gone && w.Tanks[i].holdScore >= wc.Count {
				return true
			}
		}
	}
	return false
}

// eliminationMet reports whether an elimination win condition is satisfied. For
// a co-op ruleset (Survival) that means every human is out of lives; otherwise
// it's last-side-standing (one or zero tanks left alive) - used by FFA/team
// elimination modes (Phase B Stage 3).
func (w *World) eliminationMet() bool {
	if w.rules().CoOp {
		humans, allOut := 0, true
		for i := range w.Tanks {
			t := &w.Tanks[i]
			if t.Bot || t.gone {
				continue
			}
			humans++
			if !t.Dead || t.lives > 0 {
				allOut = false
			}
		}
		return humans > 0 && allOut
	}
	alive := 0
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone {
			continue
		}
		if !t.Dead || t.lives > 0 {
			alive++
		}
	}
	return alive <= 1
}

func (w *World) computeWinner() int {
	r := w.rules()
	if r.CoOp {
		return -1 // co-op: the result is the wave/progress reached, not a winner
	}
	if r.Teams == 2 {
		return -1 // team mode: the winner is a team, not a tank (see WinnerTeam)
	}
	if r.Objective == ObjNeutralFlags {
		if !w.allFlagsTaken() {
			return -1 // ran out of time
		}
		for i := range w.Tanks { // the (solo) collector
			if !w.Tanks[i].Bot && !w.Tanks[i].gone {
				return i
			}
		}
		return -1
	}
	for _, wc := range r.Win {
		switch wc.Kind {
		case WinElimination: // last tank standing wins (0 alive = draw)
			for i := range w.Tanks {
				t := &w.Tanks[i]
				if !t.gone && (!t.Dead || t.lives > 0) {
					return i
				}
			}
			return -1
		case WinScore: // FFA King of the Hill: most hold-points wins
			best, bestS := -1, -1
			for i := range w.Tanks {
				if w.Tanks[i].gone {
					continue
				}
				if w.Tanks[i].holdScore > bestS {
					bestS, best = w.Tanks[i].holdScore, i
				}
			}
			return best
		}
	}
	best, bestK := -1, -1
	for i := range w.Tanks {
		if w.Tanks[i].gone {
			continue
		}
		if w.Tanks[i].Kills > bestK {
			bestK, best = w.Tanks[i].Kills, i
		}
	}
	return best
}

func (w *World) simulate(dt float64, inputs map[int]Input) {
	w.ticks++ // nav repath staggering rides the tick count
	for i := range w.Tanks {
		if w.Tanks[i].cooldown > 0 {
			w.Tanks[i].cooldown -= dt
		}
		if w.Tanks[i].cooldown2 > 0 {
			w.Tanks[i].cooldown2 -= dt
		}
		if w.Tanks[i].pounceT > 0 {
			w.Tanks[i].pounceT -= dt
		}
		if w.Tanks[i].wp2Used > 0 { // charge-stock secondaries regenerate one charge at a time
			if w.Tanks[i].wp2RegenT -= dt; w.Tanks[i].wp2RegenT <= 0 {
				w.Tanks[i].wp2Used--
				if w.Tanks[i].wp2Used > 0 && w.Tanks[i].weapon2 > 0 && w.Tanks[i].weapon2 < len(Weapons) {
					w.Tanks[i].wp2RegenT = Weapons[w.Tanks[i].weapon2].ChargeRegen
				}
			}
		}
		if w.Tanks[i].hookT > 0 {
			w.Tanks[i].hookT -= dt
		}
		if w.Tanks[i].healFlash > 0 {
			w.Tanks[i].healFlash -= dt
		}
		if max := w.Tanks[i].veh().AmmoMax; w.Tanks[i].ammo < max { // regenerate ammo
			if w.Tanks[i].ammo += w.Tanks[i].veh().AmmoRegen * dt; w.Tanks[i].ammo > max {
				w.Tanks[i].ammo = max
			}
		}
		if w.Tanks[i].slowT > 0 {
			w.Tanks[i].slowT -= dt
		}
		if w.Tanks[i].slipT > 0 {
			w.Tanks[i].slipT -= dt
		}
		if w.Tanks[i].dmgDownT > 0 {
			w.Tanks[i].dmgDownT -= dt
		}
		if w.Tanks[i].guard > 0 {
			w.Tanks[i].guard -= dt
		}
		if w.Tanks[i].hitFlash > 0 {
			w.Tanks[i].hitFlash -= dt
		}
		if w.Tanks[i].teleT > 0 {
			w.Tanks[i].teleT -= dt
		}
	}
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || t.Dead {
			continue
		}
		if t.Bot {
			w.botAI(i, dt)
		} else {
			w.applyInput(i, inputs[i], dt)
		}
	}
	w.stepProjectiles(dt)
	r := w.rules()
	switch r.Objective {
	case ObjNeutralFlags:
		w.collectFlags()
	case ObjTeamFlags:
		w.stepCTF(dt)
	case ObjZone:
		w.stepZones(dt)
	}
	w.stepPickups(dt)
	w.stepEntities(dt)
	w.respawns(dt)
	if r.Bots == BotWaves && w.activeBots() == 0 {
		w.emit("wave_cleared", -1, -1)
		w.spawnWave() // wave cleared -> next, bigger wave
	}
	w.stepTankWatch()  // mobile-boss HP thresholds -> hp_below
	w.runBehaviors(dt) // dispatch this tick's signals (start/killed/.../custom)
}

// aimAssistStep is lock-on-sweep aim assist for a human player. While the player
// is turning/elevating the turret, if the aim is within the capture radius of a
// valid target (both axes, in range, line of sight), it LOCKS onto that target:
// the turret snaps on and tracks it so the player can fire, catching small/fast-
// passing targets that discrete key steps overshoot. Holding still keeps the lock;
// keeping a turn held for assistLockBreak sec releases it; Recenter and target
// loss clear it. Valid targets are live enemy tanks and shootable entities
// (turrets, destructibles) - the small/elevated things hardest to hit by hand.
func (w *World) aimAssistStep(i int, turning, elevating bool, dt float64) {
	if !w.assistAim {
		return
	}
	t := &w.Tanks[i]
	if t.lockCool > 0 {
		t.lockCool -= dt
	}
	// Maintain an existing lock: hold/track it, or release on a sustained turn.
	if t.lockKind != 0 {
		if p, ok := w.lockPoint(i); ok {
			if turning || elevating {
				if t.lockBreak += dt; t.lockBreak >= assistLockBreak {
					// released; suppress re-acquire so the held turn carries you off
					t.lockKind, t.lockBreak, t.lockCool = 0, 0, assistBreakCool
					return
				}
			} else {
				t.lockBreak = 0
			}
			w.aimAt(t, p)
			return
		}
		t.lockKind, t.lockBreak = 0, 0 // target gone
	}
	if (!turning && !elevating) || t.lockCool > 0 {
		return // acquire only while actively aiming, and not during break cooldown
	}
	// Acquire: the valid target closest to the reticle within the capture radii.
	aimYaw := t.HullYaw + t.TurretYaw
	muzzleY := t.Pos.Y + EyeHeight
	teamMode := w.rules().Teams == 2
	best := assistCaptureYaw
	bestKind, bestIdx := 0, -1
	var bestP V3
	consider := func(kind, idx int, p V3) {
		dx, dz := p.X-t.Pos.X, p.Z-t.Pos.Z
		dist := math.Hypot(dx, dz)
		if dist < 0.5 || dist > botFireRange {
			return
		}
		yoff := math.Abs(angDiff(math.Atan2(dx, dz), aimYaw))
		if yoff >= best {
			return
		}
		wp := clampPitch(math.Atan2(p.Y-muzzleY, dist))
		if math.Abs(wp-t.TurretPitch) >= assistCapturePitch {
			return
		}
		if w.aimBlocked(t.Pos, p) {
			return
		}
		best, bestKind, bestIdx, bestP = yoff, kind, idx, p
	}
	for j := range w.Tanks {
		e := &w.Tanks[j]
		if j == i || e.Dead || e.gone || e.cloakT > 0 || (teamMode && e.Team == t.Team) {
			continue
		}
		consider(1, j, V3{e.Pos.X, e.Pos.Y + turretAimHeight, e.Pos.Z})
	}
	for k := range w.entities {
		e := &w.entities[k]
		if e.Dead || e.Destruct == nil {
			continue
		}
		consider(2, k, e.Pos)
	}
	if bestKind == 0 {
		return
	}
	t.lockKind, t.lockIdx, t.lockBreak = bestKind, bestIdx, 0
	w.aimAt(t, bestP)
}

// lockPoint resolves the current aim-lock's target world point and whether it's
// still a valid lock (alive, in range, line of sight).
func (w *World) lockPoint(i int) (V3, bool) {
	t := &w.Tanks[i]
	var p V3
	switch t.lockKind {
	case 1:
		if t.lockIdx < 0 || t.lockIdx >= len(w.Tanks) {
			return V3{}, false
		}
		e := &w.Tanks[t.lockIdx]
		if e.Dead || e.gone || e.cloakT > 0 {
			return V3{}, false
		}
		if w.rules().Teams == 2 && e.Team == t.Team {
			return V3{}, false
		}
		p = V3{e.Pos.X, e.Pos.Y + turretAimHeight, e.Pos.Z}
	case 2:
		if t.lockIdx < 0 || t.lockIdx >= len(w.entities) {
			return V3{}, false
		}
		e := &w.entities[t.lockIdx]
		if e.Dead || e.Destruct == nil {
			return V3{}, false
		}
		p = e.Pos
	default:
		return V3{}, false
	}
	dx, dz := p.X-t.Pos.X, p.Z-t.Pos.Z
	if math.Hypot(dx, dz) > botFireRange || w.aimBlocked(t.Pos, p) {
		return V3{}, false
	}
	return p, true
}

// aimAt points a tank's turret (yaw + pitch) exactly at a world point.
func (w *World) aimAt(t *Tank, p V3) {
	dx, dz := p.X-t.Pos.X, p.Z-t.Pos.Z
	t.TurretYaw = angDiff(math.Atan2(dx, dz), t.HullYaw)
	t.TurretPitch = clampPitch(math.Atan2(p.Y-(t.Pos.Y+EyeHeight), math.Hypot(dx, dz)))
}

// aimBlocked reports whether a tall obstacle sits on the line between two points
// (a cheap line-of-sight check at eye height for aim assist).
func (w *World) aimBlocked(from, to V3) bool {
	const steps = 8
	for k := 1; k < steps; k++ {
		f := float64(k) / float64(steps)
		p := V3{from.X + (to.X-from.X)*f, EyeHeight, from.Z + (to.Z-from.Z)*f}
		if w.hitObstacle(p) {
			return true
		}
	}
	return false
}

func (w *World) applyInput(i int, in Input, dt float64) {
	t := &w.Tanks[i]
	v := t.veh()
	f := fwd(t.HullYaw)
	if t.body == BodyTurtle && in.Fire2 && t.cooldown2 <= 0 {
		if t.shellT > 0 { // pop out early; the recharge starts now
			t.shellT, t.cooldown2 = 0, shellRecharge
		} else { // shell up: immobile + invulnerable until it expires or B again
			t.shellT, t.cooldown2 = shellDuration, 0.4 // short debounce vs a held key
		}
	}
	if t.shellT > 0 { // tucked in: nothing else moves, aims, or fires
		clampArena(&t.Pos, w.half())
		w.collide(&t.Pos)
		support := w.ground(t.Pos.X, t.Pos.Z, t.Pos.Y)
		stepVertical(&t.Pos, &t.vy, false, dt, 0, support) // gravity still applies
		return
	}
	// Minotaur barrier: B TOGGLES it (a terminal can't hold a key while you also
	// drive with WASD, so hold-to-brace would strand you). Tap to raise (when it
	// has HP and isn't on the post-shatter cooldown), tap again to lower. While
	// up you can turn and shuffle but not attack or jump - gated below on
	// shieldUp. cooldown2 is a short debounce so key auto-repeat can't flicker it.
	if t.body == BodyMinotaur && in.Fire2 && t.cooldown2 <= 0 {
		if t.shieldUp {
			t.shieldUp, t.cooldown2 = false, 0.4
		} else if t.shieldBroken <= 0 && t.shieldHP > 0 {
			t.shieldUp, t.cooldown2 = true, 0.4
		}
	}
	if t.body == BodyMinotaur && t.shieldUp && in.Fire {
		t.shieldUp = false // swinging drops the barrier; the fire below lands this tick
	}
	spd := v.Speed * BodySpeedMul(t.body)
	if t.slowT > 0 { // EffSlow drag
		spd *= 1 - t.slowMag
	}
	if t.boostT > 0 { // EffSpeed lift
		spd *= 1 + t.boostMag
	}
	if t.shieldUp { // a braced barrier slows the advance
		spd *= minoShieldMoveMul
	}
	// EffSlip (banana): no driver control - the tank slides helplessly forward at
	// 60% speed in its current facing and can't steer the hull. Turret aim below
	// still works. Server-authoritative; the Slip wire bit lets the client predict
	// the slide (net.go munges the predicted input) instead of rubber-banding.
	if t.slipT > 0 {
		t.Pos = t.Pos.Add(V3{f.X * spd * 0.6 * dt, 0, f.Z * spd * 0.6 * dt})
	} else {
		if in.Throttle {
			t.Pos = t.Pos.Add(V3{f.X * spd * dt, 0, f.Z * spd * dt})
		}
		if in.Reverse {
			t.Pos = t.Pos.Sub(V3{f.X * spd * dt, 0, f.Z * spd * dt})
		}
		if in.StrafeL || in.StrafeR { // sidestep along the right vector (f rotated -90)
			rt := V3{f.Z, 0, -f.X}
			if in.StrafeR {
				t.Pos = t.Pos.Add(V3{rt.X * spd * dt, 0, rt.Z * spd * dt})
			}
			if in.StrafeL {
				t.Pos = t.Pos.Sub(V3{rt.X * spd * dt, 0, rt.Z * spd * dt})
			}
		}
		if in.HullL {
			t.HullYaw -= v.HullTurn * dt
		}
		if in.HullR {
			t.HullYaw += v.HullTurn * dt
		}
	}
	if in.TurretL {
		t.TurretYaw -= v.AimTurn * dt
	}
	if in.TurretR {
		t.TurretYaw += v.AimTurn * dt
	}
	if in.AimUp {
		t.TurretPitch = clampPitch(t.TurretPitch + pitchRate*dt)
	}
	if in.AimDown {
		t.TurretPitch = clampPitch(t.TurretPitch - pitchRate*dt)
	}
	if in.Recenter {
		t.TurretYaw, t.TurretPitch = 0, 0 // snap turret to hull-forward and level
		t.lockKind, t.lockBreak = 0, 0    // clear any aim lock
	} else {
		w.aimAssistStep(i, in.TurretL || in.TurretR, in.AimUp || in.AimDown, dt)
	}
	desired := t.Pos // post-move, pre-collision: tells us if we pressed into a wall
	clampArena(&t.Pos, w.half())
	w.collide(&t.Pos)
	support := w.ground(t.Pos.X, t.Pos.Z, t.Pos.Y)
	jump := in.Jump && !t.shieldUp // can't jump/charge while bracing the barrier
	bd := bodyDefFor(t.body)
	moving := in.Throttle || in.Reverse || in.StrafeL || in.StrafeR
	switch {
	case bd.fly:
		stepFlight(&t.Pos, &t.vy, jump, dt, support) // butterfly: true hover
	case bd.climb && moving && w.obstacleSide(desired):
		// Scaling a wall: the insect presses into an obstacle face and scuttles up
		// it, gripping (no gravity). The moment the rise would clear the lip, snap
		// onto the top surface (pulled just inside the footprint) so it actually
		// crests over instead of bobbing at the edge and sliding back off.
		crest, ok := w.climbCrest(desired)
		if ok && t.Pos.Y+climbSpeed*dt >= crest.Y-stepUp {
			t.Pos, t.vy = crest, 0
		} else {
			t.Pos.Y += climbSpeed * dt
			t.vy = 0
		}
	default:
		jumpV := v.Jump
		if bd.jump > 0 {
			jumpV = bd.jump // per-character jump (a gorilla bounds, a turtle barely hops)
		}
		if bd.leap && jump && t.Pos.Y <= support+0.0001 && t.vy <= 0 { // leap forward into the fray
			f := fwd(t.HullYaw)
			t.lungeVX, t.lungeVZ = f.X*lungeSpeed, f.Z*lungeSpeed
		}
		wasAir := t.Pos.Y > support+0.05
		stepVertical(&t.Pos, &t.vy, jump, dt, jumpV, support)
		if bd.leap && wasAir && t.Pos.Y <= support {
			w.leapLand(i) // the bruiser slams down: splash anyone alongside
		}
	}
	w.stepLunge(t, dt)
	if in.Fire && t.cooldown <= 0 && !t.shieldUp { // no attacking while the barrier is braced
		w.fire(i)
	}
	// B is the barrier for the minotaur (handled above), not a weapon; everyone
	// else fires their secondary.
	if in.Fire2 && t.body != BodyMinotaur {
		w.fireSecondary(i) // B: secondary weapon (cooldown/charge/weapon2 gates live here)
	}
	if in.Drop {
		w.dropFlag(i) // CTF: set the carried flag down where we stand
	}
}

// dropFlag releases a carried CTF flag at the carrier's feet: it sits on the
// usual drop-return timer (a friendly touch returns it home, an enemy can
// pick it up, or it auto-returns). Lets a carrier hand off or stash the flag
// while their own is still in enemy hands.
func (w *World) dropFlag(i int) {
	t := &w.Tanks[i]
	if t.Carrying < 0 || t.Carrying >= len(w.flags) {
		return
	}
	f := &w.flags[t.Carrying]
	f.Carrier, f.atHome = -1, false
	f.Pos = V3{t.Pos.X, 0, t.Pos.Z}
	f.dropTimer = flagReturnTime
	t.Carrying = -1
}

// stepVertical applies a jump impulse (only when grounded on `support`) then
// gravity, resting at the support height. Shared by server and client predictor.
func stepVertical(pos *V3, vy *float64, jump bool, dt, jumpV, support float64) {
	if jump && pos.Y <= support+0.0001 && *vy <= 0 {
		*vy = jumpV
	}
	*vy -= gravity * dt
	pos.Y += *vy * dt
	if pos.Y < support {
		pos.Y = support
		*vy = 0
	}
}

// stepFlight is true flight (the butterfly): hold jump to rise, release to drift
// down, floating up to a ceiling above the ground - dodgy and above ground hazards.
func stepFlight(pos *V3, vy *float64, rise bool, dt, support float64) {
	const lift, fall, maxV, ceil = 24.0, 7.0, 12.0, 8.0
	if rise {
		*vy += lift * dt
	} else {
		*vy -= fall * dt
	}
	if *vy > maxV {
		*vy = maxV
	} else if *vy < -maxV {
		*vy = -maxV
	}
	pos.Y += *vy * dt
	if pos.Y < support {
		pos.Y, *vy = support, 0
	} else if pos.Y > support+ceil {
		pos.Y, *vy = support+ceil, 0
	}
}

// Predict advances a tank's kinematics from one input sample using the same
// movement constants as the authoritative sim (firing/combat excluded). The
// net client uses it to predict its own tank between server snapshots so steering
// feels instant; because the math matches, reconciliation error stays small.
// solids are the alive solid entities' collision boxes this tick (from
// SolidBoxes) so the predictor blocks on them exactly as the server does.
func Predict(pos V3, hullYaw, turretYaw, pitch, vy float64, in Input, dt float64, v Vehicle, m Map, solids []Box) (V3, float64, float64, float64, float64) {
	f := fwd(hullYaw)
	if in.Throttle {
		pos = pos.Add(V3{f.X * v.Speed * dt, 0, f.Z * v.Speed * dt})
	}
	if in.Reverse {
		pos = pos.Sub(V3{f.X * v.Speed * dt, 0, f.Z * v.Speed * dt})
	}
	if in.StrafeL || in.StrafeR { // sidestep along the right vector (must match applyInput)
		rt := V3{f.Z, 0, -f.X}
		if in.StrafeR {
			pos = pos.Add(V3{rt.X * v.Speed * dt, 0, rt.Z * v.Speed * dt})
		}
		if in.StrafeL {
			pos = pos.Sub(V3{rt.X * v.Speed * dt, 0, rt.Z * v.Speed * dt})
		}
	}
	if in.HullL {
		hullYaw -= v.HullTurn * dt
	}
	if in.HullR {
		hullYaw += v.HullTurn * dt
	}
	if in.TurretL {
		turretYaw -= v.AimTurn * dt
	}
	if in.TurretR {
		turretYaw += v.AimTurn * dt
	}
	if in.AimUp {
		pitch = clampPitch(pitch + pitchRate*dt)
	}
	if in.AimDown {
		pitch = clampPitch(pitch - pitchRate*dt)
	}
	if in.Recenter {
		turretYaw, pitch = 0, 0 // match applyInput so prediction doesn't fight the snap
	}
	boxes := m.Obstacles
	if len(solids) > 0 {
		boxes = append(append([]Box(nil), m.Obstacles...), solids...)
	}
	clampArena(&pos, MapHalf(m))
	CollideBoxes(boxes, &pos)
	CollideRamps(m.Ramps, &pos)
	support := GroundBoxes(boxes, m.Ramps, pos.X, pos.Z, pos.Y)
	stepVertical(&pos, &vy, in.Jump, dt, v.Jump, support)
	return pos, hullYaw, turretYaw, pitch, vy
}

// Veh exposes a vehicle's stats by index (for the client predictor).
func Veh(i int) Vehicle { return veh(i) }

// BuiltinVehicles is the number of shared preset chassis (custom builds are a
// per-tank override, never added to this table, so bots never roll one).
func BuiltinVehicles() int { return len(Vehicles) }

// Render body styles (silhouette only; the sim treats every actor as a tank that
// moves, shoots, and dies). Survival assigns creature bodies to its wave enemies;
// the renderer's appendCreature draws them with an animated gait. BodyTank = 0 so
// the zero value is an ordinary tank.
const (
	BodyTank   = iota
	BodySpider // retired from the roster (kept so existing indices/maps don't shift)
	BodyQuad
	BodyInsect
	BodyHumanoid
	BodyScorpion
	BodySerpent
	BodyTripod // retired from the roster (kept so existing indices/maps don't shift)
	BodyDrone  // retired from the roster (kept so existing indices/maps don't shift)
	BodyCrab
	BodyOctopod
	BodyButterfly // flying healer / support
	BodyMantis
	BodyTurtle
	BodyTrex
	BodyGorilla
	BodyElephant // support tank: tusk gore + trunk shield-spray
	BodyFalcon   // flying striker
	BodyStag     // ground healer: radial aura + speed boost
	BodyMinotaur // bruiser: hammer + a held, destructible frontal barrier
	BodyKinds
)

// botBodies is the pool regular (non-Survival) bots roll their characters from -
// the tank weighted in so the field keeps some armor among the creatures.
var botBodies = []int{
	BodyTank, BodyTank, BodyTank,
	BodyQuad, BodyInsect, BodyHumanoid, BodyScorpion, BodySerpent,
	BodyCrab, BodyOctopod, BodyMantis, BodyTurtle, BodyTrex, BodyGorilla,
	BodyElephant, BodyFalcon, BodyStag, BodyMinotaur,
}

// bodyDef gives each character a distinct identity beyond the chassis: a signature
// primary weapon, a jump override, a muzzle origin (body-local: x lateral, y up, z
// forward; rotated by facing) so shots leave from the right place, and a flight
// flag (the butterfly hovers). Stats (HP/speed/...) still come from the chassis.
type bodyDef struct {
	weapon    int     // signature primary; -1 = the cannon
	secondary int     // secondary (B) default; 0 = the usual grenade
	jump      float64 // jump velocity override; 0 = chassis default
	muzzle    V3      // local muzzle origin
	fly       bool    // true flight (hover) instead of a one-shot jump
	leap      bool    // a jump also lunges forward (melee charge)
	climb     bool    // scales obstacle walls: presses into a face and scuttles up it
	hpRegen   float64 // passive HP/sec recharge (fragile fast movers; 0 = none)
	speedMul  float64 // move-speed multiplier over the chassis (0 = 1.0)
}

// BodySpeedMul is a body's move-speed multiplier over its chassis (1.0 if none).
// Applied wherever movement speed is computed (sim, bots, and the net predictor).
func BodySpeedMul(body int) float64 {
	if m := bodyDefFor(body).speedMul; m > 0 {
		return m
	}
	return 1
}

// defaultSecondary is a character's starting B-weapon (pickups can still change it).
func defaultSecondary(body int) int {
	if s := bodyDefFor(body).secondary; s > 0 {
		return s
	}
	return wepGrenade
}

// WeaponName is the display name of weapon index i (CANNON if out of range).
func WeaponName(i int) string {
	if i < 0 || i >= len(Weapons) {
		return "CANNON"
	}
	return Weapons[i].Name
}

// PrimaryWeapon is the weapon index a body fires as its primary (CANNON for the
// plain tank and any body with no signature weapon).
func PrimaryWeapon(body int) int {
	if bd := bodyDefFor(body); bd.weapon >= 0 {
		return bd.weapon
	}
	return wepCannon
}

// SecondaryWeapon is a body's default B-weapon (pickups can change it in play).
func SecondaryWeapon(body int) int { return defaultSecondary(body) }

// EffectiveJump is a body's jump impulse: its bodyDef override if set, else the
// body's own row default. Used for the character-select jump stat.
func EffectiveJump(body int) float64 {
	if j := bodyDefFor(body).jump; j > 0 {
		return j
	}
	return VehBody(body).Jump
}

// bodyDefFor returns a body's definition with any server-pushed numeric balance
// override applied (see ApplyBalance). The structural fields (signature weapon,
// muzzle, fly/leap/climb) come straight from the builtin def and are NOT pushable
// - they are kit identity, not tuning, so changing them is a client-update-worthy
// redesign. Only the scalar knobs (jump, hpRegen, speedMul) ride the wire.
func bodyDefFor(body int) bodyDef {
	d := bodyDefBuiltin(body)
	if body >= 0 && body < len(bodyBal) {
		d.jump = bodyBal[body].Jump
		d.hpRegen = bodyBal[body].HPRegen
		d.speedMul = bodyBal[body].SpeedMul
	}
	return d
}

func bodyDefBuiltin(body int) bodyDef {
	switch body {
	case BodyScorpion:
		return bodyDef{weapon: wepLaser, secondary: wepSting, muzzle: V3{0, 2.2, 0.2}, hpRegen: 1.5} // arched tail
	case BodySerpent:
		return bodyDef{weapon: wepVenom, secondary: wepVSpray, muzzle: V3{0, 1.0, 1.6}, hpRegen: 3} // poison spit from the raised head
	case BodySpider: // retired from the roster
		return bodyDef{weapon: wepSlower, muzzle: V3{0, 0.8, 1.3}} // web from the fangs
	case BodyCrab:
		return bodyDef{weapon: wepSand, secondary: wepClaw, muzzle: V3{0.6, 0.8, 0.9}} // sand spray; heavy claw melee
	case BodyOctopod:
		return bodyDef{weapon: wepSlower, secondary: wepInk, muzzle: V3{0, 1.0, 0.8}, hpRegen: 2} // ink
	case BodyInsect:
		return bodyDef{weapon: wepSpit, secondary: wepWeb, muzzle: V3{0, 0.8, 1.0}, climb: true, hpRegen: 2, speedMul: 1.45} // scuttles fast and scales walls; sprays its deep artillery magazine
	case BodyHumanoid:
		return bodyDef{weapon: wepGun, secondary: wepGrenade, muzzle: V3{0.4, 1.6, 0.4}, hpRegen: 1.5} // fires a sidearm from the hand
	case BodyQuad: // the tiger: pounce in (leap), scratch for a bleed, lick wounds (regen)
		return bodyDef{weapon: wepScratch, secondary: wepPounce, jump: 12, leap: true, muzzle: V3{0, 0.9, 1.0}, hpRegen: 3.5, speedMul: 1.2}
	case BodyButterfly:
		return bodyDef{weapon: wepMedic, secondary: wepHealBomb, fly: true, muzzle: V3{0, 1.6, 0.6}, hpRegen: 2.5} // heal beam + heal bombs, flies
	case BodyMantis:
		return bodyDef{weapon: wepSlash, secondary: wepSpine, jump: 12, leap: true, muzzle: V3{0.3, 1.4, 0.7}, hpRegen: 2} // raptorial forearm: leaps in and slashes
	case BodyTurtle:
		return bodyDef{weapon: wepSnap, jump: 4, muzzle: V3{0, 0.7, 1.1}} // bunker: snapping bite up close, B = shell up
	case BodyTrex:
		return bodyDef{weapon: wepFlame, secondary: wepRoar, jump: 7, muzzle: V3{0, 2.4, 1.6}} // towering fire-breather (jaws)
	case BodyGorilla:
		return bodyDef{weapon: wepPound, secondary: wepBanana, jump: 13, leap: true, muzzle: V3{0, 1.2, 0.6}, hpRegen: 1} // melee bruiser: leaps in, pounds everyone in range
	case BodyElephant:
		return bodyDef{weapon: wepTusks, secondary: wepAegis, jump: 5, muzzle: V3{0, 1.6, 1.4}} // support tank: gores up close, trunk-sprays shields
	case BodyFalcon:
		return bodyDef{weapon: wepTalon, secondary: wepGust, fly: true, muzzle: V3{0, 1.2, 0.8}, hpRegen: 3} // flying striker
	case BodyStag:
		return bodyDef{weapon: wepAura, secondary: wepSwift, jump: 12, leap: true, muzzle: V3{0, 1.4, 0.9}, hpRegen: 2.5} // pack healer; antler charge
	case BodyMinotaur:
		return bodyDef{weapon: wepHammer, secondary: -1, jump: 8, leap: true, muzzle: V3{0, 1.7, 0.9}} // hammer + a B-toggled barrier; a gore-charge on jump
	}
	return bodyDef{weapon: -1, secondary: wepSmoke, muzzle: V3{0, EyeHeight, 1.7}} // tank default
}

// --- pushable balance (server-authoritative roster tuning) ------------------
//
// The arena server owns the authoritative numbers; combat (HP/damage) is already
// resolved server-side, but a stale client mispredicts movement and shows wrong
// stat bars. ApplyBalance lets the server push its tuning to clients so a tweak
// deploys without a client rebuild: the server sends CurrentBalance() once after
// the welcome, and the client applies it over its compiled-in defaults. An older
// client that doesn't understand the message simply keeps its built-in numbers
// (the server's combat math still governs the fight), so the push is additive
// and never forces a client update.

// BodyRow is the pushable numeric stat row of one character (per body now - the
// shared chassis is gone). Name/Desc are intentionally omitted - identity, not
// tuning.
type BodyRow struct {
	MaxHP                                                                int
	Speed, HullTurn, AimTurn, FireDelay, Jump, Scale, AmmoMax, AmmoRegen float64
}

// BodyStats are the pushable per-body kit-modifier scalars layered over the row.
// The kit itself (weapon, muzzle, fly/leap/climb) stays compiled in; only these
// scalars travel.
type BodyStats struct {
	Jump, HPRegen, SpeedMul float64
}

// Balance is the full pushable tuning table: a stat row per body, plus the
// per-body scalar modifiers. Both are indexed by body.
type Balance struct {
	Rows   []BodyRow
	Bodies []BodyStats
}

// bodyVeh is the per-character stat table - each character owns its row (the
// shared chassis is retired; Vehicles[] survives only as an authoring palette).
var bodyVeh = []Vehicle{
	BodyTank:      {MaxHP: 100, Speed: 6, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1, AmmoMax: 8, AmmoRegen: 1.8},
	BodySpider:    {MaxHP: 70, Speed: 8.2, HullTurn: 2.4, AimTurn: 1.7, FireDelay: 0.42, Jump: 10, Scale: 0.82, AmmoMax: 6, AmmoRegen: 2.4},
	BodyQuad:      {MaxHP: 85, Speed: 7, HullTurn: 2.1, AimTurn: 1.5, FireDelay: 0.48, Jump: 9, Scale: 0.9, AmmoMax: 7, AmmoRegen: 2},
	BodyInsect:    {MaxHP: 60, Speed: 3.8, HullTurn: 1.1, AimTurn: 1.4, FireDelay: 0.5, Jump: 5, Scale: 1.12, AmmoMax: 14, AmmoRegen: 2.2},
	BodyHumanoid:  {MaxHP: 100, Speed: 6, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1, AmmoMax: 8, AmmoRegen: 1.8},
	BodyScorpion:  {MaxHP: 60, Speed: 3.8, HullTurn: 1.1, AimTurn: 1.4, FireDelay: 0.5, Jump: 5, Scale: 1.12, AmmoMax: 14, AmmoRegen: 2.2},
	BodySerpent:   {MaxHP: 70, Speed: 8.2, HullTurn: 2.4, AimTurn: 1.7, FireDelay: 0.42, Jump: 10, Scale: 0.82, AmmoMax: 6, AmmoRegen: 2.4},
	BodyTripod:    {MaxHP: 100, Speed: 6, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1, AmmoMax: 8, AmmoRegen: 1.8},
	BodyDrone:     {MaxHP: 100, Speed: 6, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1, AmmoMax: 8, AmmoRegen: 1.8},
	BodyCrab:      {MaxHP: 150, Speed: 4.3, HullTurn: 1.3, AimTurn: 1, FireDelay: 0.85, Jump: 6.5, Scale: 1.22, AmmoMax: 12, AmmoRegen: 1.2},
	BodyOctopod:   {MaxHP: 60, Speed: 3.8, HullTurn: 1.1, AimTurn: 1.4, FireDelay: 0.5, Jump: 5, Scale: 1.12, AmmoMax: 14, AmmoRegen: 2.2},
	BodyButterfly: {MaxHP: 70, Speed: 8.2, HullTurn: 2.4, AimTurn: 1.7, FireDelay: 0.42, Jump: 10, Scale: 0.82, AmmoMax: 6, AmmoRegen: 2.4},
	BodyMantis:    {MaxHP: 100, Speed: 6, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1, AmmoMax: 8, AmmoRegen: 1.8},
	BodyTurtle:    {MaxHP: 150, Speed: 4.3, HullTurn: 1.3, AimTurn: 1, FireDelay: 0.85, Jump: 6.5, Scale: 1.22, AmmoMax: 12, AmmoRegen: 1.2},
	BodyTrex:      {MaxHP: 150, Speed: 4.3, HullTurn: 1.3, AimTurn: 1, FireDelay: 0.85, Jump: 6.5, Scale: 1.22, AmmoMax: 12, AmmoRegen: 1.2},
	BodyGorilla:   {MaxHP: 100, Speed: 6, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1, AmmoMax: 8, AmmoRegen: 1.8},
	BodyElephant:  {MaxHP: 150, Speed: 4.3, HullTurn: 1.3, AimTurn: 1, FireDelay: 0.85, Jump: 6.5, Scale: 1.22, AmmoMax: 12, AmmoRegen: 1.2},
	BodyFalcon:    {MaxHP: 70, Speed: 8.2, HullTurn: 2.4, AimTurn: 1.7, FireDelay: 0.42, Jump: 10, Scale: 0.82, AmmoMax: 6, AmmoRegen: 2.4},
	BodyStag:      {MaxHP: 85, Speed: 7, HullTurn: 2.1, AimTurn: 1.5, FireDelay: 0.48, Jump: 9, Scale: 0.9, AmmoMax: 7, AmmoRegen: 2},
	BodyMinotaur:  {MaxHP: 150, Speed: 4.3, HullTurn: 1.3, AimTurn: 1, FireDelay: 0.85, Jump: 6.5, Scale: 1.22, AmmoMax: 12, AmmoRegen: 1.2},
}

// bodyBal is the live scalar layer (jump/hpRegen/speedMul), seeded from each
// body's builtin def so resolved stats match the builtins until a push changes them.
var bodyBal []BodyStats

func init() {
	bodyBal = make([]BodyStats, BodyKinds)
	for b := 0; b < BodyKinds; b++ {
		d := bodyDefBuiltin(b)
		bodyBal[b] = BodyStats{Jump: d.jump, HPRegen: d.hpRegen, SpeedMul: d.speedMul}
	}
}

// VehBody returns a character's stat row by body (TANK's row if out of range).
func VehBody(body int) Vehicle {
	if body < 0 || body >= len(bodyVeh) {
		body = BodyTank
	}
	return bodyVeh[body]
}

// CurrentBalance snapshots the live tables - what the server sends to clients.
func CurrentBalance() Balance {
	bal := Balance{
		Rows:   make([]BodyRow, len(bodyVeh)),
		Bodies: make([]BodyStats, len(bodyBal)),
	}
	for i, v := range bodyVeh {
		bal.Rows[i] = BodyRow{
			MaxHP: v.MaxHP, Speed: v.Speed, HullTurn: v.HullTurn, AimTurn: v.AimTurn,
			FireDelay: v.FireDelay, Jump: v.Jump, Scale: v.Scale, AmmoMax: v.AmmoMax, AmmoRegen: v.AmmoRegen,
		}
	}
	copy(bal.Bodies, bodyBal)
	return bal
}

// ApplyBalance overwrites the live per-character tables with a pushed (or
// file-loaded) tuning. Bounds-safe and index-aligned by body: entries past the
// local table length are ignored, and a shorter push leaves the rest at their
// built-in values, so a client and server with different roster sizes still
// interoperate.
func ApplyBalance(bal Balance) {
	for i := 0; i < len(bal.Rows) && i < len(bodyVeh); i++ {
		c := bal.Rows[i]
		v := &bodyVeh[i]
		v.MaxHP, v.Speed, v.HullTurn, v.AimTurn = c.MaxHP, c.Speed, c.HullTurn, c.AimTurn
		v.FireDelay, v.Jump, v.Scale, v.AmmoMax, v.AmmoRegen = c.FireDelay, c.Jump, c.Scale, c.AmmoMax, c.AmmoRegen
	}
	for i := 0; i < len(bal.Bodies) && i < len(bodyBal); i++ {
		bodyBal[i] = bal.Bodies[i]
	}
}

// HPRegen is a body's passive HP/sec recharge (0 for the armored bruisers).
// Exposed for the character-select UI.
func HPRegen(body int) float64 { return bodyDefFor(body).hpRegen }

// SecondaryWeaponName is the display name of a body's B-key action: the turtle's
// B is its shell (a mode, not a palette weapon), everyone else's is a weapon.
func SecondaryWeaponName(body int) string {
	switch body {
	case BodyTurtle:
		return "SHELL"
	case BodyMinotaur:
		return "SHIELD"
	}
	return WeaponName(SecondaryWeapon(body))
}

func (w *World) botAI(i int, dt float64) {
	if w.Tanks[i].shellT > 0 {
		return // tucked into the shell: sit it out
	}
	w.botSpecial(i)
	if w.Tanks[i].shellT > 0 {
		return // just shelled up
	}
	if w.Tanks[i].body == BodyMinotaur { // braces its barrier toward the threat, then commits
		w.botMinotaurAI(i, dt)
		return
	}
	// Healers mend allies instead of fighting. The butterfly always plays medic;
	// the stag only when there's a team to heal (in FFA its aura stings, so it
	// skirmishes like everyone else).
	if w.Tanks[i].body == BodyButterfly || (w.Tanks[i].body == BodyStag && w.rules().Teams == 2) {
		w.botHealerAI(i, dt)
		return
	}
	if w.rules().Teams == 2 {
		w.ctfBotAI(i, dt)
		return
	}
	b := &w.Tanks[i]
	tgt := w.nearestEnemy(i)
	od, seekObj, holdObj := w.botObjectiveDest(i)
	if tgt < 0 {
		b.lastTgt, b.acquireT = -1, 0
		switch {
		case seekObj: // no one in sight: take ground at the objective
			v := b.veh()
			ang := math.Atan2(od.X-b.Pos.X, od.Z-b.Pos.Z)
			b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, od, ang)), v.HullTurn*dt)
			b.TurretYaw = turnToward(b.TurretYaw, 0, b.aiTrack*dt*0.5)
			w.driveForward(i, dt, 0.7)
			w.botVertical(i, dt, w.wantHop(b))
		case holdObj: // in position: stand the post, scan, let the capture run
			b.TurretYaw = turnToward(b.TurretYaw, 0, b.aiTrack*dt*0.5)
			w.botVertical(i, dt, false)
		default:
			w.botWander(i, dt) // no one in sight: roam instead of standing still
		}
		return
	}
	if tgt != b.lastTgt { // newly acquired target: take a beat to react
		b.lastTgt, b.acquireT = tgt, b.aiReact
	}
	if b.acquireT > 0 {
		b.acquireT -= dt
	}
	reacting := b.acquireT > 0
	v := b.veh()
	d := w.Tanks[tgt].Pos.Sub(b.Pos)
	dist := math.Hypot(d.X, d.Z)
	angTo := math.Atan2(d.X, d.Z)
	// Drive along the grid path to the target (direct when the way is open);
	// the whiskers still deflect locally. The turret aims at the target itself.
	b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, w.Tanks[tgt].Pos, angTo)), v.HullTurn*dt)
	wantPitch := clampPitch(math.Atan2((w.Tanks[tgt].Pos.Y+turretAimHeight)-(b.Pos.Y+EyeHeight), dist))
	if !reacting { // hold turret while reacting, then track at the bot's rate
		b.TurretYaw = turnToward(b.TurretYaw, angDiff(angTo, b.HullYaw), b.aiTrack*dt)
		b.TurretPitch = turnToward(b.TurretPitch, wantPitch, b.aiTrack*dt)
	}
	// Movement: the mode's objective ground comes first (the hill, a flag to
	// guard); then a power-up detour; melee bruisers charge to contact;
	// everyone else holds engagement range. The turret fights throughout.
	mr := w.botMeleeRange(b)
	keep := b.aiKeep
	if mr > 0 {
		keep = mr * 0.55 // close well inside strike range
	}
	if seekObj {
		ang := math.Atan2(od.X-b.Pos.X, od.Z-b.Pos.Z)
		b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, od, ang)), v.HullTurn*dt)
		w.driveForward(i, dt, 0.8)
	} else if holdObj {
		// Fight from the objective: aim and fire (handled below) but don't
		// chase off the hill / abandon the flag for range adjustments.
	} else if pp, ok := w.botSeekTarget(b); ok {
		ang := math.Atan2(pp.X-b.Pos.X, pp.Z-b.Pos.Z)
		b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, pp, ang)), v.HullTurn*dt)
		w.driveForward(i, dt, 0.8)
	} else if dist > keep {
		frac := 0.7
		if mr > 0 {
			frac = 0.95 // bruisers rush in
		}
		w.driveForward(i, dt, frac)
	}
	w.botVertical(i, dt, w.wantHop(b))
	fireRange, pitchOK := botFireRange, math.Abs(wantPitch-b.TurretPitch) < botAimTol
	if mr > 0 {
		fireRange, pitchOK = mr, true // a radial strike only lands in range, and ignores pitch
	}
	if !reacting && dist < fireRange && math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) < botAimTol &&
		pitchOK && b.cooldown <= 0 {
		w.fire(i) // fire() applies this bot's wobble + reload cadence
	}
	if !reacting {
		w.botSecondary(i, dist, angTo)
	}
}

// botObjectiveDest returns the mode's positional objective for bot i: a spot
// to drive to (the control zone, the nearest untaken flag to guard), whether
// to go there (seek), and whether the bot is already in position and should
// stand its ground (hold) instead of wandering off mid-capture.
func (w *World) botObjectiveDest(i int) (dest V3, seek, hold bool) {
	b := &w.Tanks[i]
	switch w.rules().Objective {
	case ObjZone:
		best, bestD := -1, math.MaxFloat64
		for zi := range w.zones {
			z := &w.zones[zi]
			dx, dz := z.Pos.X-b.Pos.X, z.Pos.Z-b.Pos.Z
			if dd := dx*dx + dz*dz; dd < bestD {
				bestD, best = dd, zi
			}
		}
		if best < 0 {
			return V3{}, false, false
		}
		z := &w.zones[best]
		if w.inZone(z, b.Pos) {
			return V3{}, false, true // actually on the hill (right XZ AND height): stand and fight
		}
		// FFA zones score on SOLE presence, so piling everyone on deadlocks
		// the hill as "contested" forever: go take an empty hill or duel its
		// one occupant, but with two-plus already brawling, fight from here.
		// Team zones contest by TEAM - teammates stack freely (reinforcing a
		// held hill is good play), so no pileup limit applies.
		if w.rules().Teams != 2 {
			occupants := 0
			for ti := range w.Tanks {
				t := &w.Tanks[ti]
				if t.Dead || t.gone {
					continue
				}
				if w.inZone(z, t.Pos) {
					occupants++
				}
			}
			if occupants >= 2 {
				return V3{}, false, false
			}
		}
		// Elevated hill: if we're below it, head for an ascending ramp instead of
		// grinding into the platform's sides (sparse-ramp maps otherwise leave bots
		// circling the base, never contesting - the hill looks dead).
		if b.Pos.Y < z.Pos.Y-stepUp-0.3 {
			if dest, ok := w.rampApproach(z, b.Pos); ok {
				return dest, true, false
			}
		}
		return z.Pos, true, false
	case ObjNeutralFlags:
		best, bestD := -1, math.MaxFloat64
		for fi := range w.flags {
			f := &w.flags[fi]
			if f.Taken {
				continue
			}
			dx, dz := f.Pos.X-b.Pos.X, f.Pos.Z-b.Pos.Z
			if dd := dx*dx + dz*dz; dd < bestD {
				bestD, best = dd, fi
			}
		}
		if best < 0 {
			return V3{}, false, false
		}
		if i == w.demoHero {
			// The demo's player stand-in plays the level: drive onto the
			// flag and take it, exactly as a human collector would.
			return w.flags[best].Pos, true, false
		}
		const guard = 5.5 // patrol ring: defend the flag, don't stack on it
		if bestD < guard*guard {
			return V3{}, false, true // in the guard ring: stay on post
		}
		return w.flags[best].Pos, true, false
	}
	return V3{}, false, false
}

// botMeleeRange is the strike radius if this bot's primary is a melee weapon (the
// gorilla's pound), else 0 - so the AI knows to charge in instead of firing from afar.
func (w *World) botMeleeRange(b *Tank) float64 {
	if b.body == BodyElephant {
		return 0 // the elephant hooks from range (fire() auto-gores once a foe is reeled in) - don't rush
	}
	def := &Weapons[wepCannon]
	if bd := bodyDefFor(b.body); bd.weapon >= 0 {
		def = &Weapons[bd.weapon]
	}
	if def.Delivery == DeliverMelee {
		if def.Blast > 0 {
			return def.Blast
		}
		return 4
	}
	// A character whose primary carries no damage (the crab's utility SAND cone)
	// but whose secondary IS melee (the claw) has to close to strike range to do
	// anything - treat its melee secondary as the rush distance so the bot commits.
	if def.Effect.Kind != EffDamage && def.Damage <= 0 &&
		b.weapon2 > 0 && b.weapon2 < len(Weapons) && Weapons[b.weapon2].Delivery == DeliverMelee {
		if bl := Weapons[b.weapon2].Blast; bl > 0 {
			return bl
		}
		return 4
	}
	return 0
}

// botSpecial drives the body-specific defensive kit a bot can't express through
// aim-and-shoot: the turtle shells up when cornered, the elephant trunk-sprays
// shields when it (or a nearby ally) is in trouble. Called each tick from botAI.
func (w *World) botSpecial(i int) {
	b := &w.Tanks[i]
	switch b.body {
	case BodyTurtle:
		if b.shellT > 0 || b.cooldown2 > 0 || b.HP*2 >= b.veh().MaxHP {
			return // healthy (>= 50%) or shell not recharged
		}
		if tgt := w.nearestEnemy(i); tgt >= 0 {
			d := w.Tanks[tgt].Pos.Sub(b.Pos)
			if d.X*d.X+d.Z*d.Z < 16*16 { // hurt and cornered: bunker down
				b.shellT, b.cooldown2 = shellDuration, 0.4
			}
		}
	case BodyElephant:
		if b.weapon2 != wepAegis || b.cooldown2 > 0 {
			return
		}
		need := b.HP*5 < b.veh().MaxHP*3 // own armor below 60%
		if !need {
			if a := w.mostHurtAlly(i); a >= 0 {
				d := w.Tanks[a].Pos.Sub(b.Pos)
				need = d.X*d.X+d.Z*d.Z < 36 // a wounded ally in trunk range
			}
		}
		if need && w.nearestEnemy(i) >= 0 { // only worth a coat mid-fight
			w.fireWeapon(i, &Weapons[wepAegis], true)
		}
	}
}

// botMinotaurAI drives the minotaur bot: it keeps its hull (and thus the barrier
// and hammer) pointed at the nearest foe, braces the barrier while closing the
// gap at a slowed pace, and drops the barrier to swing once in hammer reach -
// exposing itself, the way a player must. Without the facing, the directional
// barrier would point wherever the bot happened to be steering and rarely block.
func (w *World) botMinotaurAI(i int, dt float64) {
	b := &w.Tanks[i]
	v := b.veh()
	tgt := w.nearestEnemy(i)
	if tgt < 0 {
		b.shieldUp = false
		w.botWander(i, dt)
		return
	}
	d := w.Tanks[tgt].Pos.Sub(b.Pos)
	dist := math.Hypot(d.X, d.Z)
	ang := math.Atan2(d.X, d.Z)
	b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, w.Tanks[tgt].Pos, ang)), v.HullTurn*dt)
	b.TurretYaw = turnToward(b.TurretYaw, angDiff(ang, b.HullYaw), b.aiTrack*dt)
	melee := w.botMeleeRange(b) // hammer reach (~4)
	canShield := b.shieldBroken <= 0 && b.shieldHP > 0
	if dist > melee*0.9 { // approach: brace the barrier (slowed) unless it's broken
		b.shieldUp = canShield && dist < 34
		frac := 0.85
		if b.shieldUp {
			frac = 0.5 // a braced advance is a slow one
		}
		w.driveForward(i, dt, frac)
	} else { // in reach: drop the barrier and swing, exposed
		b.shieldUp = false
		if b.cooldown <= 0 && math.Abs(angDiff(b.HullYaw, ang)) < botAimTol {
			w.fire(i)
		}
	}
	w.botVertical(i, dt, w.wantHop(b))
}

// botSecondary lets a bot lob its secondary (grenade) at a target in the mid-range
// band where a level-aimed lob lands, on its own cooldown - so bots use grenades
// too instead of only the cannon.
func (w *World) botSecondary(i int, dist, angTo float64) {
	b := &w.Tanks[i]
	if b.weapon2 <= 0 || b.weapon2 >= len(Weapons) {
		return
	}
	if def2 := &Weapons[b.weapon2]; def2.Charges > 0 {
		if b.wp2Used >= def2.Charges || b.cooldown2 > 0 {
			return // charge-stock: out of charges or in the inter-snap gap
		}
	} else if b.cooldown2 > 0 {
		return
	}
	if b.body == BodyTurtle || Weapons[b.weapon2].Affects == TargetAllies {
		return // the shell and the support secondaries are botSpecial's job
	}
	// A character whose primary carries no damage (the octopod's ink slower)
	// fights WITH its secondary: wider envelope and nearly every opportunity,
	// instead of the occasional opportunistic lob.
	prim := &Weapons[wepCannon]
	if bd := bodyDefFor(b.body); bd.weapon >= 0 {
		prim = &Weapons[bd.weapon]
	}
	lo, hi, p := 9.0, 20.0, 0.5
	if prim.Effect.Kind != EffDamage && prim.Damage <= 0 {
		lo, hi, p = 5.0, 22.0, 0.85
	}
	if def2 := &Weapons[b.weapon2]; def2.Delivery == DeliverCone {
		// A forward cone (the falcon's gust) only reaches Blast units; the
		// lob band would have it firing at air.
		lo, hi = 1.5, def2.Blast*0.9
		if hi <= lo {
			hi = lo + 2
		}
	} else if def2 := &Weapons[b.weapon2]; def2.Delivery == DeliverMelee {
		// A melee secondary (crab claw snap, tiger pounce) is a close-range
		// commit: close the gap and strike, don't wait at lob range.
		lo, hi = 0, def2.Blast+1.0
	}
	if dist < lo || dist > hi {
		return
	}
	if math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) > botAimTol {
		return
	}
	if rand.Float64() < p {
		w.fireSecondary(i)
	}
}

// botWander ambles a bot toward a roaming destination when it has no target, so
// idle bots patrol the map (and stumble into fights) instead of standing still. A
// new destination is picked on arrival or after a few seconds.
func (w *World) botWander(i int, dt float64) {
	b := &w.Tanks[i]
	v := b.veh()
	b.roamT -= dt
	dx, dz := b.roam.X-b.Pos.X, b.roam.Z-b.Pos.Z
	if b.roamT <= 0 || dx*dx+dz*dz < 4 { // reached it or timed out: pick a new spot
		m := w.half() - 2
		if m < 2 {
			m = 2
		}
		b.roam = V3{X: (rand.Float64()*2 - 1) * m, Z: (rand.Float64()*2 - 1) * m}
		b.roamT = 3 + rand.Float64()*3
		dx, dz = b.roam.X-b.Pos.X, b.roam.Z-b.Pos.Z
	}
	b.HullYaw = turnToward(b.HullYaw, w.navYaw(i, b.roam, math.Atan2(dx, dz)), v.HullTurn*dt)
	b.TurretYaw = turnToward(b.TurretYaw, 0, b.aiTrack*dt*0.5) // ease the barrel back to center while scanning
	w.driveForward(i, dt, 0.5)                                 // amble, not a charge
	w.botVertical(i, dt, w.wantHop(b))
}

// botSeekTarget returns a power-up a SeekPickups bot should divert toward (the
// nearest within range; preferring a repair when hurt), or ok=false.
func (w *World) botSeekTarget(b *Tank) (V3, bool) {
	if !b.aiSeek || len(w.pickups) == 0 {
		return V3{}, false
	}
	best, bestD := -1, pickupSeekRange*pickupSeekRange
	for pi := range w.pickups {
		p := w.pickups[pi]
		dx, dz := p.Pos.X-b.Pos.X, p.Pos.Z-b.Pos.Z
		dd := dx*dx + dz*dz
		if dd > bestD {
			continue
		}
		// When healthy, only bother with a repair if badly hurt; else grab anything close.
		bestD, best = dd, pi
	}
	if best < 0 {
		return V3{}, false
	}
	return w.pickups[best].Pos, true
}

// ctfBotAI drives a CTF bot by objective: carry the enemy flag home, else fetch
// it, else defend by fighting the nearest enemy. The turret tracks and fires at
// the nearest enemy throughout, so bots stay dangerous while pursuing flags.
func (w *World) ctfBotAI(i int, dt float64) {
	b := &w.Tanks[i]
	v := b.veh()

	// Pick a destination from the objective.
	var dst V3
	have, hold := false, false
	switch {
	case b.Carrying >= 0:
		if of := w.ownFlag(b.Team); of != nil {
			dst, have = of.Home, true // run the flag back to base
		}
	default:
		if ef := w.enemyFlag(b.Team); ef != nil && ef.Carrier < 0 {
			dst, have = ef.Pos, true // go grab the enemy flag
		} else if zd, zok, zhold := w.botObjectiveDest(i); zok {
			dst, have = zd, true // Team KotH: converge on the hill
		} else if zhold {
			hold = true // on the hill: stand the post
		}
	}
	tgt := w.nearestEnemy(i)
	if !have && tgt >= 0 {
		dst, have = w.Tanks[tgt].Pos, true // nothing to fetch: hunt
	}
	switch {
	case have:
		d := dst.Sub(b.Pos)
		if math.Hypot(d.X, d.Z) > 1.0 {
			angTo := math.Atan2(d.X, d.Z)
			b.HullYaw = turnToward(b.HullYaw, w.avoidYaw(b, w.navYaw(i, dst, angTo)), v.HullTurn*dt)
			w.driveForward(i, dt, 0.7)
		}
	case hold:
		// On the hill: stand the post (the turret below still fights).
	default:
		w.botWander(i, dt) // no objective and no enemy: roam
	}
	// Turret tracks/fires at the nearest enemy regardless of where we're driving.
	if tgt >= 0 {
		if tgt != b.lastTgt {
			b.lastTgt, b.acquireT = tgt, b.aiReact
		}
		if b.acquireT > 0 {
			b.acquireT -= dt
		}
		d := w.Tanks[tgt].Pos.Sub(b.Pos)
		dist := math.Hypot(d.X, d.Z)
		angTo := math.Atan2(d.X, d.Z)
		wantPitch := clampPitch(math.Atan2((w.Tanks[tgt].Pos.Y+turretAimHeight)-(b.Pos.Y+EyeHeight), dist))
		if b.acquireT <= 0 {
			b.TurretYaw = turnToward(b.TurretYaw, angDiff(angTo, b.HullYaw), b.aiTrack*dt)
			b.TurretPitch = turnToward(b.TurretPitch, wantPitch, b.aiTrack*dt)
			fireRange, pitchOK := botFireRange, math.Abs(wantPitch-b.TurretPitch) < botAimTol
			if mr := w.botMeleeRange(b); mr > 0 {
				fireRange, pitchOK = mr, true
			}
			if dist < fireRange && math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) < botAimTol &&
				pitchOK && b.cooldown <= 0 {
				w.fire(i)
			}
			w.botSecondary(i, dist, angTo)
		}
	} else {
		b.lastTgt, b.acquireT = -1, 0
	}
	w.botVertical(i, dt, w.wantHop(b))
}

// inHazard reports whether a point lies in (or just outside, by a keep-clear
// margin) any live hazard's footprint - so bots steer around lava/spikes.
func (w *World) inHazard(p V3) bool {
	const margin = 1.0
	for i := range w.entities {
		e := &w.entities[i]
		if e.Hazard == nil || e.Dead {
			continue
		}
		if math.Abs(p.X-e.Pos.X) < e.Half.X+margin && math.Abs(p.Z-e.Pos.Z) < e.Half.Z+margin {
			return true
		}
	}
	return false
}

// jumpApex is how high this character's jump reaches (0 = effectively can't clear
// anything), from its body's jump override or its chassis default.
func (w *World) jumpApex(b *Tank) float64 {
	jv := b.veh().Jump
	if bd := bodyDefFor(b.body); bd.jump > 0 {
		jv = bd.jump
	}
	if jv <= 0 {
		return 0
	}
	return jv * jv / (2 * gravity)
}

// avoidYaw deflects a desired heading around obstacles and walls using whisker
// probes, returning the nearest clear heading (or `desired` if the way is open).
// Probes ride at the character's jump apex, so a strong jumper ignores low cover it
// can hop (wantHop then makes it jump) and only routes around what it can't clear.
func (w *World) avoidYaw(b *Tank, desired float64) float64 {
	clearH := w.jumpApex(b) * 0.8
	clear := func(yaw float64) bool {
		f := fwd(yaw)
		for _, d := range []float64{2.0, 4.0} { // look a few body-lengths ahead
			p := V3{b.Pos.X + f.X*d, b.Pos.Y + clearH, b.Pos.Z + f.Z*d}
			if w.blocked(p) || w.inHazard(p) { // route around solid cover and lava alike
				return false
			}
		}
		return true
	}
	if clear(desired) {
		return desired
	}
	for _, off := range []float64{0.45, -0.45, 0.9, -0.9, 1.4, -1.4, 2.0, -2.0} {
		if clear(desired + off) { // smallest deflection that opens up
			return desired + off
		}
	}
	return desired + math.Pi // boxed in: turn around
}

// wantHop reports whether a clearable obstacle sits dead ahead - so a jumping
// character bounds over low cover instead of stopping at it.
func (w *World) wantHop(b *Tank) bool {
	apex := w.jumpApex(b)
	if apex < 0.5 {
		return false
	}
	f := fwd(b.HullYaw)
	low := V3{b.Pos.X + f.X*2.5, b.Pos.Y, b.Pos.Z + f.Z*2.5}
	if !w.blocked(low) { // nothing immediately ahead
		return false
	}
	high := V3{low.X, b.Pos.Y + apex*0.8, low.Z}
	return !w.blocked(high) // ...and a jump would clear its top
}

// botVertical integrates a bot's jump/gravity (bots otherwise stay glued to the
// ground). wantJump launches it when grounded.
func (w *World) botVertical(i int, dt float64, wantJump bool) {
	b := &w.Tanks[i]
	support := w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y+stepUp)
	if bodyDefFor(b.body).fly { // flyers (butterfly) cruise at altitude, soaring over cover
		const cruiseAlt = 3.2
		b.Pos.Y += (support + cruiseAlt - b.Pos.Y) * math.Min(1, 4*dt)
		b.vy = 0
		w.stepLunge(b, dt)
		return
	}
	if wantJump && b.Pos.Y <= support+0.05 && b.vy <= 0 {
		bd := bodyDefFor(b.body)
		jv := b.veh().Jump
		if bd.jump > 0 {
			jv = bd.jump
		}
		b.vy = jv
		if bd.leap { // leapers charge forward when they jump
			f := fwd(b.HullYaw)
			b.lungeVX, b.lungeVZ = f.X*lungeSpeed, f.Z*lungeSpeed
		}
	}
	wasAir := b.Pos.Y > support+0.05
	b.vy -= gravity * dt
	b.Pos.Y += b.vy * dt
	if b.Pos.Y < support {
		b.Pos.Y, b.vy = support, 0
		if wasAir && bodyDefFor(b.body).leap {
			w.leapLand(i) // bot bruisers slam down too
		}
	}
	w.stepLunge(b, dt)
}

// driveForward moves a bot along its hull heading at the given speed fraction
// (turning to route around obstacles is the AI's job, via avoidYaw). It creeps
// rather than ramming when something is still close ahead.
func (w *World) driveForward(i int, dt, frac float64) {
	b := &w.Tanks[i]
	v := b.veh()
	f := fwd(b.HullYaw)
	if w.inHazard(V3{b.Pos.X + f.X*2.2, b.Pos.Y, b.Pos.Z + f.Z*2.2}) && b.Pos.Y <= w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y)+0.2 {
		return // lava dead ahead and we're grounded: hold while the hull turns away
	}
	if w.blocked(V3{b.Pos.X + f.X*3, b.Pos.Y, b.Pos.Z + f.Z*3}) {
		frac *= 0.35
	}
	spd := v.Speed * BodySpeedMul(b.body)
	b.Pos = b.Pos.Add(V3{f.X * spd * frac * dt, 0, f.Z * spd * frac * dt})
	clampArena(&b.Pos, w.half())
	w.collide(&b.Pos)
}

// nearestEnemy returns the closest live opponent. In free-for-all that's anyone
// else; in CTF it's the closest tank on the other team (so allies aren't targets).
func (w *World) nearestEnemy(self int) int {
	sight := w.Tanks[self].aiSight // 0 = unlimited (high tiers see the whole map)
	best, bestD := -1, math.MaxFloat64
	for j := range w.Tanks {
		t := &w.Tanks[j]
		if j == self || t.Dead || t.gone || t.cloakT > 0 {
			continue // cloaked tanks can't be targeted by bots
		}
		if w.rules().Teams == 2 && t.Team == w.Tanks[self].Team {
			continue
		}
		d := t.Pos.Sub(w.Tanks[self].Pos)
		dd := d.X*d.X + d.Z*d.Z
		if sight > 0 && dd > sight*sight {
			continue // beyond this bot's sight: it doesn't notice you
		}
		if dd < bestD {
			bestD, best = dd, j
		}
	}
	return best
}

// hurt applies dmg to tank ti and handles death bookkeeping (respawn timer,
// deaths, buff clearing, Survival lives, CTF flag drop, kill credit). owner is
// the firing tank index for kill credit; <0 means no credit (e.g. a turret or
// a hazard killed them). Caller has already checked the tank is vulnerable.
// shieldFracOf is the 0..1 shield gauge for the HUD/render: the minotaur's
// barrier charge, or the elephant's regenerating buffer.
func shieldFracOf(t *Tank) float64 {
	switch t.body {
	case BodyMinotaur:
		return t.shieldHP / minoShieldMax
	case BodyElephant:
		return t.bufferHP / elephantBufferMax
	}
	return 0
}

func (w *World) hurt(ti, dmg, owner int, cause KillCause) {
	t := &w.Tanks[ti]
	// EffDamageDown: the shooter is weakened, so trim everything it deals (direct
	// hits and DoT ticks alike, since DoT routes through hurt too).
	if owner >= 0 && owner < len(w.Tanks) && w.Tanks[owner].dmgDownT > 0 {
		dmg = int(float64(dmg) * (1 - w.Tanks[owner].dmgDownMag))
	}
	// A regenerating shield buffer (elephant) soaks damage from any direction
	// first; only the overflow reaches HP, and the hit pauses the buffer's regen.
	if t.bufferHP > 0 && dmg > 0 {
		t.hitFlash, t.regenPause = tankHitFlash, regenHitPause
		if float64(dmg) <= t.bufferHP {
			t.bufferHP -= float64(dmg)
			return // fully soaked
		}
		dmg -= int(t.bufferHP)
		t.bufferHP = 0
	}
	if owner >= 0 && owner != ti && owner < len(w.Tanks) { // credit effective damage (no overkill)
		eff := dmg
		if t.HP > 0 && eff > t.HP {
			eff = t.HP
		}
		if eff > 0 {
			w.Tanks[owner].dmgDealt += eff
		}
	}
	t.HP -= dmg
	t.hitFlash = tankHitFlash
	t.regenPause = regenHitPause // passive regen waits out the fight
	if t.HP > 0 {
		return
	}
	t.Dead = true
	t.respawn = respawnDelay
	if !t.Bot {
		t.respawn = playerRespawnDelay // longer wait so the human's kill-cam replay can play
	}
	t.Deaths++
	w.bus = append(w.bus, Signal{Name: "killed", Source: -1, Subject: ti, Other: owner}) // subject=victim, other=killer
	t.shieldT, t.rapidT, t.cloakT = 0, 0, 0                                              // buffs die with you
	t.shellT, t.boostT, t.dotT, t.dotDebt = 0, 0, 0, 0
	t.shieldUp, t.shieldBroken = false, 0
	t.shieldHP, t.bufferHP, t.hookT = 0, 0, 0
	if t.body == BodyMinotaur {
		t.shieldHP = minoShieldMax // the barrier comes back fresh on respawn
	}
	if t.body == BodyElephant {
		t.bufferHP = elephantBufferMax // the shield buffer comes back fresh on respawn
	}
	if r := w.rules(); r.Lives > 0 && !(r.Bots == BotWaves && t.Bot) {
		t.lives-- // wave bots don't burn lives; humans (and elimination bots) do
	}
	// Team modes: drop any carried flag where the carrier fell.
	if w.rules().Objective == ObjTeamFlags && t.Carrying >= 0 {
		f := &w.flags[t.Carrying]
		f.Carrier, f.atHome = -1, false
		f.Pos, f.dropTimer = V3{t.Pos.X, 0, t.Pos.Z}, flagReturnTime
		t.Carrying = -1
	}
	killer := -1
	if owner >= 0 && owner < len(w.Tanks) && owner != ti {
		w.Tanks[owner].Kills++
		killer = owner
		if w.Tanks[owner].pounceT > 0 {
			w.Tanks[owner].cooldown2 = 0 // POUNCE kill refunds the dash (Genji-style chain)
		}
	}
	w.kills = append(w.kills, KillEvent{Killer: killer, Victim: ti, Cause: cause})
}

// shotHitsEntity reports whether a projectile is absorbed by a map entity: a
// solid entity blocks it, and a destructible one also takes damage (and is
// destroyed at 0 HP). Low entities the shot flies over (height check, like
// hitObstacle) don't absorb it.
func (w *World) shotHitsEntity(s *Projectile) bool {
	for i := range w.entities {
		e := &w.entities[i]
		if e.Dead || (!e.Solid && e.Destruct == nil) {
			continue
		}
		if math.Abs(s.Pos.X-e.Pos.X) >= e.Half.X || math.Abs(s.Pos.Z-e.Pos.Z) >= e.Half.Z {
			continue
		}
		if s.Pos.Y >= e.Pos.Y+e.Half.Y { // flew over the top
			continue
		}
		if e.Destruct != nil {
			dmg := s.dmg
			if dmg <= 0 {
				dmg = projDmg
			}
			e.HP -= dmg
			if e.HP <= 0 {
				w.destroyEntity(e)
			}
		}
		return true // absorbed (blocked, and damaged if destructible)
	}
	return false
}

// destroyEntity marks an entity destroyed and arms its respawn timer if it has
// the Respawn trait (otherwise it stays gone for the match).
func (w *World) destroyEntity(e *Entity) {
	e.Dead, e.HP = true, 0
	if e.Respawn != nil {
		e.respawnT = e.Respawn.Delay
	}
	for i := range w.entities { // self-scoped signal: only this entity (+ director)
		if &w.entities[i] == e {
			w.emit("destroyed", i, -1)
			break
		}
	}
}

func (w *World) stepProjectiles(dt float64) {
	live := w.Shots[:0]
	for _, s := range w.Shots {
		if s.fx { // visual-only explosion debris: drift and fade, never collide
			s.vel.Y -= 14 * dt
			s.Pos = s.Pos.Add(V3{s.vel.X * dt, s.vel.Y * dt, s.vel.Z * dt})
			if s.life -= dt; s.life > 0 {
				live = append(live, s)
			}
			continue
		}
		if s.mine { // stationary: arm, then trigger on a nearby foe
			s.life -= dt
			if s.armT > 0 {
				s.armT -= dt
			}
			if s.life <= 0 {
				continue
			}
			if s.armT <= 0 && w.foeNear(&s, mineTrigger) {
				w.shotImpact(&s, s.Pos, -1)
				continue
			}
			live = append(live, s)
			continue
		}
		if s.grav > 0 { // lobbed: gravity arc
			s.vel.Y -= s.grav * dt
		}
		s.Pos = s.Pos.Add(V3{s.vel.X * dt, s.vel.Y * dt, s.vel.Z * dt})
		s.life -= dt
		if s.life <= 0 || math.Abs(s.Pos.X) > w.half()+0.6 || math.Abs(s.Pos.Z) > w.half()+0.6 {
			continue
		}
		if s.grav > 0 && s.Pos.Y <= w.ground(s.Pos.X, s.Pos.Z, s.Pos.Y) { // grenade hit the deck
			w.shotImpact(&s, s.Pos, -1)
			continue
		}
		if w.hitObstacle(s.Pos) { // tall cover blocks shots; low cover they fly over
			if s.blast > 0 {
				w.detonate(&s, s.Pos)
			}
			continue
		}
		if w.shotHitsEntity(&s) { // solid entities block; destructibles take damage
			if s.blast > 0 {
				w.detonate(&s, s.Pos)
			}
			continue
		}
		hit := false
		for ti := range w.Tanks {
			if !w.shotCanAffect(&s, ti) {
				continue
			}
			t := &w.Tanks[ti]
			sz := BodySizeScale(t.body)
			dx, dz := t.Pos.X-s.Pos.X, t.Pos.Z-s.Pos.Z
			// Height-aware: the shot must also be within the tank's body span, so
			// elevation matters (shoot over cover, or miss high). The window spans
			// from just below the feet to above the turret, scaled by vehicle size.
			dyLow, dyHigh := t.Pos.Y-0.3, t.Pos.Y+tankBodyTop*t.veh().Scale*sz
			r := hitRadius * sz
			if dx*dx+dz*dz < r*r && s.Pos.Y >= dyLow && s.Pos.Y <= dyHigh {
				w.shotImpact(&s, s.Pos, ti)
				hit = true
				break
			}
		}
		if !hit {
			live = append(live, s)
		}
	}
	if len(w.spawnQ) > 0 { // explosion FX queued during the step
		live = append(live, w.spawnQ...)
		w.spawnQ = w.spawnQ[:0]
	}
	w.Shots = live
}

// shotCanAffect reports whether projectile s may apply its effect to tank ti
// (alive, not the owner, friend/foe per Affects, shield/guard immunity except a
// buster). Shared by direct hits, blast AoE, and mine triggers.
func (w *World) shotCanAffect(s *Projectile, ti int) bool {
	t := &w.Tanks[ti]
	if ti == s.owner || t.Dead || t.gone {
		return false
	}
	teammate := w.rules().Teams == 2 && s.owner >= 0 && s.owner < len(w.Tanks) && w.Tanks[s.owner].Team == t.Team
	if s.affects == TargetAllies && !teammate && s.dmg <= 0 {
		return false // pure support: teammates only (a stinging medic bolt passes)
	}
	if s.affects == TargetFoes && teammate { // damage/debuffs: no friendly fire
		return false
	}
	// Shields/spawn-guard block anything that would harm; healing an ally through
	// their shield is fine.
	if !(s.affects == TargetAllies && teammate) && (t.guard > 0 || t.shieldT > 0 || t.shellT > 0) && s.eff != EffShieldBust {
		return false
	}
	if t.shellT > 0 && s.eff == EffShieldBust {
		return false // the shell is armor, not a buff - a buster can't strip it
	}
	return true
}

// shotImpact resolves a hit at point p: a blast weapon splashes everyone in range
// (detonate); a direct weapon affects just the struck tank (direct >= 0).
func (w *World) shotImpact(s *Projectile, p V3, direct int) {
	if s.blast > 0 {
		w.detonate(s, p)
		return
	}
	if direct >= 0 {
		if s.owner >= 0 && s.owner < len(w.Tanks) {
			w.Tanks[s.owner].shotsHit++ // direct hit (bolt/beam); blast counted in detonate
		}
		w.applyShotHit(s, direct)
	}
}

// detonate applies a blast weapon's effect to every eligible tank within its
// radius of the impact point, shoves them outward (for damage blasts), and emits
// a burst of visual debris.
func (w *World) detonate(s *Projectile, at V3) {
	w.spawnBlastFX(at)
	connected := false
	for ti := range w.Tanks {
		if !w.shotCanAffect(s, ti) {
			continue
		}
		t := &w.Tanks[ti]
		dx, dz := t.Pos.X-at.X, t.Pos.Z-at.Z
		d2 := dx*dx + dz*dz
		if d2 > s.blast*s.blast {
			continue
		}
		connected = true
		w.applyShotHit(s, ti)
		if s.eff == EffDamage { // damage blasts also shove outward, strongest at the center
			dist := math.Sqrt(d2)
			if push := blastPush * (1 - dist/s.blast); push > 0 && dist > 0.01 {
				t.Pos.X += dx / dist * push
				t.Pos.Z += dz / dist * push
				clampArena(&t.Pos, w.half())
				w.collide(&t.Pos)
			}
		}
	}
	if s.eff == EffDamage { // splash destructible map entities (turrets, breakable walls)
		dmg := s.dmg
		if dmg <= 0 {
			dmg = projDmg
		}
		for i := range w.entities {
			e := &w.entities[i]
			if e.Dead || e.Destruct == nil {
				continue
			}
			dx, dz := e.Pos.X-at.X, e.Pos.Z-at.Z
			if dx*dx+dz*dz <= s.blast*s.blast {
				if e.HP -= dmg; e.HP <= 0 {
					w.destroyEntity(e)
				}
			}
		}
	}
	if connected && s.owner >= 0 && s.owner < len(w.Tanks) {
		w.Tanks[s.owner].shotsHit++ // a blast counts as one connecting shot (accuracy)
	}
}

// spawnBlastFX queues a short-lived burst of debris sparks at the impact point
// (queued, not appended directly, so we don't grow w.Shots mid-iteration).
func (w *World) spawnBlastFX(at V3) {
	for i := 0; i < 9; i++ {
		a := rand.Float64() * 2 * math.Pi
		sp := 4 + rand.Float64()*5
		w.spawnQ = append(w.spawnQ, Projectile{
			Pos:   V3{X: at.X, Y: at.Y + 0.4, Z: at.Z},
			vel:   V3{X: math.Cos(a) * sp, Y: 3 + rand.Float64()*3, Z: math.Sin(a) * sp},
			life:  0.22 + rand.Float64()*0.16,
			owner: -1,
			fx:    true,
			vis:   VisSpark,
		})
	}
}

// foeNear reports whether any tank a mine can affect is within radius r.
func (w *World) foeNear(s *Projectile, r float64) bool {
	for ti := range w.Tanks {
		if !w.shotCanAffect(s, ti) {
			continue
		}
		t := &w.Tanks[ti]
		dx, dz := t.Pos.X-s.Pos.X, t.Pos.Z-s.Pos.Z
		if dx*dx+dz*dz <= r*r {
			return true
		}
	}
	return false
}

// absorbedByShield handles a minotaur's deployed frontal barrier: a harmful hit
// arriving from within the barrier's frontal arc is soaked by the shield's HP
// instead of the tank. The hit drains the barrier; when it runs the barrier dry
// it shatters (lowers + enters its redeploy cooldown) and any overkill leaks
// through as real damage. Returns true if the hit was (fully or partly) eaten by
// the barrier - callers then skip their normal effect application. Friendly
// effects (heals/buffs) and hits from the sides/back are never absorbed.
func (w *World) absorbedByShield(s *Projectile, ti int) bool {
	t := &w.Tanks[ti]
	if t.body != BodyMinotaur || !t.shieldUp || t.shieldHP <= 0 || s.affects == TargetAllies {
		return false
	}
	// Source of the hit: the firer (or, for environment fire, the shot itself).
	src := s.Pos
	if s.owner >= 0 && s.owner < len(w.Tanks) {
		src = w.Tanks[s.owner].Pos
	}
	dx, dz := src.X-t.Pos.X, src.Z-t.Pos.Z
	n := math.Hypot(dx, dz)
	if n < 0.01 {
		return false // right on top of the minotaur: no facing to block with
	}
	f := fwd(t.HullYaw)
	if (f.X*dx+f.Z*dz)/n < minoShieldArc {
		return false // came from a flank or behind: the barrier doesn't cover it
	}
	cost := float64(s.dmg)
	if s.eff == EffDamage && cost <= 0 {
		cost = projDmg
	}
	if cost <= 0 {
		cost = 8 // a debuff/utility hit still chips the barrier
	}
	if cost < t.shieldHP {
		t.shieldHP -= cost
		return true
	}
	// The barrier shatters; surplus damage carries through to the minotaur.
	leak := cost - t.shieldHP
	t.shieldHP, t.shieldUp, t.shieldBroken = 0, false, minoShieldBreakCD
	if (s.eff == EffDamage || s.dmg > 0) && leak >= 1 {
		w.hurt(ti, int(leak), s.owner, s.cause)
	}
	return true
}

// applyShotHit resolves a projectile striking tank ti by its effect payload. W1
// handles EffDamage (ordinary damage); the effect palette (heal/slow/knockback/
// shield-bust) lands in W2, dispatched off the same switch.
func (w *World) applyShotHit(s *Projectile, ti int) {
	if w.absorbedByShield(s, ti) {
		return
	}
	t := &w.Tanks[ti]
	switch s.eff {
	case EffHeal:
		teammate := w.rules().Teams == 2 && s.owner >= 0 && s.owner < len(w.Tanks) && w.Tanks[s.owner].Team == t.Team
		if !teammate && s.dmg > 0 {
			// The medic bolt stings anyone who isn't kin - so a butterfly is
			// never a zero-damage character (it just isn't much of one).
			w.hurt(ti, s.dmg, s.owner, CauseCannon)
			return
		}
		if max := t.veh().MaxHP; t.HP < max {
			healed := int(s.mag)
			if t.HP+healed > max {
				healed = max - t.HP
			}
			t.HP += healed
			t.healFlash = healFlashDur // mend halo (drawn above the tank)
			if s.owner >= 0 && s.owner != ti && s.owner < len(w.Tanks) {
				w.Tanks[s.owner].healDone += healed // support: HP restored to allies
			}
		}
		t.hitFlash = tankHitFlash // brief blink as feedback
	case EffSlow:
		teammate := w.rules().Teams == 2 && s.owner >= 0 && s.owner < len(w.Tanks) && w.Tanks[s.owner].Team == t.Team
		if !teammate && s.dmg > 0 { // chip on top of the slow (octopod SLOWER, serpent SPRAY); pure-utility slows carry Damage 0
			w.hurt(ti, s.dmg, s.owner, s.cause)
		}
		t.slowT, t.slowMag = s.dur, clampF01(s.mag)
		t.hitFlash = tankHitFlash
	case EffDamageDown:
		t.dmgDownT, t.dmgDownMag = s.dur, clampF01(s.mag)
		t.hitFlash = tankHitFlash
	case EffSlip:
		t.slipT = s.dur
		t.hitFlash = tankHitFlash
	case EffSpeed:
		teammate := w.rules().Teams == 2 && s.owner >= 0 && s.owner < len(w.Tanks) && w.Tanks[s.owner].Team == t.Team
		if !teammate && s.dmg > 0 { // the swift bolt stings anyone who isn't kin (medic pattern)
			w.hurt(ti, s.dmg, s.owner, s.cause)
			return
		}
		t.boostT, t.boostMag = s.dur, s.mag
		t.hitFlash = tankHitFlash
	case EffShield:
		if t.shieldT < s.dur {
			t.shieldT = s.dur
		}
		t.hitFlash = tankHitFlash
	case EffPoison, EffDrain, EffBleed:
		if s.dmg > 0 { // the bite/burn/scratch itself, on top of the lingering DoT
			w.hurt(ti, s.dmg, s.owner, s.cause)
			if t.Dead {
				return // no DoT on a corpse
			}
		}
		t.dotT, t.dotPS, t.dotFrom = s.dur, s.mag, s.owner
		t.dotLeech = s.eff == EffDrain
		if t.dotCause = s.cause; t.dotCause == CauseCannon {
			t.dotCause = CausePoison
		}
		t.hitFlash = tankHitFlash
	case EffPull:
		// Reel the target in toward the shooter, ending ~Mag units away (point
		// blank, in tusk reach). The elephant's trunk hook.
		if s.owner >= 0 && s.owner < len(w.Tanks) {
			o := &w.Tanks[s.owner]
			dx, dz := t.Pos.X-o.Pos.X, t.Pos.Z-o.Pos.Z
			d := math.Hypot(dx, dz)
			if d > 0.01 {
				end := s.mag
				if end < 0.5 {
					end = 0.5
				}
				t.Pos.X = o.Pos.X + dx/d*end
				t.Pos.Z = o.Pos.Z + dz/d*end
				clampArena(&t.Pos, w.half())
				w.collide(&t.Pos)
			}
		}
		if s.dmg > 0 {
			w.hurt(ti, s.dmg, s.owner, s.cause)
		}
		t.hitFlash = tankHitFlash
	case EffKnockback:
		n := math.Hypot(s.vel.X, s.vel.Z)
		if n > 0 {
			t.Pos.X += s.vel.X / n * s.mag
			t.Pos.Z += s.vel.Z / n * s.mag
			clampArena(&t.Pos, w.half())
			w.collide(&t.Pos)
		}
		t.hitFlash = tankHitFlash
	case EffShieldBust:
		t.shieldT, t.rapidT = 0, 0
		t.hitFlash = tankHitFlash
	default: // EffDamage
		dmg := s.dmg
		if dmg <= 0 {
			dmg = projDmg
		}
		cause := s.cause // melee/cone strikes label themselves (pound, tusks)
		if s.owner < 0 {
			cause = CauseTurret // entity-fired shots have no tank owner
		}
		w.hurt(ti, dmg, s.owner, cause)
	}
}

func clampF01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 0.9 {
		return 0.9 // never a total freeze
	}
	return v
}

// blocked reports whether a point is inside an obstacle (inflated by tank
// radius) or outside the arena — used by bot whisker avoidance.
func (w *World) blocked(p V3) bool {
	const rad = 1.1
	if math.Abs(p.X) > w.half() || math.Abs(p.Z) > w.half() {
		return true
	}
	for _, b := range w.collidables() { // obstacles + alive solid entities
		if p.Y > b.Pos.Y+b.Half.Y+0.1 {
			continue
		}
		if math.Abs(p.X-b.Pos.X) < b.Half.X+rad && math.Abs(p.Z-b.Pos.Z) < b.Half.Z+rad {
			return true
		}
	}
	// A ramp's solid wedge blocks too: inside the footprint and below the surface
	// (with a step's grace) is the wall under the incline, not walkable ground.
	// Without this, bot avoidance and pathing don't see ramp sides and drive into
	// them (the collision now stops them; this stops them aiming there at all).
	for _, r := range w.ActiveMap().Ramps {
		cx := math.Max(r.Pos.X-r.Half.X, math.Min(p.X, r.Pos.X+r.Half.X))
		cz := math.Max(r.Pos.Z-r.Half.Z, math.Min(p.Z, r.Pos.Z+r.Half.Z))
		if math.Abs(p.X-r.Pos.X) >= r.Half.X+rad || math.Abs(p.Z-r.Pos.Z) >= r.Half.Z+rad {
			continue
		}
		if rh, ok := rampHeight(r, cx, cz); ok && p.Y < rh-stepUp {
			return true
		}
	}
	return false
}

// hitObstacle reports whether a projectile position is inside a solid obstacle
// (so tall obstacles block shots; ones shorter than the shot height don't).
func (w *World) hitObstacle(p V3) bool {
	for _, b := range w.ActiveMap().Obstacles {
		if math.Abs(p.X-b.Pos.X) < b.Half.X && math.Abs(p.Z-b.Pos.Z) < b.Half.Z && p.Y < b.Pos.Y+b.Half.Y {
			return true
		}
	}
	return false
}

func (w *World) respawns(dt float64) {
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if !t.Dead || t.gone {
			continue
		}
		r := w.rules()
		if r.Bots == BotWaves && t.Bot {
			continue // wave bots return via spawnWave, not auto-respawn
		}
		if r.Lives > 0 && t.lives <= 0 {
			continue // out of lives: stay dead (survival, elimination)
		}
		t.respawn -= dt
		if t.respawn <= 0 {
			t.Dead = false
			t.HP = t.veh().MaxHP
			t.ammo = t.veh().AmmoMax
			t.wp2Used, t.wp2RegenT, t.pounceT = 0, 0, 0
			t.TurretYaw = 0
			t.guard = spawnGuardTime
			t.Pos = w.spawnPoint(i)
			t.HullYaw = w.faceTarget(i)
		}
	}
}

func (w *World) spawnPoint(self int) V3 {
	// Team modes: spawn near your own base (jittered), so teams hold opposite ends.
	if w.rules().Teams == 2 && self >= 0 && self < len(w.Tanks) && w.Tanks[self].Team >= 0 {
		base := w.ctfBase(w.Tanks[self].Team)
		x := base.X + (rand.Float64()*2-1)*4
		z := base.Z + (rand.Float64()*2-1)*2.5
		lim := w.half() - 1
		px := math.Max(-lim, math.Min(lim, x))
		pz := math.Max(-lim, math.Min(lim, z))
		p := V3{px, 0, pz}
		w.collide(&p)
		return p
	}
	clear := func(x, z float64) bool {
		for j := range w.Tanks {
			if j == self || w.Tanks[j].Dead || w.Tanks[j].gone {
				continue
			}
			dx, dz := w.Tanks[j].Pos.X-x, w.Tanks[j].Pos.Z-z
			if dx*dx+dz*dz < 64 {
				return false
			}
		}
		return true
	}
	lim := w.half() - 1
	// Prefer the map's authored spawn points (clear of other tanks).
	if sp := w.ActiveMap().Spawns; len(sp) > 0 {
		for _, k := range rand.Perm(len(sp)) {
			if clear(sp[k].X, sp[k].Z) {
				return sp[k]
			}
		}
		// More tanks than clear spawns: jitter around one so they don't stack.
		k := rand.Intn(len(sp))
		p := V3{
			math.Max(-lim, math.Min(lim, sp[k].X+(rand.Float64()*2-1)*4)),
			0,
			math.Max(-lim, math.Min(lim, sp[k].Z+(rand.Float64()*2-1)*4)),
		}
		w.collide(&p)
		return p
	}
	for tries := 0; tries < 24; tries++ {
		x := (rand.Float64()*2 - 1) * lim
		z := (rand.Float64()*2 - 1) * lim
		if clear(x, z) {
			return V3{x, 0, z}
		}
	}
	return V3{(rand.Float64()*2 - 1) * lim, 0, (rand.Float64()*2 - 1) * lim} // last resort: random, not origin
}

func (w *World) faceTarget(self int) float64 {
	best, bestD := -1, math.MaxFloat64
	for j := range w.Tanks {
		t := &w.Tanks[j]
		if j == self || t.Dead || t.gone {
			continue
		}
		d := t.Pos.Sub(w.Tanks[self].Pos)
		if dd := d.X*d.X + d.Z*d.Z; dd < bestD {
			bestD, best = dd, j
		}
	}
	var d V3
	if best >= 0 {
		d = w.Tanks[best].Pos.Sub(w.Tanks[self].Pos)
	} else {
		d = V3{}.Sub(w.Tanks[self].Pos)
	}
	return math.Atan2(d.X, d.Z)
}

// Snapshot returns the flat, renderable view of the world: live tanks (gone
// slots omitted), projectile positions, and untaken flag positions. Used both to
// render locally and to build STATE on the wire.
func (w *World) Snapshot() ([]TankSnap, []ShotSnap, []FlagSnap, []PickupSnap) {
	ts := make([]TankSnap, 0, len(w.Tanks))
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone {
			continue
		}
		reload := t.cooldown / t.veh().FireDelay
		if reload < 0 {
			reload = 0
		} else if reload > 1 {
			reload = 1
		}
		ammoFrac := 0.0
		if mx := t.veh().AmmoMax; mx > 0 {
			if ammoFrac = t.ammo / mx; ammoFrac > 1 {
				ammoFrac = 1
			} else if ammoFrac < 0 {
				ammoFrac = 0
			}
		}
		// Secondary gauge: charge weapons report a pip stock, cooldown weapons
		// report a recharge fraction (mirrors the primary `reload` math).
		var reload2 float64
		var charges, maxCharges int
		if t.weapon2 > 0 && t.weapon2 < len(Weapons) {
			def := Weapons[t.weapon2]
			if def.Charges > 0 { // charge-stock weapon: pips are the gauge
				maxCharges = def.Charges
				if charges = def.Charges - t.wp2Used; charges < 0 {
					charges = 0
				}
				reload2 = 0
			} else { // cooldown weapon: recharge fraction (0 = ready)
				gap := def.Cooldown
				if gap <= 0 {
					gap = t.veh().FireDelay
				}
				if gap > 0 {
					if reload2 = t.cooldown2 / gap; reload2 < 0 {
						reload2 = 0
					} else if reload2 > 1 {
						reload2 = 1
					}
				}
			}
		}
		ts = append(ts, TankSnap{
			ID: i, Pos: t.Pos, HullYaw: t.HullYaw, TurretYaw: t.TurretYaw, TurretPitch: t.TurretPitch,
			HP: t.HP, Color: t.Color, Name: t.Name, Dead: t.Dead, Bot: t.Bot,
			Shield: t.guard > 0 || t.shieldT > 0, Hit: t.hitFlash > 0,
			Cloak: t.cloakT > 0, Rapid: t.rapidT > 0, Shell: t.shellT > 0,
			Burning:  t.dotT > 0 && t.dotCause == CauseFire,
			Poisoned: t.dotT > 0 && t.dotCause == CausePoison,
			Bleeding: t.dotT > 0 && t.dotCause == CauseBleed,
			Healing:  t.healFlash > 0,
			ShieldUp: t.shieldUp, ShieldFrac: shieldFracOf(t),
			Body: t.body, ShotsFired: t.shotsFired, ShotsHit: t.shotsHit, Pickups: t.pickups,
			DmgDealt: t.dmgDealt, HealDone: t.healDone,
			Lives: t.lives, Team: t.Team, Carrying: t.Carrying >= 0,
			Kills: t.Kills, Deaths: t.Deaths, RespawnIn: t.respawn, Reload: reload, Ammo: ammoFrac,
			Reload2: reload2, Charges: charges, MaxCharges: maxCharges, Slip: t.slipT > 0,
			HoldScore: t.holdScore,
		})
	}
	sh := make([]ShotSnap, len(w.Shots))
	for i := range w.Shots {
		sh[i] = ShotSnap{Pos: w.Shots[i].Pos, Vis: w.Shots[i].vis, Owner: w.Shots[i].owner}
	}
	var fl []FlagSnap
	for i := range w.flags {
		f := &w.flags[i]
		if f.Taken { // Flag Run: collected flags vanish
			continue
		}
		fl = append(fl, FlagSnap{Pos: f.Pos, Home: f.Home, Team: f.Team, Carried: f.Carrier >= 0})
	}
	var pk []PickupSnap
	for i := range w.pickups {
		pk = append(pk, PickupSnap{Pos: w.pickups[i].Pos, Kind: w.pickups[i].Kind, Weapon: w.pickups[i].Weapon})
	}
	return ts, sh, fl, pk
}
