// Package game is the authoritative tank-combat simulation, shared by the arena
// server (which owns and broadcasts it) and the door (which runs it locally in
// offline mode and reconstructs it from network state online). It contains NO
// rendering or I/O — just world state and the tick update.
package game

import (
	"embed"
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
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
	Kind  string     // archetype / render selector: "turret","wall","hazard","teleporter"
	Pos   V3         // center
	Half  V3         // box half-extent (collision + default visual)
	Color [3]float64 // base tint
	Yaw   float64    // facing: authored = initial; runtime = current (turret tracks)
	Pitch float64    // gun elevation (turret): runtime, + = aim up
	Solid bool       // collides like an obstacle while alive

	Turret   *TurretTrait
	Hazard   *HazardTrait
	Teleport *TeleportTrait
	Destruct *DestructTrait
	Respawn  *RespawnTrait

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

// Map is a static arena layout. Size is the arena half-extent (0 = default).
// Pickups reserves power-up spawn spots for a later drop system. Entities are
// authored trait-objects (turrets, hazards, ...) instantiated each match.
type Map struct {
	Name      string
	Size      float64
	Obstacles []Box
	Ramps     []Ramp
	Scenery   []Prop
	Spawns    []V3
	Pickups   []V3
	Entities  []Entity
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
type jentity struct {
	Kind     string     `json:"kind"`
	Pos      [3]float64 `json:"pos"`
	Half     [3]float64 `json:"half"`
	Color    [3]float64 `json:"color"`
	Yaw      float64    `json:"yaw"`
	Solid    bool       `json:"solid"`
	Turret   *jturret   `json:"turret"`
	Hazard   *jhazard   `json:"hazard"`
	Teleport *jteleport `json:"teleport"`
	Destruct *jdestruct `json:"destruct"`
	Respawn  *jrespawn  `json:"respawn"`
}
type jmap struct {
	Name      string       `json:"name"`
	Size      float64      `json:"size"`
	Obstacles []jbox       `json:"obstacles"`
	Ramps     []jramp      `json:"ramps"`
	Scenery   []jprop      `json:"scenery"`
	Spawns    [][2]float64 `json:"spawns"`
	Pickups   [][2]float64 `json:"pickups"`
	Entities  []jentity    `json:"entities"`
}

func (je jentity) toEntity() Entity {
	e := Entity{Kind: je.Kind, Pos: v3(je.Pos), Half: v3(je.Half), Color: je.Color, Yaw: je.Yaw, Solid: je.Solid}
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
// these get sent to clients over the wire, so the door needn't have them). It
// returns how many were added.
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
			continue
		}
		Maps = append(Maps, jm.toMap())
		n++
	}
	return n
}

func v3(a [3]float64) V3   { return V3{a[0], a[1], a[2]} }
func v3xz(a [2]float64) V3 { return V3{a[0], 0, a[1]} }

func (jm jmap) toMap() Map {
	m := Map{Name: jm.Name, Size: jm.Size}
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
	for _, s := range jm.Pickups {
		m.Pickups = append(m.Pickups, v3xz(s))
	}
	for _, e := range jm.Entities {
		m.Entities = append(m.Entities, e.toEntity())
	}
	return m
}

// --- tuning knobs (exported ones are also needed by the renderer) ---
const (
	tankSpeed        = 6.0
	hullTurnRate     = 1.9
	turretRate       = 2.6 // bot turret tracking speed
	playerTurretRate = 1.3 // player aim speed (slower = finer aim, less overshoot)
	pitchRate        = 1.1  // player gun-elevation speed (rad/sec)
	pitchMax         = 0.70 // max elevation (aim up), ~40 deg
	pitchMin         = -0.50 // max depression (aim down), ~-29 deg
	fireDelay        = 0.55
	jumpSpeed        = 8.5  // upward launch velocity (units/sec)
	gravity          = 24.0 // downward acceleration (units/sec^2)
	projSpeed        = 24.0
	projLife         = 2.4
	projDmg          = 34
	tankMaxHP        = 100
	hitRadius        = 1.15
	tankBodyTop      = 1.9 // top of a tank's hittable body above its feet (×vehicle scale)

	ArenaA = 22.0 // default playfield half-extent (maps may override via Size)

	respawnDelay   = 3.0
	spawnGuardTime = 1.6
	EyeHeight      = 1.35

	botFireRange = 26.0
	botAimTol    = 0.12
	botKeepDist  = 7.0
	botFireDelay = 1.2

	turretAimHeight = 0.9 // aim point above a target tank's feet (body center)
)

// Mode is a game mode; Phase is the match lifecycle state.
type Mode int

const (
	ModeDeathmatch Mode = iota
	ModeFlagRun
	ModeCTF
	ModeSurvival
)

func (m Mode) String() string {
	switch m {
	case ModeFlagRun:
		return "FLAG RUN"
	case ModeCTF:
		return "CAPTURE THE FLAG"
	case ModeSurvival:
		return "SURVIVAL"
	default:
		return "DEATHMATCH"
	}
}

type Phase int

const (
	PhaseCountdown Phase = iota
	PhaseActive
	PhaseEnded
	PhaseLobby // between matches (server only): vote for the next mode
)

// votable lists the modes the lobby rotates/votes between (functional ones).
var votable = []Mode{ModeDeathmatch, ModeFlagRun, ModeSurvival, ModeCTF}

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
}

// Vehicles is the selectable class list (index = wire id).
var Vehicles = []Vehicle{
	{Name: "SCOUT", MaxHP: 70, Speed: 8.2, HullTurn: 2.4, AimTurn: 1.7, FireDelay: 0.42, Jump: 10.0, Scale: 0.82},
	{Name: "HUNTER", MaxHP: 100, Speed: 6.0, HullTurn: 1.9, AimTurn: 1.3, FireDelay: 0.55, Jump: 8.5, Scale: 1.0},
	{Name: "HEAVY", MaxHP: 150, Speed: 4.3, HullTurn: 1.3, AimTurn: 1.0, FireDelay: 0.85, Jump: 6.5, Scale: 1.22},
}

func veh(i int) Vehicle {
	if i < 0 || i >= len(Vehicles) {
		i = 1 // HUNTER default
	}
	return Vehicles[i]
}

// BotPalette / PlayerPalette give tanks distinct colors by slot.
var BotPalette = [][3]float64{
	{0.78, 0.26, 0.26}, {0.72, 0.60, 0.22}, {0.30, 0.70, 0.35},
	{0.65, 0.35, 0.75}, {0.80, 0.45, 0.20}, {0.40, 0.55, 0.80},
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
	Recenter                                                     bool // snap turret to hull-forward + level
	Vote                                                         int
}

type Tank struct {
	Pos         V3
	HullYaw     float64
	TurretYaw   float64 // relative to hull
	TurretPitch float64 // gun elevation: + = aim up, - = aim down (radians)
	HP          int
	Color     [3]float64
	Vehicle   int
	Bot       bool
	Dead      bool
	Kills     int
	Deaths    int

	cooldown float64
	respawn  float64
	guard    float64
	vy       float64 // vertical velocity (jump/gravity)
	hitFlash float64 // brief flash timer after taking damage
	vote     int     // lobby vote: mode index, or -1 for none
	lives    int     // Survival: respawns remaining (humans)
	Team     int     // CTF: 0 or 1 (-1 = none in non-team modes)
	Carrying int     // CTF: index of the enemy flag being carried, or -1
	shieldT  float64 // power-up: invulnerability remaining (sec)
	rapidT   float64 // power-up: rapid-fire remaining (sec)
	cloakT   float64 // power-up: cloak/invisibility remaining (sec)
	gone     bool    // player left; slot inert and reusable

	hazardDebt float64 // hazard-trait: fractional HP damage carried between ticks
	teleT      float64 // teleporter debounce remaining (sec); 0 = can teleport
}

// TankSnap is the renderable/transmittable view of a tank: exported, flat, no
// sim internals. ID is the stable world index (so the viewer matches by ID even
// as the snapshot omits vacated slots).
type TankSnap struct {
	ID                      int
	Pos                     V3
	HullYaw, TurretYaw      float64
	TurretPitch             float64 // gun elevation (+ up)
	HP                      int
	Color                  [3]float64
	Dead, Bot, Shield, Hit bool
	Kills, Deaths          int
	Vehicle                int
	Lives                  int
	Team                   int  // CTF team (-1 in non-team modes)
	Carrying               bool // CTF: carrying an enemy flag
	Cloak                  bool // power-up: cloaked (hidden from enemies)
	Rapid                  bool // power-up: rapid-fire active
	RespawnIn              float64
	Reload                 float64 // 0 = ready to fire, ->1 = just fired
}

// PickKind enumerates power-up drop types.
const (
	PickRepair = iota // instant heal to full HP
	PickShield        // timed invulnerability
	PickRapid         // timed faster fire
	PickCloak         // timed invisibility
	pickKinds         // count (keep last)
)

// Pickup is a power-up drop sitting on the map.
type Pickup struct {
	Pos  V3
	Kind int
}

// PickupSnap is the renderable/transmittable view of a pickup.
type PickupSnap struct {
	Pos  V3
	Kind int
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
	Pos   V3
	vel   V3
	life  float64
	owner int // firing tank index; <0 = a map entity (e.g. a turret), no kill credit
	dmg   int // 0 -> default projDmg
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

// MatchSnap is the transmittable match state (lifecycle, mode, clock, winner,
// and Flag Run progress).
type MatchSnap struct {
	Mode       Mode
	Phase      Phase
	Timer      float64
	WinnerID   int
	FlagsLeft  int
	FlagsTotal int
	Votes      [4]int // lobby vote tally per mode index
	MapIdx     int    // active map index
	Wave       int    // Survival: current wave
	TeamScore  [2]int // CTF: captures per team
	WinnerTeam int    // CTF: winning team (0/1), -1 = tie/none
}

// Match returns the current match state for the snapshot/wire.
func (w *World) Match() MatchSnap {
	left := 0
	for i := range w.flags {
		if !w.flags[i].Taken {
			left++
		}
	}
	var votes [4]int
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if !t.Bot && !t.gone && t.vote >= 0 && t.vote < len(votes) {
			votes[t.vote]++
		}
	}
	return MatchSnap{
		Mode: w.Mode, Phase: w.Phase, Timer: w.Timer, WinnerID: w.WinnerID,
		FlagsLeft: left, FlagsTotal: len(w.flags), Votes: votes, MapIdx: w.MapIdx, Wave: w.wave,
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
	w := &World{Mode: mode, Phase: PhaseCountdown, Timer: countdownTime, WinnerID: -1}
	if len(Maps) > 0 {
		w.MapIdx = rand.Intn(len(Maps)) // start on a random map, not always the empty one
	}
	for b := 0; b < numBots; b++ {
		vi := rand.Intn(len(Vehicles))
		w.Tanks = append(w.Tanks, Tank{
			Bot: true, HP: veh(vi).MaxHP, guard: spawnGuardTime, vote: -1, Vehicle: vi,
			Color: BotPalette[b%len(BotPalette)], Team: -1, Carrying: -1,
		})
		w.Tanks[b].Pos = w.spawnPoint(b)
	}
	return w
}

// AddPlayer inserts a human tank (reusing a vacated slot if any) and returns its
// index. color may be the zero value to auto-pick from PlayerPalette.
func (w *World) AddPlayer(color [3]float64, vehicle int) int {
	if color == ([3]float64{}) {
		color = PlayerPalette[w.humanCount()%len(PlayerPalette)]
	}
	mk := func(i int) Tank {
		t := Tank{HP: veh(vehicle).MaxHP, Color: color, guard: spawnGuardTime, vote: -1, Vehicle: vehicle, lives: survivalLives, Team: -1, Carrying: -1}
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

func (w *World) fire(owner int) {
	t := &w.Tanks[owner]
	d := aimDir(t.HullYaw+t.TurretYaw, t.TurretPitch)
	w.Shots = append(w.Shots, Projectile{
		// muzzle: gun height above the tank's feet, offset forward along the aim.
		Pos:   V3{t.Pos.X + d.X*1.7, t.Pos.Y + EyeHeight + d.Y*1.7, t.Pos.Z + d.Z*1.7},
		vel:   V3{d.X * projSpeed, d.Y * projSpeed, d.Z * projSpeed},
		life:  projLife,
		owner: owner,
	})
	delay := veh(t.Vehicle).FireDelay
	if t.rapidT > 0 {
		delay *= rapidFireMul
	}
	t.cooldown = delay
}

// Update advances the world by dt. inputs maps a human tank index to its held
// buttons this tick (absent => idle); bots are driven by AI. The match
// lifecycle (countdown -> active -> ended -> next countdown) gates simulation.
func (w *World) Update(dt float64, inputs map[int]Input) {
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
			w.startCountdown(w.pickNextMode())
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
	w.Mode, w.Phase, w.Timer, w.WinnerID = mode, PhaseCountdown, countdownTime, -1
	if len(Maps) > 1 && !w.pinned {
		w.MapIdx = (w.MapIdx + 1) % len(Maps) // rotate the map each match
	}
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

// pickNextMode chooses the next mode from votes (highest wins), else advances the
// rotation so an empty/idle arena still cycles.
func (w *World) pickNextMode() Mode {
	best, bestN := Mode(-1), 0
	for _, m := range votable {
		n := 0
		for i := range w.Tanks {
			t := &w.Tanks[i]
			if !t.Bot && !t.gone && t.vote == int(m) {
				n++
			}
		}
		if n > bestN {
			bestN, best = n, m
		}
	}
	if bestN > 0 {
		return best
	}
	w.rotIdx = (w.rotIdx + 1) % len(votable)
	return votable[w.rotIdx]
}

// startMatch begins active play: full reset of scores, health, and positions.
func (w *World) startMatch() {
	w.Phase, w.Timer, w.WinnerID, w.Shots = PhaseActive, matchTime, -1, nil
	w.teamScore, w.winnerTeam = [2]int{}, -1
	if w.Mode == ModeCTF {
		w.assignTeams() // teams first, so spawnPoint can use the team bases
	}
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone {
			continue
		}
		t.Kills, t.Deaths, t.Carrying = 0, 0, -1
		t.HP, t.Dead, t.vy = veh(t.Vehicle).MaxHP, false, 0
		t.shieldT, t.rapidT, t.cloakT = 0, 0, 0
		t.guard = spawnGuardTime
		t.Pos = w.spawnPoint(i)
		t.HullYaw = w.faceTarget(i)
		t.TurretYaw = 0
	}
	w.pickups, w.pickupTimer = nil, pickupInterval
	w.resetEntities()
	w.flags = nil
	if w.Mode == ModeFlagRun {
		for i := 0; i < flagCount; i++ {
			x := (rand.Float64()*2 - 1) * (w.half() - 2)
			z := (rand.Float64()*2 - 1) * (w.half() - 2)
			w.flags = append(w.flags, Flag{Pos: V3{x, 0, z}, Team: -1, Carrier: -1})
		}
	}
	if w.Mode == ModeCTF {
		w.createTeamFlags()
	}
	if w.Mode == ModeSurvival {
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

// createTeamFlags places one flag at each team's base, sitting at home.
func (w *World) createTeamFlags() {
	for team := 0; team < 2; team++ {
		home := w.ctfBase(team)
		w.flags = append(w.flags, Flag{Pos: home, Home: home, Team: team, Carrier: -1, atHome: true})
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
		w.Tanks = append(w.Tanks, Tank{Bot: true, gone: true, Dead: true, vote: -1, Vehicle: 1, HP: veh(1).MaxHP, Team: -1, Carrying: -1})
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
			t.guard, t.cooldown, t.vy, t.TurretYaw = spawnGuardTime, 0, 0, 0
			t.Color = BotPalette[act%len(BotPalette)]
			t.Pos = w.spawnPoint(i)
			t.HullYaw = w.faceTarget(i)
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
				w.applyPickup(t, p.Kind)
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
			w.hurt(ti, whole, -1)
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
	wantPitch := math.Atan2((t.Pos.Y + turretAimHeight) - muzzleY, horiz)
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
	d := aimDir(e.Yaw, e.Pitch)
	muzzleY := e.Pos.Y + e.Half.Y + 0.3
	dmg := 0
	if e.Turret != nil {
		dmg = e.Turret.Dmg
	}
	w.Shots = append(w.Shots, Projectile{
		Pos:   V3{e.Pos.X + d.X*1.2, muzzleY + d.Y*1.2, e.Pos.Z + d.Z*1.2},
		vel:   V3{d.X * projSpeed, d.Y * projSpeed, d.Z * projSpeed},
		life:  projLife,
		owner: -1,
		dmg:   dmg,
	})
}

// spawnPickup drops a random power-up at a free authored spawn spot (or a random
// open tile if the map defines none).
func (w *World) spawnPickup() {
	pos, ok := w.pickupSpot()
	if !ok {
		return
	}
	pos.Y = GroundHeight(w.ActiveMap(), pos.X, pos.Z, pos.Y)
	w.pickups = append(w.pickups, Pickup{Pos: pos, Kind: rand.Intn(pickKinds)})
}

// pickupSpot returns a free location for a new drop: an unoccupied authored spot
// if the map lists any, otherwise a random open (non-blocked) tile.
func (w *World) pickupSpot() (V3, bool) {
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
			if !occupied(spots[k].X, spots[k].Z) {
				return spots[k], true
			}
		}
		return V3{}, false
	}
	for tries := 0; tries < 16; tries++ {
		x := (rand.Float64()*2 - 1) * (w.half() - 2)
		z := (rand.Float64()*2 - 1) * (w.half() - 2)
		if !w.blocked(V3{x, 0, z}) {
			return V3{x, 0, z}, true
		}
	}
	return V3{}, false
}

// applyPickup grants a power-up's effect to a tank.
func (w *World) applyPickup(t *Tank, kind int) {
	switch kind {
	case PickRepair:
		t.HP = veh(t.Vehicle).MaxHP
	case PickShield:
		t.shieldT = buffShieldTime
	case PickRapid:
		t.rapidT = buffRapidTime
	case PickCloak:
		t.cloakT = buffCloakTime
	}
}

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
	done := false
	switch w.Mode {
	case ModeDeathmatch:
		done = w.Timer <= 0
		for i := range w.Tanks {
			if !w.Tanks[i].gone && w.Tanks[i].Kills >= DMFragLimit {
				done = true
			}
		}
	case ModeFlagRun:
		done = w.Timer <= 0 || w.allFlagsTaken()
	case ModeSurvival:
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
		done = humans > 0 && allOut
	case ModeCTF:
		done = w.Timer <= 0 || w.teamScore[0] >= ctfCaptureLimit || w.teamScore[1] >= ctfCaptureLimit
	}
	if !done {
		return
	}
	if w.Mode == ModeCTF {
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

func (w *World) computeWinner() int {
	if w.Mode == ModeSurvival {
		return -1 // co-op: the result is the wave reached, not a winner
	}
	if w.Mode == ModeCTF {
		return -1 // team mode: the winner is a team, not a tank (see WinnerTeam)
	}
	if w.Mode == ModeFlagRun {
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
	if w.Mode == ModeFlagRun {
		w.collectFlags()
	}
	if w.Mode == ModeCTF {
		w.stepCTF(dt)
	}
	w.stepPickups(dt)
	w.stepEntities(dt)
	w.respawns(dt)
	if w.Mode == ModeSurvival && w.activeBots() == 0 {
		w.spawnWave() // wave cleared -> next, bigger wave
	}
}

func (w *World) applyInput(i int, in Input, dt float64) {
	t := &w.Tanks[i]
	v := veh(t.Vehicle)
	f := fwd(t.HullYaw)
	if in.Throttle {
		t.Pos = t.Pos.Add(V3{f.X * v.Speed * dt, 0, f.Z * v.Speed * dt})
	}
	if in.Reverse {
		t.Pos = t.Pos.Sub(V3{f.X * v.Speed * dt, 0, f.Z * v.Speed * dt})
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
	}
	clampArena(&t.Pos, w.half())
	w.collide(&t.Pos)
	support := w.ground(t.Pos.X, t.Pos.Z, t.Pos.Y)
	stepVertical(&t.Pos, &t.vy, in.Jump, dt, v.Jump, support)
	if in.Fire && t.cooldown <= 0 {
		w.fire(i)
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

func (w *World) botAI(i int, dt float64) {
	if w.Mode == ModeCTF {
		w.ctfBotAI(i, dt)
		return
	}
	b := &w.Tanks[i]
	tgt := w.nearestEnemy(i)
	if tgt < 0 {
		return // no one to chase
	}
	v := veh(b.Vehicle)
	d := w.Tanks[tgt].Pos.Sub(b.Pos)
	dist := math.Hypot(d.X, d.Z)
	angTo := math.Atan2(d.X, d.Z)
	b.HullYaw = turnToward(b.HullYaw, angTo, v.HullTurn*dt)
	b.TurretYaw = turnToward(b.TurretYaw, angDiff(angTo, b.HullYaw), turretRate*dt) // bots track responsively
	wantPitch := clampPitch(math.Atan2((w.Tanks[tgt].Pos.Y+turretAimHeight)-(b.Pos.Y+EyeHeight), dist))
	b.TurretPitch = turnToward(b.TurretPitch, wantPitch, turretRate*dt)
	if dist > botKeepDist {
		w.driveForward(i, dt, 0.7)
	}
	b.Pos.Y = w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y+stepUp)
	if dist < botFireRange && math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) < botAimTol &&
		math.Abs(wantPitch-b.TurretPitch) < botAimTol && b.cooldown <= 0 {
		w.fire(i) // fire() sets cooldown to the bot's vehicle FireDelay
	}
}

// ctfBotAI drives a CTF bot by objective: carry the enemy flag home, else fetch
// it, else defend by fighting the nearest enemy. The turret tracks and fires at
// the nearest enemy throughout, so bots stay dangerous while pursuing flags.
func (w *World) ctfBotAI(i int, dt float64) {
	b := &w.Tanks[i]
	v := veh(b.Vehicle)

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
	}
	// Turret tracks/fires at the nearest enemy regardless of where we're driving.
	if tgt >= 0 {
		d := w.Tanks[tgt].Pos.Sub(b.Pos)
		dist := math.Hypot(d.X, d.Z)
		angTo := math.Atan2(d.X, d.Z)
		b.TurretYaw = turnToward(b.TurretYaw, angDiff(angTo, b.HullYaw), turretRate*dt)
		wantPitch := clampPitch(math.Atan2((w.Tanks[tgt].Pos.Y+turretAimHeight)-(b.Pos.Y+EyeHeight), dist))
		b.TurretPitch = turnToward(b.TurretPitch, wantPitch, turretRate*dt)
		if dist < botFireRange && math.Abs(angDiff(b.HullYaw+b.TurretYaw, angTo)) < botAimTol &&
			math.Abs(wantPitch-b.TurretPitch) < botAimTol && b.cooldown <= 0 {
			w.fire(i)
		}
	}
	b.Pos.Y = w.ground(b.Pos.X, b.Pos.Z, b.Pos.Y+stepUp)
}

// driveForward moves a bot along its hull heading at the given speed fraction,
// using whisker avoidance to route around obstacles instead of shoving them.
func (w *World) driveForward(i int, dt, frac float64) {
	b := &w.Tanks[i]
	v := veh(b.Vehicle)
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
	best, bestD := -1, math.MaxFloat64
	for j := range w.Tanks {
		t := &w.Tanks[j]
		if j == self || t.Dead || t.gone || t.cloakT > 0 {
			continue // cloaked tanks can't be targeted by bots
		}
		if w.Mode == ModeCTF && t.Team == w.Tanks[self].Team {
			continue
		}
		d := t.Pos.Sub(w.Tanks[self].Pos)
		if dd := d.X*d.X + d.Z*d.Z; dd < bestD {
			bestD, best = dd, j
		}
	}
	return best
}

// hurt applies dmg to tank ti and handles death bookkeeping (respawn timer,
// deaths, buff clearing, Survival lives, CTF flag drop, kill credit). owner is
// the firing tank index for kill credit; <0 means no credit (e.g. a turret or
// a hazard killed them). Caller has already checked the tank is vulnerable.
func (w *World) hurt(ti, dmg, owner int) {
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
	if w.Mode == ModeSurvival && !t.Bot {
		t.lives--
	}
	// CTF: drop any carried flag where the carrier fell.
	if w.Mode == ModeCTF && t.Carrying >= 0 {
		f := &w.flags[t.Carrying]
		f.Carrier, f.atHome = -1, false
		f.Pos, f.dropTimer = V3{t.Pos.X, 0, t.Pos.Z}, flagReturnTime
		t.Carrying = -1
	}
	if owner >= 0 && owner < len(w.Tanks) && owner != ti {
		w.Tanks[owner].Kills++
	}
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
		s.Pos = s.Pos.Add(V3{s.vel.X * dt, s.vel.Y * dt, s.vel.Z * dt})
		s.life -= dt
		if s.life <= 0 || math.Abs(s.Pos.X) > w.half()+0.6 || math.Abs(s.Pos.Z) > w.half()+0.6 {
			continue
		}
		if w.hitObstacle(s.Pos) { // tall cover blocks shots; low cover they fly over
			continue
		}
		if w.shotHitsEntity(&s) { // solid entities block; destructibles take damage
			continue
		}
		hit := false
		for ti := range w.Tanks {
			t := &w.Tanks[ti]
			if ti == s.owner || t.Dead || t.gone || t.guard > 0 || t.shieldT > 0 {
				continue
			}
			// CTF: no friendly fire between teammates.
			if w.Mode == ModeCTF && s.owner >= 0 && s.owner < len(w.Tanks) && w.Tanks[s.owner].Team == t.Team {
				continue
			}
			dx, dz := t.Pos.X-s.Pos.X, t.Pos.Z-s.Pos.Z
			// Height-aware: the shot must also be within the tank's body span, so
			// elevation matters (shoot over cover, or miss high). The window spans
			// from just below the feet to above the turret, scaled by vehicle size.
			dyLow, dyHigh := t.Pos.Y-0.3, t.Pos.Y+tankBodyTop*veh(t.Vehicle).Scale
			if dx*dx+dz*dz < hitRadius*hitRadius && s.Pos.Y >= dyLow && s.Pos.Y <= dyHigh {
				dmg := s.dmg
				if dmg <= 0 {
					dmg = projDmg
				}
				w.hurt(ti, dmg, s.owner)
				hit = true
				break
			}
		}
		if !hit {
			live = append(live, s)
		}
	}
	w.Shots = live
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
		if w.Mode == ModeSurvival {
			if t.Bot || t.lives <= 0 {
				continue // bots come back via waves; out-of-lives humans stay dead
			}
		}
		t.respawn -= dt
		if t.respawn <= 0 {
			t.Dead = false
			t.HP = veh(t.Vehicle).MaxHP
			t.TurretYaw = 0
			t.guard = spawnGuardTime
			t.Pos = w.spawnPoint(i)
			t.HullYaw = w.faceTarget(i)
		}
	}
}

func (w *World) spawnPoint(self int) V3 {
	// CTF: spawn near your own base (jittered), so teams hold opposite ends.
	if w.Mode == ModeCTF && self >= 0 && self < len(w.Tanks) && w.Tanks[self].Team >= 0 {
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
func (w *World) Snapshot() ([]TankSnap, []V3, []FlagSnap, []PickupSnap) {
	ts := make([]TankSnap, 0, len(w.Tanks))
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone {
			continue
		}
		reload := t.cooldown / veh(t.Vehicle).FireDelay
		if reload < 0 {
			reload = 0
		} else if reload > 1 {
			reload = 1
		}
		ts = append(ts, TankSnap{
			ID: i, Pos: t.Pos, HullYaw: t.HullYaw, TurretYaw: t.TurretYaw, TurretPitch: t.TurretPitch,
			HP: t.HP, Color: t.Color, Dead: t.Dead, Bot: t.Bot,
			Shield: t.guard > 0 || t.shieldT > 0, Hit: t.hitFlash > 0,
			Cloak: t.cloakT > 0, Rapid: t.rapidT > 0,
			Vehicle: t.Vehicle, Lives: t.lives, Team: t.Team, Carrying: t.Carrying >= 0,
			Kills: t.Kills, Deaths: t.Deaths, RespawnIn: t.respawn, Reload: reload,
		})
	}
	sh := make([]V3, len(w.Shots))
	for i := range w.Shots {
		sh[i] = w.Shots[i].Pos
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
		pk = append(pk, PickupSnap{Pos: w.pickups[i].Pos, Kind: w.pickups[i].Kind})
	}
	return ts, sh, fl, pk
}
