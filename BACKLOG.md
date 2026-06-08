# Spekder — Backlog & Ideas

Living list of planned work and design notes. Items here are not yet scheduled;
they capture intent so nothing is lost between sessions. Newest themes first.

## Recently shipped (context)

- **Map entity / trait palette** — authored map objects assembled from composable
  traits: `turret`, `destruct`, `respawn`, `hazard`, `teleport`, plus a `solid`
  flag. Authored in map JSON, synced over `MsgMap`, dynamic state over `MsgState`
  (`EntitySnap`), collidable on both server and client predictor. Demo map:
  `internal/game/maps/07-redoubt.json`.
- **Vertical aim (full ballistic 3D)** — `TurretPitch` on tanks + turret entities;
  up/down arrows elevate/depress; shots fly along the aimed 3D direction;
  height-aware hit detection; bots & turrets compute elevation toward the target;
  camera tilts with aim. C recenters and levels.
- **Offline map pin** — `map = <name|index>` in spekder.ini/door.ini (or
  `SPEKDER_MAP` env) pins single-player to one map. Stopgap until a real picker.
- **Verticality (partial)** — `bounce` trait (trampoline / jump pad, fixed launch
  impulse) added to the palette; ramps authored into a map for the first time
  (`08-ascent.json`, which also showcases vertical aim against a rooftop turret).
- **Phase B (complete)** — modes are data: a `Ruleset` table replaces the
  hardcoded mode switches; objectives are entity traits (`flag`, `zone`) with
  procedural fallbacks; seven modes now (DM, Flag Run, CTF, Survival, Elimination,
  Team KotH, King of the Hill), the new ones added almost entirely as table
  entries; menu + HUD + scoreboard + arena lobby all render from the ruleset.
  See `PHASE_B.md`.
- **Phase A consolidation** — map format frozen at schema `version: 1`; authoring
  contract written (`SCHEMA.md`); `game.ValidateMap` (severity-tagged issues,
  editor-consumable) wired into map loading + tested on every embedded map;
  `cmd/mapcheck` CLI; `game.ParseMapJSON` for tools/editor.

## Decisions (locked)

- **Phases**: A (entity/trait model) done -> B (rules/win-conditions as data) ->
  C (editor). Editor comes AFTER B so it can author rules, not just place objects.
- **Palette, not scripting** — applies to Phase B too: win conditions and mode
  mechanics are a fixed, parameterized set, never a scripting VM.
- **Objectives become entity traits** (Phase B): flags / capture zones / control
  points join the trait palette; rulesets reference them. Unifies authoring and
  the editor. Requires refactoring the current CTF/flag/wave systems.
- **Arenas stay square** — `size` is a single per-map half-extent. Rectangular
  rejected: keeps radar/scaling honest and avoids symmetry/bounds edge cases;
  use obstacles + spawn placement for corridors.

## Backlog

### Bot AI: difficulty tiers
Tiers (e.g. Easy / Beginner / Normal / Hard / Ultimate) selectable from a new
**Game Options** view off the main menu. A tier is a parameter set — cleanest as
a `BotProfile` struct the World holds, replacing today's scattered bot constants
(`botAimTol`, `botFireRange`, `botFireDelay`, `turretRate`, pitch-tracking rate).
Scaling axes:
- **Aiming latency** — reaction delay before tracking a new target, so the player
  can break line of fire.
- **Range / sight** — engagement/acquire distance cap (prevents bot sniping).
- **Aim wobble** — chance and magnitude of aim error.
- **Decision-making** — item pickup seek + usage, pathfinding / obstacle handling,
  teleporter usage.
- **Maneuver bias** — evasive vs. offensive.

### Bot jump AI
Bots never use the jump mechanic. Add a `canJump` gate + situational logic
(cross a gap, mount a ledge, dodge a shot). Most useful once maps have real
verticality — pairs with the verticality item and the difficulty tiers
(decision-making axis). Likely lands as one "bot brain" pass.

### Verticality content
- **Ramps** — DONE: authored into `08-ascent.json`. More maps could use them.
- **Trampoline / bounce trait** — DONE (fixed launch impulse).
- **Landing on obstacle tops** — supported via `GroundHeight`; still want a
  play-test pass to confirm it reads/feels right with ramps + trampolines now in.
- **Open question**: trampolines/jumps only move tanks with vertical physics
  (players). Bots snap to ground each tick, so they ignore launches — revisit
  when bot-jump / bot vertical movement lands.

### Map selection UX
Replace the `SPEKDER_MAP` / `map=` stopgap with a real picker — in-door map
list with preview (e.g. an `L` load key, or a menu entry), and eventually a
load→preview→play flow that the map editor can share.

### Ammo / magazine model
Currently firing is gated only by reload cooldown (effectively infinite ammo).
Two independent, additive features:
- **Magazine** — `MagSize` + `ReloadTime` on `Vehicle` and `TurretTrait`: short
  cadence within a clip, longer reload when empty. Reticle reload-gauge already
  exists to visualize it.
- **Finite ammo** — a per-tank counter, refilled by a new `PickAmmo` power-up
  and/or an ammo-crate entity.
Both should be authorable per-object in the eventual map editor.

### Weapon effects / projectile payloads
A projectile's effect need not be damage. Generalize the shot's payload so a
hit can instead (or also): slow the target, teleport them, boost/launch them,
apply a buff/debuff, push (knockback), etc. Think of it as an effect attached to
the projectile (and authorable per weapon / per turret). Pairs with the
ammo/weapon-type and difficulty work; turrets and pickups could grant alternate
"ammo" with different payloads. Hit-detection already resolves the target tank;
this is about what happens on hit beyond `hurt()`.
- **Team support / healer**: a heal payload that restores a friendly tank's HP
  enables a medic/support role in team modes (CTF and future team modes). Implies
  friendly-target resolution (today friendly fire is ignored in CTF) and a
  positive `hurt()` counterpart (e.g. `heal()`).
- **Projectile types**: eventually distinct shot kinds (visuals + speed/arc +
  payload) rather than one bullet — e.g. heal beam, slow round, knockback shell.
  The payload work is the foundation; types are the authorable presets on top.

### Map / entity / trait schema docs
DONE — see `SCHEMA.md` (the authoring contract) and `cmd/mapcheck` (validator CLI).

### Map editor
The bigger goal: human-curated level authoring. "Basic deathmatch in <10 min,
something crazy in 10 hours." Fast default path (drop spawns, pick a rule
template) over progressive disclosure (per-object trait params). In-door ANSI
editor is the native home; falls out of the data model + a selection/preview UX.

### Rule / win-condition layer (Phase B)
Re-express the four hardcoded modes (DM / Flag Run / CTF / Survival) as data so
new modes emerge from composed win-condition predicates rather than Go code.
The endgame of the "flexible sandbox" direction.
