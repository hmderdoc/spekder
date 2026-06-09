# Spekder Map Schema

The authoring contract for arena maps. A map is a JSON file describing a static
arena: geometry, spawn points, power-up spots, and trait-driven entities. Maps
live in `internal/game/maps/*.json` (embedded into the build) or are loaded at
runtime from a directory by the server (`LoadMapDir`).

Validate any map with the `mapcheck` tool:

    go run ./cmd/mapcheck internal/game/maps/07-redoubt.json

The same checks (`game.ValidateMap`) run when the server loads author maps and in
the test suite for the embedded set, so this document and the validator agree.

## Coordinates & units

- Right-handed-ish: **X** = east/west, **Z** = north/south, **Y** = up.
- One unit ≈ one tank radius. A tank's eye height is ~1.35.
- Arenas are **square**, centered on the origin. `size` is the **half-extent**:
  the playable area runs from `-size` to `+size` on both X and Z (default 22).
  (Square keeps the radar honest and scaling uniform; make corridors with
  obstacles and spawn placement rather than a rectangular bound.)
- Positions are `[x, y, z]`; box extents are `[hx, hy, hz]` (half-sizes, so a
  box spans `pos ± half`). Spawn/pickup spots are `[x, z]` (ground level).
- Colors are `[r, g, b]`, each 0.0–1.0.

## Top-level fields

| field       | type             | notes |
|-------------|------------------|-------|
| `version`     | int                | schema version (current: 3). Absent = 1. |
| `name`        | string             | **required**; shown in selection, sent on the wire. |
| `size`        | number             | arena half-extent. 0/absent = 22. |
| `obstacles`   | array of Box       | solid, collidable blocks (cover, walls, pillars). |
| `ramps`       | array of Ramp      | drive-up sloped surfaces. |
| `scenery`     | array of Prop      | decorative only (no collision). |
| `spawns`      | array of `[x,z]`   | tank start points. 0 = random fallback. |
| `pickups`     | array of `[x,z]`   | **v1/v2 legacy**: untyped power-up spots (read-only; load as "any"). |
| `pickupSpots` | array of PickupSpot | **v3**: typed power-up spots (written by the editor). |
| `entities`    | array of Entity    | trait-driven objects (turrets, hazards, …). |
| `rules`       | Rules (optional)   | **v2**; per-map victory conditions. Absent = implied by objectives. |

### PickupSpot (v3)

Where a power-up appears, and what it is. A spot with `kind: -1` ("any") behaves
like the old untyped spot — the periodic spawner picks a random power-up there.

| field    | type       | notes |
|----------|------------|-------|
| `pos`    | `[x,z]`    | location. |
| `kind`   | int        | `-1` = any (random); else a power-up: 0 repair, 1 shield, 2 rapid, 3 cloak, 4 ammo, 5 weapon. |
| `weapon` | int (opt)  | for `kind: 5` (weapon): which weapon to grant (0 = random). |

Saving any map (re)writes pickups in the `pickupSpots` form; the legacy `pickups`
array is still read so older maps load unchanged.

### Rules (v2, optional)

Overrides how the map is played. Every field uses `-1` to mean "use the mode's
default", so a v1 map (no `rules`) behaves exactly as before. Set by the editor's
RULES panel (FILE → RULES).

| field       | type   | notes |
|-------------|--------|-------|
| `mode`      | int    | mode index; `-1` = auto (implied by objectives — team flags→CTF, zone→KotH, neutral flags→Flag Run, else Deathmatch). |
| `timeLimit` | number | match seconds; `-1` = mode default, `0` = endless. |
| `target`    | int    | win count (frags / captures / hold-points); `-1` = default. |
| `lives`     | int    | per-tank lives; `-1` = default, `0` = infinite. |

The mode (explicit or auto) determines the *win family* (frags/captures/collect/
elimination/hold-score); `target`/`timeLimit`/`lives` tune the numbers. Applied
both offline and online (the server honors a published map's rules).

### Box (obstacle)
```json
{ "pos": [0, 1.2, 0], "half": [2.2, 1.2, 2.2], "color": [0.42, 0.44, 0.52] }
```
Tanks collide with the sides and can stand on top (within a small step height).
A shot passing over a box shorter than the shot's height flies over it.

### Ramp
```json
{ "pos": [-6.5, 0, 0], "half": [2.5, 0, 4.0], "h": 3.2, "dir": "+x",
  "color": [0.38, 0.40, 0.48] }
```
A wedge rising to height `h` toward `dir` (`"+x"`, `"-x"`, `"+z"`, `"-z"`). Drive
up it to reach raised ground. (`half.y` is unused; the slope uses `h`.)

### Prop (scenery)
```json
{ "kind": "obelisk", "pos": [0, 0, 0], "h": 7, "color": [0.85, 0.30, 0.60] }
```
Decorative, non-colliding. `kind` selects the model (currently `"obelisk"` — a
pyramid; one placed at the origin slowly spins).

## Entities

An entity is a placeable object assembled from a fixed palette of **traits**. A
single entity may compose several traits (e.g. a turret that is also destruct +
respawn = a shootable gun emplacement that rebuilds). Absent trait = absent.

| field   | type      | notes |
|---------|-----------|-------|
| `kind`  | string    | render selector: `"turret"`, `"trampoline"`, else a generic block. |
| `pos`   | `[x,y,z]` | center. |
| `half`  | `[hx,hy,hz]` | box half-extent (collision footprint + default visual). **Must be > 0 on every axis.** |
| `color` | `[r,g,b]` | base tint. |
| `yaw`   | number    | initial facing (radians); turrets track from here. |
| `solid` | bool      | if true, collides like an obstacle **while alive**. |
| trait blocks | object | `turret`, `hazard`, `teleport`, `destruct`, `respawn`, `bounce` (below). |

### Trait: `turret`
Tracks and shoots the nearest live tank in range (ignores cloaked tanks),
elevating its gun toward the target.
```json
"turret": { "range": 22, "fireDelay": 1.4, "dmg": 16, "turnRate": 1.6 }
```
| field | notes |
|-------|-------|
| `range` | engagement radius (**> 0**). |
| `fireDelay` | seconds between shots (0 = default). |
| `dmg` | damage per shot (0 = default projectile damage). |
| `turnRate` | barrel tracking speed, rad/sec (0 = default). |

### Trait: `hazard`
Burns any tank standing in the footprint (shield / spawn-guard protect).
```json
"hazard": { "dps": 22 }
```
`dps` = damage per second (**> 0**).

### Trait: `teleport`
Warps a tank that drives into the footprint to `dest`, then debounces.
```json
"teleport": { "dest": [16, 0, 0], "cooldown": 1.5 }
```
`dest` should be inside the arena; `cooldown` = pad re-arm seconds (≥ 0).

### Trait: `destruct`
Gives the entity hit points; projectiles damage it and destroy it at 0.
```json
"destruct": { "maxHp": 60 }
```
`maxHp` **> 0**. Without `respawn`, a destroyed entity is gone for the match.

### Trait: `respawn`
A destroyed entity returns at full HP after `delay` seconds.
```json
"respawn": { "delay": 14 }
```
Only meaningful alongside `destruct` (nothing destroys it otherwise).

### Trait: `bounce` (trampoline / jump pad)
Launches a tank that touches the footprint straight up with a fixed velocity;
standing on it re-launches each time you land.
```json
"bounce": { "power": 13 }
```
`power` **> 0** (units/sec; ~13 reaches noticeably higher than a normal jump).

### Trait: `flag` (objective marker)
Marks where an objective flag spawns. The active ruleset instantiates a runtime
flag here at match start: a Flag Run mode uses the neutral ones, a CTF mode uses
the team ones. The entity itself is an inert marker — it doesn't render or
collide; the runtime flag does. If a map defines no `flag` entities for the mode,
flags are placed procedurally (scatter / team bases) so legacy maps keep working.
```json
"flag": { "team": -1 }   // -1 = neutral (Flag Run); 0 or 1 = CTF team flag
```
`team` must be **-1, 0, or 1**. Give a flag a tiny non-zero `half` (e.g. `[0.5,
0.5, 0.5]`) so it passes validation, even though it isn't drawn.

## Validation

`game.ValidateMap` returns issues with a severity:

- **ERROR (fatal)** — the map won't work; the server skips it on load. Examples:
  missing `name`, negative `size`, an entity `half` ≤ 0 on any axis, `turret.range`
  ≤ 0, `hazard.dps` ≤ 0, `destruct.maxHp` ≤ 0, `bounce.power` ≤ 0, negative timings.
- **warn** — likely a mistake but not breaking. Examples: no `spawns`, a spawn /
  pickup / entity / teleport `dest` outside the arena, `respawn` without `destruct`,
  a `version` newer than this build supports.

### Trait: `zone` (King-of-the-Hill control zone)
Marks a control zone. In a KotH ruleset, a single uncontested contender (a team
in team modes, a lone tank in FFA) standing in the footprint captures it over
`capture` seconds; the controller then accrues a hold-point per second. Inert
marker — the runtime zone renders (a pad tinted by the controller, brightening
with capture progress). If a KotH map has no `zone`, a default hill spawns at the
arena center.
```json
"zone": { "capture": 4 }   // seconds of uncontested presence to flip control (0 = default ~4)
```
The footprint is the entity's `half` (X/Z). Placing a zone atop a solid obstacle
(as in `10-hilltop.json`) makes it a literal hilltop you must climb a ramp to hold.

## Modes are data

Modes themselves are data (see `PHASE_B.md`): a `Ruleset` picks team structure,
win conditions, lives, bot spawning, and which objective kind to instantiate.
Objective traits (`flag`, `zone`) are placed in maps; the ruleset wires them up.

## Example

A minimal arena with one respawning turret and a trampoline:
```json
{
  "version": 1,
  "name": "EXAMPLE",
  "size": 18,
  "obstacles": [
    { "pos": [0, 1.0, 0], "half": [2, 1, 2], "color": [0.42, 0.44, 0.52] }
  ],
  "spawns": [[-14, -14], [14, 14]],
  "pickups": [[0, 10]],
  "entities": [
    { "kind": "turret", "pos": [0, 2.3, 0], "half": [0.7, 0.3, 0.7],
      "color": [0.55, 0.50, 0.30], "solid": true,
      "turret": { "range": 20, "fireDelay": 1.5, "dmg": 14, "turnRate": 1.5 },
      "destruct": { "maxHp": 60 }, "respawn": { "delay": 12 } },
    { "kind": "trampoline", "pos": [10, 0.2, 0], "half": [1.5, 0.2, 1.5],
      "color": [0.30, 0.80, 0.55], "bounce": { "power": 13 } }
  ]
}
```
