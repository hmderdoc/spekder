# Phase C — In-door Map Editor

The last of the three phases: a human-curated level editor living in OPTIONS →
MAP EDITOR. Goal (the user's): "a basic deathmatch in under 10 minutes, something
crazy in 10 hours." Fast default path + progressive disclosure, over the frozen
map schema (`SCHEMA.md`).

## What the user asked for

- Switch between **top-down** and **3D** views, like the game (TAB).
- A **catalog/palette** of assets to drop in — anything: walls, ramps, platforms,
  turrets, hazards, teleporters, trampolines, enemies, pickups, spawns, objectives.
- A **list of placed items**, selectable and editable.
- **Edit panels** for a placed item's behavior (trait params), placement, size, etc.
- Tools to build **extended walls / platforms / ramps**.
- **Two placement methods**: top-down (X/Z cursor) and 3D (gun-sight raycast, like
  Minecraft — aim where you want it and place).
- Edit items after placing. Clear, sensible menus.

## Architecture

An `editorSession` parallel to the game session: it owns a working `gm.Map` and
editor state, and reuses the existing renderer to draw it.

- **Rendering**: reuse `buildArena` + `renderWorld`-style passes to draw the map's
  static geometry and a *live overlay* of entities (so authored turrets/zones show
  as they will in game), plus editor-only overlays: the cursor/ghost preview and a
  selection highlight. Both views work: TAB toggles top-down vs 3D, same as play.
- **Camera**: free-fly in 3D (move on the plane + raise/lower + look), and a grid
  cursor in top-down. (Editor movement is free-fly, *not* tank-style — you need to
  place at any spot/height; this is the one deliberate difference from gameplay.)
- **Modes**: NAVIGATE (move around) · PLACE (a chosen asset ghosts at the
  cursor/crosshair; confirm to drop) · EDIT (a placed object is selected; a panel
  edits its fields).

## The catalog (asset palette)

Grouped, each entry a template with sane defaults the user then tweaks:

- **Structure**: Wall/Box (resizable), Ramp, Platform (a wide low raised box preset).
- **Hazards / utility**: Hazard pad, Teleporter (place pad, then set its dest),
  Trampoline.
- **Combat**: Turret (composable with Destruct + Respawn toggles).
- **Objectives**: Flag (neutral / team 0 / team 1), Zone (King of the Hill).
- **Markers**: Spawn point, Pickup spot.

Maps to the schema directly: Box→obstacles, Ramp→ramps, the trait entities→
entities, spawns/pickups→their arrays.

## Placement

- **Top-down**: an X/Z cursor moved by the keys, snapped to a grid (toggleable),
  with a height setting for elevated pieces. Drop at the cursor.
- **3D (gun-sight)**: a free-fly camera; the crosshair raycasts to the floor (or
  the top of an object under it) and the ghost previews there; drop at that point.
- **Extended walls / platforms**: place a start point, then an end point — the
  editor spans a box between them (length + a set thickness/height). Same gesture
  for a ramp's run.

## Editing a placed item

- **Select**: cursor-over (top-down) or aim-at (3D), or cycle selection through the
  placed list. Selected item gets a highlight.
- **Edit panel**: lists the item's fields by type — position (x,y,z), size (half
  x,y,z) for boxes, color, and trait params (turret range/dmg/turn/fireDelay,
  hazard dps, teleport dest, destruct hp, respawn delay, bounce power, flag team,
  zone capture). Navigate fields, adjust values, toggle traits on/off. Delete removes.

## Save / load / playtest

- **Save**: serialize the working `gm.Map` to JSON (new writer; see prerequisites),
  run `gm.ValidateMap` first and surface any issues, write to the author-maps dir.
- **Load**: pick an existing author map to edit (`gm.ParseMapJSON`).
- **Playtest**: jump straight into an offline match on the working map (pin it),
  then return to the editor.

## Prerequisites (not yet in the codebase)

1. **Map writer** — `gm.MapJSON(m) []byte` (Map → jmap → JSON). The reader
   (`ParseMapJSON`) exists; this is its inverse. Round-trip tested.
2. **Author-map loading in the door** — today only the arena server `LoadMapDir`s
   author maps; the offline door uses only embedded maps. Load an author-maps dir
   (next to the binary) at door startup so editor-made maps are playable/pinnable
   offline. Editor saves there.

## Build stages (each ends usable)

1. **Foundations** — map writer + round-trip test; door loads author maps; OPTIONS
   → MAP EDITOR opens an editor that renders a (blank or loaded) map in both views
   with free-fly nav + top-down cursor and a help/status bar. No placing yet.
2. **Placement** — catalog palette + ghost preview + drop in both modes, for the
   core assets (box/wall, ramp, turret, hazard, teleporter, trampoline, spawn,
   pickup).
3. **Select + edit** — selection, the edit panel (pos/size/color/trait params),
   delete.
4. **Objectives + build tools** — flag/zone placement; extended-wall / platform /
   ramp drag gestures; grid-snap toggle; inline validation display.
5. **Save / load / playtest** — write to author maps, load existing, jump to play.

This is the largest single feature in the project — realistically several sessions.
Stage 1 (foundations) is the concrete starting point: it gets the editor on screen,
moving, and able to save, which everything else builds on.
