# Vehicle revamp — design

Goals (from the user): a richer **selection screen** — lightbar list on one side,
a **rotating 3D preview** of the selected vehicle on the other; **more vehicle
options including a CUSTOM one**; and **picking your color** (if not already taken
by another player). The ammo knobs (`AmmoMax`/`AmmoRegen`) are already on the
`Vehicle` struct from W5b, so the stat model is in place.

## What exists today

- `Vehicles []Vehicle` (SCOUT/HUNTER/HEAVY), each: HP, Speed, HullTurn, AimTurn,
  FireDelay, Jump, Scale, AmmoMax, AmmoRegen.
- `runVehicleMenu` is a static draw + key-wait (centered list, stat line).
- Color: `AddPlayer(color, vehicle, name)` already accepts a color and auto-picks
  from `PlayerPalette` when zero. `TankSnap.Color` rides the wire. HELLO carries
  token/bbsid/handle/vehicle (no color yet).
- The renderer draws a scene into a `Renderer{W,H,fb,zb}`; `renderWorld` sets the
  camera and rasterizes. `appendTank` builds a vehicle's model.

## Selection screen (two-pane, live)

`runVehicleMenu` becomes a small real-time loop (~20fps):

- **Left pane:** the lightbar list (vehicles + CUSTOM), with the selected one's
  stat block. Left-aligned (start near col 2), width ~ a third of the screen.
- **Right pane:** a **rotating preview**. A dedicated panel `Renderer` sized to
  the pane renders just the selected vehicle (a `TankSnap` with that vehicle +
  the chosen color) on a small ground pad, with the camera **orbiting** (angle
  advances each frame). A new `blitPanel(w, r, col0,row0)` emits the panel's
  half-block cells at a screen offset (full paint each frame — the pane is small).
- Up/down change selection (re-targets the preview), `C` opens the **color
  picker**, ENTER confirms, ESC quits. On narrow terminals (< ~70 cols) fall back
  to today's centered list (no preview).

## More vehicles + CUSTOM

- Add a few presets to round out the roster (e.g. a glass-cannon **ARTILLERY**:
  low HP, slow, big FireDelay but high damage/ammo; a **RANGER**: balanced-fast).
  Final stats TBD with the user.
- **CUSTOM** = a point-buy build, the last entry in the list. Selecting it and
  pressing ENTER (or a key) opens a **point-buy editor**: a fixed budget spread
  across stats, each with a cost; live preview updates as you tune. Saved
  per-BBS-user (same pattern as difficulty/aim-assist in `userSettings`, keyed by
  the DOOR32 handle) so your build persists between calls.
  - Tunable stats + suggested ranges: HP, Speed, HullTurn, FireDelay, AmmoMax,
    AmmoRegen (Jump/Scale derived or fixed). A budget keeps it balanced; over/under
    is clamped. Custom is appended to `Vehicles` at runtime for that session, or
    carried as a `Vehicle` value on the session — **decision below**.

## Color picking

- A palette (≥ the player palette, maybe 8–12 swatches). The menu shows swatches;
  pick one with the arrows/`C` cycle.
- **Offline:** any color is free (bots use `BotPalette`; the human just picks).
- **Online "not taken":** add the chosen color to **HELLO** (wire change); the
  server passes it to `AddPlayer`, which **dedups** — if another connected tank
  holds that color (within a threshold), it shifts to the nearest free palette
  entry. The client then sees its assigned color in its own `TankSnap`. This
  avoids a pre-connect "who has what" query; the server is the authority. (The
  menu can't gray out taken colors before connecting, so it offers a *preference*.)

## Render approach (preview)

- `buildArena` a tiny floor-only scene once before the loop (the menu has no active
  map). Each frame: clear the panel renderer, set an orbit `Cam` around the model,
  `renderWorld(cam, t, []TankSnap{preview}, nil…, -1, 0, false, reticle, 0)`,
  then `blitPanel`. Reuses the whole existing render path; no new rasterizer.

## Build phases (each shippable)

1. **V1 — two-pane screen + preview** for the existing vehicles (+ the new
   presets). The visual centerpiece; no new systems.
2. **V2 — color picking** (palette + offline free + HELLO color + server dedup).
3. **V3 — CUSTOM point-buy** vehicle + per-user persistence.

## Shipped

All three phases are in:

- **V1** two-pane selector: lightbar + class blurb on the left, rotating 3D preview
  (black backdrop) on the right, stats in the preview's black space. Each class has
  a distinct silhouette via `bodyShape`/`bodyFor` (render_world.go). New presets:
  RANGER, ARTILLERY.
- **V2** color: a 10-swatch palette (`gm.SelectColors`) picked with `</>`; offline
  any color is free; online the pick rides HELLO and `World.freeColor` dedups it
  against other *players* (bots don't reserve).
- **V3** CUSTOM (works online): a build = a **chassis** (builtin index, for
  body/scale/render — every client renders it by index, no desync) **plus six tuned
  sim stats** carried as a per-tank override (`Tank.custom`, accessor `Tank.veh()`).
  HELLO carries an optional `gm.CustomStats` block; the server rebuilds it with
  `gm.MakeCustom(chassis, stats)` and spawns via `AddPlayerCustom`. Point-buy: 6
  stats (HP/SPEED/TURN/FIRE/AMMO/REGEN), each 0..8 pips, total capped at **22 pips**
  (`pbBudget`); persisted per-user as `customchassis`/`customlevels` (settings.go,
  vehicle_custom.go). Custom builds never enter the shared `Vehicles` table, so bots
  never roll one.

Decisions as resolved: one pip budget with per-stat clamps; server-dedup color;
RANGER + ARTILLERY presets; build order V1 -> V2 -> V3.
