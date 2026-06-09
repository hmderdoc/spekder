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

//go:embed maps/*.json
var mapFS embed.FS

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

	// --- runtime instance state (set in the World copy, not authored) ---
	HP       int     // current hit points (Destruct); 0/unused otherwise
	Dead     bool    // destroyed; awaiting respawn or gone for good
	cooldown float64 // turret fire / teleport debounce timer
	respawnT float64 // sec until respawn while Dead (Respawn trait)
}

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
const SchemaVersion = 3 // v3 added typed pickup spots (pickupSpots)

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
	Rules     *MapRules // optional per-map victory conditions (nil = implied by objectives)
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
	Mode      int     // -1 = auto (NaturalMode); else a mode index
	TimeLimit float64 // -1 = default; 0 = endless; >0 = match seconds
	Target    int     // -1 = default; else the win count (frags/captures/hold-points)
	Lives     int     // -1 = default; 0 = infinite; >0 = lives per tank
}

// NewEntities returns a fresh runtime copy of the map's authored entities with
// instance state initialized (HP from Destruct, alive). Called at match start.
// Trait pointers are shared with the template - they are read-only params; only
// the value fields (HP/Dead/cooldown/respawnT/Yaw) are mutated at runtime.
func (m Map) NewEntities() []Entity {
	out := make([]Entity, len(m.Entities))
	for i, e := range m.Entities {
		out[i] = e // value copy; trait pointers shared, fine (params are read-only)
		out[i].Dead, out[i].cooldown, out[i].respawnT = false, 0, 0
		if e.Destruct != nil {
			out[i].HP = e.Destruct.MaxHP
		}
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

// Maps is the indexed, embedded map set (shared by server and door; the wire
// syncs the active map by index, so both must share the same build).
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
type jentity struct {
	Kind     string     `json:"kind"`
	Pos      [3]float64 `json:"pos"`
	Half     [3]float64 `json:"half"`
	Color    [3]float64 `json:"color"`
	Yaw      float64    `json:"yaw"`
	Solid    bool       `json:"solid"`
	Weapon   int        `json:"weapon,omitempty"`
	Turret   *jturret   `json:"turret"`
	Hazard   *jhazard   `json:"hazard"`
	Teleport *jteleport `json:"teleport"`
	Destruct *jdestruct `json:"destruct"`
	Respawn  *jrespawn  `json:"respawn"`
	Bounce   *jbounce   `json:"bounce"`
	Flag     *jflag     `json:"flag"`
	Zone     *jzone     `json:"zone"`
}
type jmap struct {
	Version   int          `json:"version"`
	Name      string       `json:"name"`
	Size      float64      `json:"size"`
	Obstacles []jbox       `json:"obstacles"`
	Ramps     []jramp      `json:"ramps"`
	Scenery   []jprop      `json:"scenery"`
	Spawns    [][2]float64 `json:"spawns"`
	Pickups   [][2]float64 `json:"pickups,omitempty"`     // v1/v2 legacy: untyped spots (read-only)
	PickSpots []jpickup    `json:"pickupSpots,omitempty"` // v3: typed pickup spots
	Entities  []jentity    `json:"entities"`
	Rules     *jrules      `json:"rules,omitempty"`
}

// jpickup is a v3 typed pickup spot. Kind < 0 = any (random); Weapon is the
// granted weapon when Kind is the weapon-drop kind.
type jpickup struct {
	Pos    [2]float64 `json:"pos"`
	Kind   int        `json:"kind"`
	Weapon int        `json:"weapon,omitempty"`
}

type jrules struct {
	Mode      int     `json:"mode"`
	TimeLimit float64 `json:"timeLimit"`
	Target    int     `json:"target"`
	Lives     int     `json:"lives"`
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
		m.Rules = &MapRules{Mode: jm.Rules.Mode, TimeLimit: jm.Rules.TimeLimit, Target: jm.Rules.Target, Lives: jm.Rules.Lives}
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
		jm.Rules = &jrules{Mode: m.Rules.Mode, TimeLimit: m.Rules.TimeLimit, Target: m.Rules.Target, Lives: m.Rules.Lives}
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
	projSpeed          = 24.0
	projLife           = 2.4
	projDmg            = 34
	tankMaxHP          = 100
	hitRadius          = 1.15
	tankBodyTop        = 1.9 // top of a tank's hittable body above its feet (×vehicle scale)

	ArenaA = 22.0 // default playfield half-extent (maps may override via Size)

	respawnDelay   = 3.0
	spawnGuardTime = 1.6
	EyeHeight      = 1.35

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
	lobbyTime     = 14.0  // sec of mode-vote lobby between matches (server only)
	flagCount     = 8     // flags scattered in Flag Run
	flagPickupRad = 1.9   // how close you must drive to grab a flag
	tankHitFlash  = 0.15  // sec a tank flashes white after taking a hit
	stepUp        = 0.6   // max ledge/step a tank can mount without jumping
	survivalLives = 3     // Survival: lives per human
	survivalPool  = 12    // Survival: bot pool size for waves

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
	{Name: "HUNTER", MaxHP: 100, Speed: 6.0, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1.0, AmmoMax: 8, AmmoRegen: 1.8,
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
		i = 1 // HUNTER default
	}
	return Vehicles[i]
}

// veh returns a tank's effective stats: its custom build if set, else its chassis.
func (t *Tank) veh() Vehicle {
	if t.custom != nil {
		return *t.custom
	}
	return veh(t.Vehicle)
}

// CustomStats are the point-buy tunable sim stats; the rest (AimTurn, Jump, Scale,
// body silhouette) come from the chosen chassis. Travels in HELLO so the server
// sims a custom build correctly while everyone renders the chassis by index.
type CustomStats struct {
	MaxHP                                          int
	Speed, HullTurn, FireDelay, AmmoMax, AmmoRegen float64
}

// MakeCustom builds a Vehicle from a chassis plus tuned stats. AimTurn scales with
// hull turn; Jump/Scale/Name/Desc are inherited from the chassis (render + feel).
func MakeCustom(chassis int, cs CustomStats) Vehicle {
	v := veh(chassis)
	v.MaxHP = cs.MaxHP
	v.Speed = cs.Speed
	v.HullTurn = cs.HullTurn
	v.AimTurn = cs.HullTurn * 0.75
	v.FireDelay = cs.FireDelay
	v.AmmoMax = cs.AmmoMax
	v.AmmoRegen = cs.AmmoRegen
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
	AimUp, AimDown                                                bool // elevate / depress the gun
	Recenter                                                      bool // snap turret to hull-forward + level
	Fire2                                                         bool // fire the secondary weapon (B)
	Vote                                                          int
}

type Tank struct {
	Pos         V3
	HullYaw     float64
	TurretYaw   float64 // relative to hull
	TurretPitch float64 // gun elevation: + = aim up, - = aim down (radians)
	HP          int
	Color       [3]float64
	Name        string   // display name: human handle, or a bot callsign
	Vehicle     int      // chassis index (body/scale/render); shared builtin table
	custom      *Vehicle // per-tank stat override (custom point-buy build); nil = use Vehicle
	body        int      // render body style (BodyTank/BodySpider/...); 0 = tank
	Bot         bool
	Dead        bool
	Kills       int
	Deaths      int

	cooldown  float64
	cooldown2 float64 // secondary-weapon recharge
	weapon2   int     // secondary weapon index (into Weapons); fired with the B key
	ammo      float64 // regenerating ammo pool (soft fire limit); max/regen per vehicle
	slowT     float64 // EffSlow remaining (sec)
	slowMag   float64 // EffSlow magnitude (fraction of speed removed)
	respawn   float64
	guard     float64
	vy        float64 // vertical velocity (jump/gravity)
	hitFlash  float64 // brief flash timer after taking damage
	vote      int     // lobby vote: mode index, or -1 for none
	lives     int     // Survival: respawns remaining (humans)
	Team      int     // CTF: 0 or 1 (-1 = none in non-team modes)
	Carrying  int     // CTF: index of the enemy flag being carried, or -1
	shieldT   float64 // power-up: invulnerability remaining (sec)
	rapidT    float64 // power-up: rapid-fire remaining (sec)
	cloakT    float64 // power-up: cloak/invisibility remaining (sec)
	gone      bool    // player left; slot inert and reusable

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

	// aim-assist lock (human players): kind 0 none / 1 tank / 2 entity, idx into
	// that slice; lockBreak accumulates sustained turn input to release the lock;
	// lockCool suppresses re-acquire after a break so a held turn carries you off.
	lockKind  int
	lockIdx   int
	lockBreak float64
	lockCool  float64
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
	Vehicle                int
	Body                   int // render body style (BodyTank/BodySpider/...)
	Lives                  int
	Team                   int  // CTF team (-1 in non-team modes)
	Carrying               bool // CTF: carrying an enemy flag
	Cloak                  bool // power-up: cloaked (hidden from enemies)
	Rapid                  bool // power-up: rapid-fire active
	RespawnIn              float64
	Reload                 float64 // 0 = ready to fire, ->1 = just fired
	Ammo                   float64 // regenerating ammo, 0..1 of capacity (HUD gauge)
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
}

// Shot visual kinds, carried to the renderer so each projectile draws distinctly.
const (
	VisBolt    byte = iota // straight bolt (cannon, slow, etc.)
	VisGrenade             // arcing lob
	VisMine                // dropped mine
	VisBeam                // hitscan beam segment
	VisSpark               // explosion debris
)

// ShotSnap is one projectile to draw: position + visual kind.
type ShotSnap struct {
	Pos V3
	Vis byte
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
	DeliverBolt Delivery = iota // straight projectile (today's shot)
	DeliverLob                  // arced/lobbed (grenade)        [W4]
	DeliverMine                 // dropped, proximity/timer fire [W4]
	DeliverBeam                 // hitscan (laser)               [W4]
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
	Glyph    byte // wire-cheap render hint                        [W4]
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
	wepCannon  = iota // default damage bolt
	wepSlower         // drags the target's speed
	wepMedic          // heals an ally
	wepKnocker        // shoves the target back
	wepBuster         // strips a target's shield
	wepGrenade        // lobbed, blast-radius damage
	wepMine           // dropped, proximity blast
	wepLaser          // hitscan beam
)

// Weapons is the built-in weapon palette. Referenced by index. CANNON preserves
// today's bolt; the rest carry effect payloads (resolved in applyShotHit) and/or
// delivery kinds (resolved in fireWeapon / stepProjectiles).
var Weapons = []WeaponDef{
	{Name: "CANNON", Delivery: DeliverBolt, Cost: 1, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: 'o'},
	{Name: "SLOWER", Delivery: DeliverBolt, Cooldown: 1.0, Cost: 2, Effect: Effect{Kind: EffSlow, Mag: 0.55, Dur: 2.5}, Affects: TargetFoes, Glyph: '~'},
	{Name: "MEDIC", Delivery: DeliverBolt, Cooldown: 1.2, Cost: 2, Effect: Effect{Kind: EffHeal, Mag: 25}, Affects: TargetAllies, Glyph: '+'},
	{Name: "KNOCKER", Delivery: DeliverBolt, Cooldown: 1.1, Cost: 2, Effect: Effect{Kind: EffKnockback, Mag: 4}, Affects: TargetFoes, Glyph: '*'},
	{Name: "BUSTER", Delivery: DeliverBolt, Cooldown: 1.5, Cost: 2, Effect: Effect{Kind: EffShieldBust}, Affects: TargetFoes, Glyph: 'x'},
	{Name: "GRENADE", Delivery: DeliverLob, Damage: 32, Speed: 20, Arc: lobGravity, Blast: 4, Cooldown: 1.3, Cost: 3, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: 'g'},
	{Name: "MINE", Delivery: DeliverMine, Damage: 45, Blast: 4, Cooldown: 2.0, Cost: 3, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: 'm'},
	{Name: "LASER", Delivery: DeliverBeam, Damage: 18, Life: 40, Cooldown: 0.5, Cost: 1, Effect: Effect{Kind: EffDamage}, Affects: TargetFoes, Glyph: '='},
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
		TeamScore: w.teamScore, WinnerTeam: w.winnerTeam,
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
}

// SetAimAssist toggles sticky aim assist for human players.
func (w *World) SetAimAssist(on bool) { w.assistAim = on }

// SetDifficulty sets the active bot profile; takes effect at the next match start
// (when bots re-roll their per-bot AI). Offline play sets this from the user's
// chosen tier; the arena server sets it from its config.
func (w *World) SetDifficulty(d Difficulty) { w.bots = ProfileFor(d) }

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
		out[i] = EntitySnap{HP: w.entities[i].HP, Dead: w.entities[i].Dead, Yaw: w.entities[i].Yaw, Pitch: w.entities[i].Pitch}
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
func (w *World) resetEntities() { w.entities = w.ActiveMap().NewEntities() }

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
func (w *World) collide(p *V3) { CollideBoxes(w.collidables(), p) }

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

// NewWorld creates a world for the given mode, seeded with numBots AI tanks
// (indices 0..numBots-1). Human players are added afterward with AddPlayer. The
// match starts in a countdown.
func NewWorld(numBots int, mode Mode) *World {
	w := &World{Mode: mode, Phase: PhaseCountdown, Timer: countdownTime, WinnerID: -1,
		bots: ProfileFor(DiffNormal), assistAim: true} // gentler default; setters override
	if len(Maps) > 0 {
		w.MapIdx = rand.Intn(len(Maps)) // start on a random map, not always the empty one
	}
	for b := 0; b < numBots; b++ {
		vi := rand.Intn(len(Vehicles))
		w.Tanks = append(w.Tanks, Tank{
			Bot: true, HP: veh(vi).MaxHP, ammo: veh(vi).AmmoMax, guard: spawnGuardTime, vote: -1, Vehicle: vi,
			body:  botBodies[rand.Intn(len(botBodies))], // mix in creatures, not just tanks
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
// index. color may be the zero value to auto-pick from PlayerPalette.
func (w *World) AddPlayer(color [3]float64, vehicle int, name string) int {
	return w.AddPlayerCustom(color, vehicle, name, nil, BodyTank)
}

// AddPlayerCustom adds a player whose sim stats come from a custom build; the
// vehicle arg is still the chassis (body/scale/render), and body is the render
// silhouette (BodyTank or a creature). custom == nil is a builtin.
func (w *World) AddPlayerCustom(color [3]float64, vehicle int, name string, custom *Vehicle, body int) int {
	color = w.freeColor(color) // honor the pick unless another player already wears it
	if name == "" {
		name = "PLAYER"
	}
	mk := func(i int) Tank {
		eff := veh(vehicle)
		if custom != nil {
			eff = *custom
		}
		t := Tank{HP: eff.MaxHP, ammo: eff.AmmoMax, Color: color, Name: name, guard: spawnGuardTime, vote: -1, Vehicle: vehicle, custom: custom, body: body, lives: survivalLives, Team: -1, Carrying: -1, weapon2: wepGrenade}
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
func (w *World) fire(owner int) { w.fireWeapon(owner, &Weapons[wepCannon], false) }

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
	}
	muzzle := V3{t.Pos.X + d.X*1.7, t.Pos.Y + EyeHeight + d.Y*1.7, t.Pos.Z + d.Z*1.7}
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
			dx, dz := tk.Pos.X-pt.X, tk.Pos.Z-pt.Z
			dyLow, dyHigh := tk.Pos.Y-0.3, tk.Pos.Y+tankBodyTop*veh(tk.Vehicle).Scale
			if dx*dx+dz*dz < hitRadius*hitRadius && pt.Y >= dyLow && pt.Y <= dyHigh {
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
	w.spawnBeamFX(muzzle, V3{X: muzzle.X + d.X*stopT, Y: muzzle.Y + d.Y*stopT, Z: muzzle.Z + d.Z*stopT})
}

// spawnBeamFX lays a short-lived line of segments from a to b (the beam visual).
func (w *World) spawnBeamFX(a, b V3) {
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
			Pos: V3{X: a.X + dx*f, Y: a.Y + dy*f, Z: a.Z + dz*f}, life: 0.12, owner: -1, fx: true, vis: VisBeam,
		})
	}
}

// Update advances the world by dt. inputs maps a human tank index to its held
// buttons this tick (absent => idle); bots are driven by AI. The match
// lifecycle (countdown -> active -> ended -> next countdown) gates simulation.
func (w *World) Update(dt float64, inputs map[int]Input) {
	w.kills = w.kills[:0] // kill feed is per-tick; Match() reads this tick's kills
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
		}
		w.Phase, w.Timer = PhaseLobby, lobbyTime
		return
	}
	w.startCountdown(w.Mode)
}

func (w *World) startCountdown(mode Mode) {
	if len(Maps) > 1 && !w.pinned {
		w.MapIdx = (w.MapIdx + 1) % len(Maps) // rotate the map each match (offline/solo)
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
func (w *World) pickNextPairing() (mapIdx int, mode Mode) {
	if len(Maps) == 0 {
		return 0, w.Mode
	}
	best, bestN := -1, 0
	for mi := 0; mi < len(Maps); mi++ {
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
		best = (w.MapIdx + 1) % len(Maps) // no votes: rotate
	}
	return best, EffectiveMode(Maps[best])
}

func (w *World) applyVotes(inputs map[int]Input) {
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.Bot || t.gone {
			continue
		}
		if in, ok := inputs[i]; ok {
			t.vote = in.Vote
		}
	}
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
		t.Kills, t.Deaths, t.Carrying = 0, 0, -1
		t.HP, t.Dead, t.vy = t.veh().MaxHP, false, 0
		t.ammo = t.veh().AmmoMax
		t.shieldT, t.rapidT, t.cloakT = 0, 0, 0
		t.guard = spawnGuardTime
		t.holdScore = 0
		t.Pos = w.spawnPoint(i)
		t.HullYaw = w.faceTarget(i)
		t.TurretYaw, t.TurretPitch = 0, 0
		if r.Lives > 0 && !(r.Bots == BotWaves && t.Bot) {
			t.lives = r.Lives // wave bots are exempt (infinite); everyone else gets lives
		}
		if t.Bot {
			w.rollBotAI(i) // re-roll per-bot variation each match
		}
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
func (w *World) assignTeams() {
	var count [2]int
	// Humans go on team 0 so callers play together.
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || t.Bot {
			continue
		}
		t.Team = 0
		count[0]++
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
		w.zones = append(w.zones, Zone{Pos: V3{e.Pos.X, 0, e.Pos.Z}, Half: e.Half, Cap: cap, Owner: -1, cont: -1})
	}
	if len(w.zones) == 0 {
		w.zones = append(w.zones, Zone{Pos: V3{}, Half: V3{X: zoneFallbackR, Y: 1, Z: zoneFallbackR}, Cap: zoneCaptureTime, Owner: -1, cont: -1})
	}
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
			if math.Abs(t.Pos.X-z.Pos.X) >= z.Half.X || math.Abs(t.Pos.Z-z.Pos.Z) >= z.Half.Z {
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
			z.Prog, z.cont = 0, -1 // partial capture lapses
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
		// Capturing from a different (or no) owner.
		if z.cont != contender {
			z.Prog, z.cont = 0, contender
		}
		z.Prog += dt
		if z.Prog >= z.Cap {
			z.Owner, z.Prog, z.hold = contender, 0, 0
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
		w.Tanks = append(w.Tanks, Tank{Bot: true, gone: true, Dead: true, vote: -1, Vehicle: 1, HP: veh(1).MaxHP, ammo: veh(1).AmmoMax, Name: botName(len(w.Tanks)), Team: -1, Carrying: -1, weapon2: wepGrenade})
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
var survivalBodies = []int{BodySpider, BodyInsect, BodyScorpion, BodyCrab, BodyQuad, BodySerpent, BodyOctopod, BodyHumanoid, BodyTripod, BodyDrone}

// spawnWave activates the next, larger wave of bots (tougher vehicles later).
func (w *World) spawnWave() {
	w.wave++
	n := 2 + w.wave
	if n > survivalPool {
		n = survivalPool
	}
	vi := w.wave / 2
	if vi > 2 {
		vi = 2
	}
	act := 0
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if !t.Bot {
			continue
		}
		if act < n {
			t.gone, t.Dead = false, false
			t.Vehicle, t.HP = vi, veh(vi).MaxHP
			t.body = survivalBodies[(w.wave-1+act)%len(survivalBodies)] // bestiary: a shifting mix per wave
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
func (w *World) collectFlags() {
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Bot || t.Dead || t.gone {
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
	}
}

// stepHazard burns tanks standing within a hazard's footprint at its DPS. A
// per-tank fractional accumulator turns the float DPS*dt into whole-HP hits so
// integer HP still drains at the authored rate. Shield/spawn-guard protect you.
func (w *World) stepHazard(e *Entity, dt float64) {
	for ti := range w.Tanks {
		t := &w.Tanks[ti]
		if t.Dead || t.gone || t.guard > 0 || t.shieldT > 0 {
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
// you come back down, touch, and fire again. Only tanks with vertical physics
// (players) actually leave the ground today; bots snap to ground each tick.
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
	dmg := def.Damage
	if dmg == 0 && e.Turret != nil { // fall back to the turret's authored damage
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
		if weapon > 0 && weapon < len(Weapons) {
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
	for i := range w.Tanks {
		if w.Tanks[i].cooldown > 0 {
			w.Tanks[i].cooldown -= dt
		}
		if w.Tanks[i].cooldown2 > 0 {
			w.Tanks[i].cooldown2 -= dt
		}
		if max := w.Tanks[i].veh().AmmoMax; w.Tanks[i].ammo < max { // regenerate ammo
			if w.Tanks[i].ammo += w.Tanks[i].veh().AmmoRegen * dt; w.Tanks[i].ammo > max {
				w.Tanks[i].ammo = max
			}
		}
		if w.Tanks[i].slowT > 0 {
			w.Tanks[i].slowT -= dt
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
		w.spawnWave() // wave cleared -> next, bigger wave
	}
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
	spd := v.Speed
	if t.slowT > 0 { // EffSlow drag
		spd *= 1 - t.slowMag
	}
	if in.Throttle {
		t.Pos = t.Pos.Add(V3{f.X * spd * dt, 0, f.Z * spd * dt})
	}
	if in.Reverse {
		t.Pos = t.Pos.Sub(V3{f.X * spd * dt, 0, f.Z * spd * dt})
	}
	if in.HullL {
		t.HullYaw -= v.HullTurn * dt
	}
	if in.HullR {
		t.HullYaw += v.HullTurn * dt
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
	clampArena(&t.Pos, w.half())
	w.collide(&t.Pos)
	support := w.ground(t.Pos.X, t.Pos.Z, t.Pos.Y)
	stepVertical(&t.Pos, &t.vy, in.Jump, dt, v.Jump, support)
	if in.Fire && t.cooldown <= 0 {
		w.fire(i)
	}
	if in.Fire2 && t.cooldown2 <= 0 && t.weapon2 > 0 && t.weapon2 < len(Weapons) {
		w.fireWeapon(i, &Weapons[t.weapon2], true) // B: secondary weapon
	}
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
	BodyTank = iota
	BodySpider
	BodyQuad
	BodyInsect
	BodyHumanoid
	BodyScorpion
	BodySerpent
	BodyTripod
	BodyDrone
	BodyCrab
	BodyOctopod
	BodyKinds
)

// botBodies is the pool regular (non-Survival) bots roll their appearance from -
// tanks weighted so the field stays mostly armor with a scatter of creatures.
var botBodies = []int{
	BodyTank, BodyTank, BodyTank,
	BodySpider, BodyQuad, BodyInsect, BodyHumanoid,
	BodyScorpion, BodySerpent, BodyTripod, BodyDrone, BodyCrab, BodyOctopod,
}

func (w *World) botAI(i int, dt float64) {
	if w.rules().Teams == 2 {
		w.ctfBotAI(i, dt)
		return
	}
	b := &w.Tanks[i]
	tgt := w.nearestEnemy(i)
	if tgt < 0 {
		b.lastTgt, b.acquireT = -1, 0
		w.botWander(i, dt) // no one in sight: roam instead of standing still
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
	b.HullYaw = turnToward(b.HullYaw, angTo, v.HullTurn*dt)
	wantPitch := clampPitch(math.Atan2((w.Tanks[tgt].Pos.Y+turretAimHeight)-(b.Pos.Y+EyeHeight), dist))
	if !reacting { // hold turret while reacting, then track at the bot's rate
		b.TurretYaw = turnToward(b.TurretYaw, angDiff(angTo, b.HullYaw), b.aiTrack*dt)
		b.TurretPitch = turnToward(b.TurretPitch, wantPitch, b.aiTrack*dt)
	}
	// Movement: smart bots divert to a nearby power-up, else hold engagement range.
	if pp, ok := w.botSeekTarget(b); ok {
		ang := math.Atan2(pp.X-b.Pos.X, pp.Z-b.Pos.Z)
		b.HullYaw = turnToward(b.HullYaw, ang, v.HullTurn*dt)
		w.driveForward(i, dt, 0.8)
	} else if dist > b.aiKeep {
		w.driveForward(i, dt, 0.7)
	}
	b.Pos.Y = w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y+stepUp)
	if !reacting && dist < botFireRange && math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) < botAimTol &&
		math.Abs(wantPitch-b.TurretPitch) < botAimTol && b.cooldown <= 0 {
		w.fire(i) // fire() applies this bot's wobble + reload cadence
	}
	if !reacting {
		w.botSecondary(i, dist, angTo)
	}
}

// botSecondary lets a bot lob its secondary (grenade) at a target in the mid-range
// band where a level-aimed lob lands, on its own cooldown - so bots use grenades
// too instead of only the cannon.
func (w *World) botSecondary(i int, dist, angTo float64) {
	b := &w.Tanks[i]
	if b.weapon2 <= 0 || b.weapon2 >= len(Weapons) || b.cooldown2 > 0 {
		return
	}
	if dist < 9 || dist > 20 { // too close/far for a level lob to land near them
		return
	}
	if math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) > botAimTol {
		return
	}
	if rand.Float64() < 0.5 { // occasional, not every opportunity
		w.fireWeapon(i, &Weapons[b.weapon2], true)
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
	b.HullYaw = turnToward(b.HullYaw, math.Atan2(dx, dz), v.HullTurn*dt)
	b.TurretYaw = turnToward(b.TurretYaw, 0, b.aiTrack*dt*0.5) // ease the barrel back to center while scanning
	w.driveForward(i, dt, 0.5)                                 // amble, not a charge
	b.Pos.Y = w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y+stepUp)
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
	have := false
	switch {
	case b.Carrying >= 0:
		if of := w.ownFlag(b.Team); of != nil {
			dst, have = of.Home, true // run the flag back to base
		}
	default:
		if ef := w.enemyFlag(b.Team); ef != nil && ef.Carrier < 0 {
			dst, have = ef.Pos, true // go grab the enemy flag
		}
	}
	tgt := w.nearestEnemy(i)
	if !have && tgt >= 0 {
		dst, have = w.Tanks[tgt].Pos, true // nothing to fetch: hunt
	}
	if have {
		d := dst.Sub(b.Pos)
		if math.Hypot(d.X, d.Z) > 1.0 {
			angTo := math.Atan2(d.X, d.Z)
			b.HullYaw = turnToward(b.HullYaw, angTo, v.HullTurn*dt)
			w.driveForward(i, dt, 0.7)
		}
	} else {
		w.botWander(i, dt) // no flag objective and no enemy: roam
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
			if dist < botFireRange && math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) < botAimTol &&
				math.Abs(wantPitch-b.TurretPitch) < botAimTol && b.cooldown <= 0 {
				w.fire(i)
			}
			w.botSecondary(i, dist, angTo)
		}
	} else {
		b.lastTgt, b.acquireT = -1, 0
	}
	b.Pos.Y = w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y+stepUp)
}

// driveForward moves a bot along its hull heading at the given speed fraction,
// using whisker avoidance to route around obstacles instead of shoving them.
func (w *World) driveForward(i int, dt, frac float64) {
	b := &w.Tanks[i]
	v := b.veh()
	f := fwd(b.HullYaw)
	ahead := V3{b.Pos.X + f.X*3, b.Pos.Y, b.Pos.Z + f.Z*3}
	if w.blocked(ahead) {
		lf := fwd(b.HullYaw - 0.7)
		if w.blocked(V3{b.Pos.X + lf.X*3, b.Pos.Y, b.Pos.Z + lf.Z*3}) {
			b.HullYaw += v.HullTurn * dt
		} else {
			b.HullYaw -= v.HullTurn * dt
		}
		f = fwd(b.HullYaw)
		b.Pos = b.Pos.Add(V3{f.X * v.Speed * 0.3 * dt, 0, f.Z * v.Speed * 0.3 * dt})
	} else {
		b.Pos = b.Pos.Add(V3{f.X * v.Speed * frac * dt, 0, f.Z * v.Speed * frac * dt})
	}
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
func (w *World) hurt(ti, dmg, owner int, cause KillCause) {
	t := &w.Tanks[ti]
	t.HP -= dmg
	t.hitFlash = tankHitFlash
	if t.HP > 0 {
		return
	}
	t.Dead = true
	t.respawn = respawnDelay
	t.Deaths++
	t.shieldT, t.rapidT, t.cloakT = 0, 0, 0 // buffs die with you
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
			dx, dz := t.Pos.X-s.Pos.X, t.Pos.Z-s.Pos.Z
			// Height-aware: the shot must also be within the tank's body span, so
			// elevation matters (shoot over cover, or miss high). The window spans
			// from just below the feet to above the turret, scaled by vehicle size.
			dyLow, dyHigh := t.Pos.Y-0.3, t.Pos.Y+tankBodyTop*t.veh().Scale
			if dx*dx+dz*dz < hitRadius*hitRadius && s.Pos.Y >= dyLow && s.Pos.Y <= dyHigh {
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
	if s.affects == TargetAllies && !teammate { // support: teammates only
		return false
	}
	if s.affects == TargetFoes && teammate { // damage/debuffs: no friendly fire
		return false
	}
	if s.affects != TargetAllies && (t.guard > 0 || t.shieldT > 0) && s.eff != EffShieldBust {
		return false
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
		w.applyShotHit(s, direct)
	}
}

// detonate applies a blast weapon's effect to every eligible tank within its
// radius of the impact point, shoves them outward (for damage blasts), and emits
// a burst of visual debris.
func (w *World) detonate(s *Projectile, at V3) {
	w.spawnBlastFX(at)
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

// applyShotHit resolves a projectile striking tank ti by its effect payload. W1
// handles EffDamage (ordinary damage); the effect palette (heal/slow/knockback/
// shield-bust) lands in W2, dispatched off the same switch.
func (w *World) applyShotHit(s *Projectile, ti int) {
	t := &w.Tanks[ti]
	switch s.eff {
	case EffHeal:
		if max := t.veh().MaxHP; t.HP < max {
			if t.HP += int(s.mag); t.HP > max {
				t.HP = max
			}
		}
		t.hitFlash = tankHitFlash // brief blink as feedback
	case EffSlow:
		t.slowT, t.slowMag = s.dur, clampF01(s.mag)
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
		cause := CauseCannon // a tank's shot; entity-fired shots have no tank owner
		if s.owner < 0 {
			cause = CauseTurret
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
	// Prefer the map's authored spawn points (clear of other tanks).
	if sp := w.ActiveMap().Spawns; len(sp) > 0 {
		for _, k := range rand.Perm(len(sp)) {
			if clear(sp[k].X, sp[k].Z) {
				return sp[k]
			}
		}
		return sp[rand.Intn(len(sp))]
	}
	for tries := 0; tries < 24; tries++ {
		x := (rand.Float64()*2 - 1) * (w.half() - 1)
		z := (rand.Float64()*2 - 1) * (w.half() - 1)
		if clear(x, z) {
			return V3{x, 0, z}
		}
	}
	return V3{0, 0, 0}
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
		ts = append(ts, TankSnap{
			ID: i, Pos: t.Pos, HullYaw: t.HullYaw, TurretYaw: t.TurretYaw, TurretPitch: t.TurretPitch,
			HP: t.HP, Color: t.Color, Name: t.Name, Dead: t.Dead, Bot: t.Bot,
			Shield: t.guard > 0 || t.shieldT > 0, Hit: t.hitFlash > 0,
			Cloak: t.cloakT > 0, Rapid: t.rapidT > 0,
			Vehicle: t.Vehicle, Body: t.body, Lives: t.lives, Team: t.Team, Carrying: t.Carrying >= 0,
			Kills: t.Kills, Deaths: t.Deaths, RespawnIn: t.respawn, Reload: reload, Ammo: ammoFrac, HoldScore: t.holdScore,
		})
	}
	sh := make([]ShotSnap, len(w.Shots))
	for i := range w.Shots {
		sh[i] = ShotSnap{Pos: w.Shots[i].Pos, Vis: w.Shots[i].vis}
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
