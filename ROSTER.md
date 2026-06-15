# Vehicle / character roster overhaul

## SHIPPED (traits + new characters pass)

- **HP regen as a trait:** `bodyDef.hpRegen` - fragile fast movers recover
  (serpent/falcon 3, quad/butterfly/stag 2.5, ...), armored bruisers (tank,
  turtle, crab, t-rex, elephant) don't. Pauses 3s after taking damage
  (`regenHitPause`); shown as `RGN +N/s` in the character select.
- **New effects:** `EffPoison` (DoT) and `EffDrain` (DoT that leeches to the
  shooter); implemented the previously-declared `EffShield`/`EffSpeed`.
  One DoT slot per tank, kill-credited (`CausePoison` -> "venom").
- **Cone/melee generalized:** `coneStrike`/`meleeStrike` now run targets through
  `shotCanAffect`+`applyShotHit` (pseudo-projectile), so any delivery can carry
  any effect; they also now respect shields (was a hole). `WeaponDef.Cause`
  labels the kill feed per weapon.
- **Kits:** serpent primary -> **VENOM** (poison spit); t-rex **FLAME** = burn
  (bite + drain DoT); turtle primary -> **SNAP** (close bite), its **B = SHELL**
  (4s invulnerable + immobile toggle, 6s recharge, bots bunker at low HP);
  weapon pickups grant the turtle ammo instead of overwriting the shell.
- **New characters:** **ELEPHANT** (HEAVY; TUSKS gore + AEGIS trunk cone that
  shields allies and self-coats 60%), **FALCON** (SCOUT; flies, TALON fast
  bolts + GUST knockback cone), **STAG** (RANGER; AURA radial heal pulse that
  stings foes, SWIFT ally speed-boost bolt, antler leap-charge). Bots play all
  three (`botSpecial`, healer dispatch extended to the stag in team modes).
- **SPIDER retired** from the picker + bot pool (enum slot kept; still a
  Survival wave monster).
- **Models:** elephant/falcon/stag builders; serpent re-rigged as a cobra
  (raised hood, flicking tongue); quadruped got ears/snout/swaying tail/trot
  bob; crab pincers enlarged + raised (visible from behind); shelled turtle
  draws armor only (`curShell`). Wire: `TankSnap.Shell` (flag bit 128).

## SHIPPED (earlier pass)

- **Size (#1):** creatures normalized from measured geometry (`naturalExtent`) to a
  target overall size - `playerBodySize` floor for player-piloted, smaller
  `enemyBodySize` for Survival bots. No more tiny beasts; chassis no longer sizes a
  beast. One-line tunable.
- **Not reskins (#2):** `bodyDef` per body = signature weapon + jump override +
  muzzle + flight. Beasts fire their own weapon from the right body part; jump is now
  a real axis (gorilla 13, mantis 12, quad 11, t-rex 7, turtle 4...). Stats still
  from the chassis (light-touch).
- **Roster (#3/#4):** cut DRONE + TRIPOD (kept as dead indices for stability); added
  **BUTTERFLY** (flying healer), **MANTIS**, **TURTLE**, **T-REX**, **GORILLA**.
  Trimmed RANGER from the picker (`selectableTanks`; still in gm.Vehicles).
- **Flight + heal (#3):** `stepFlight` true hover (hold JUMP to rise, drift down,
  ceiling, floats over ground hazards). Butterfly primary = heal beam (MEDIC),
  secondary = new **HEALBOMB** (lobbed heal blast) instead of a grenade.
- **Muzzles (#5):** `bodyDefFor` muzzle points; `fireWeapon` spawns from them
  (scorpion tail, humanoid hand, t-rex jaws, ...).
- **Menu:** renamed to **CHARACTER** (title moved to the right pane), full-height
  scrolling list on the left, color strip moved to the bottom - 18 entries fit / scroll.

New render builders: `appendButterfly/Mantis/Turtle/Trex/Gorilla` (creatures.go).

## Follow-ups / refine after eyeballing

- Tune per-creature size targets + geometry proportions if any read wrong.
- Tune signature-weapon assignments + muzzle offsets (rough first pass).
- Consider promoting characters to fully self-contained stat blocks (drop the chassis
  borrow) if the chassis stats feel wrong for a beast.
- MANTIS/TURTLE could replace weaker existing beasts (octopod? insect?) if the list
  feels long - your call after seeing them.

---

# Original plan (for reference)

## 1. Size (shipped)

Creatures are normalized from their own measured geometry to a target overall size
(`naturalExtent` -> scale). Player-piloted beasts hold a higher floor
(`playerBodySize`) than Survival bots (`enemyBodySize`), so a chosen character is
never tiny and the size range is tight. Chassis scale no longer sizes a beast.

## 2. Beasts aren't tank reskins  (+ jump as a differentiator)

Add a server-side **`bodyDef`** per body: a signature **weapon** (overrides the tank
cannon), a **jump** factor, and **muzzle** origin point(s) (point 5). A beast becomes
its body identity (weapon + jump + muzzles + size), not a chassis with a costume.
Stats (HP/speed/turn/fire/ammo) still come from the chosen chassis for now -
light-touch; we can promote characters to fully self-contained stat blocks later.

**Jump** is currently uniform; make it a real axis - heavy beasts barely hop, agile
ones bound, the butterfly *flies* (sustained hover, see below).

Open: trim the **tank** roster? Proposed: keep **SCOUT / HUNTER / HEAVY** (+ maybe
ARTILLERY for siege flavor), cut **RANGER** (redundant with SCOUT). Your call.

## 3 & 4. Replace weak beasts; add a support class

- **Cut DRONE and TRIPOD** - not real creatures, no face, weak read.
- **Add BUTTERFLY** - a flying **healer/support** class: hovers/flies, fires a heal
  beam (EffHeal) at allies, fragile. New mechanics: **flight** (holds altitude
  instead of a one-shot jump; harder to hit, floats over ground hazards) + **support
  weapon**. This is the healer role (point 3) and a strong real-creature replacement
  (point 4) in one.
- Optional adds to round it out (real creatures, distinct silhouettes): **MANTIS**
  (raptorial forearms, aggressive), **TURTLE** (slow, armored, high HP). Say which,
  if any.

Proposed player roster: SPIDER, SCORPION, SERPENT, CRAB, QUADRUPED, INSECT,
HUMANOID, BUTTERFLY (+ optional MANTIS/TURTLE). (Survival can still spawn any.)

## 5. Weapon origin points (muzzles)

`bodyDef.Muzzle` = a body-local point (rotated by facing) where shots spawn, so a
creature fires from the *right place*:

| body      | fires from           | signature weapon (proposed) |
|-----------|----------------------|-----------------------------|
| SCORPION  | arched tail stinger  | laser bolt                  |
| HUMANOID  | hands (front, mid)   | cannon                      |
| SERPENT   | mouth (head front)   | venom spit (damage)         |
| SPIDER    | fangs (front, low)   | slow/web shot               |
| CRAB      | claw                 | heavy lob                   |
| INSECT    | head                 | rapid weak shots            |
| QUADRUPED | mouth (head front)   | cannon                      |
| BUTTERFLY | proboscis (front)    | **heal beam** (support)     |

`fireWeapon` already spawns at a forward muzzle; for beasts it uses `bodyDef.Muzzle`
(facing-rotated) instead, and the signature weapon instead of the cannon.

## Build order (after sign-off)

1. `bodyDef` (weapon + jump + muzzle + size target) wired into fire/jump/render.
2. Cut DRONE/TRIPOD, add BUTTERFLY (flight + heal) + any agreed adds.
3. Per-beast muzzles + signature weapons.

## Decisions for you

1. **Tanks** - trim to SCOUT/HUNTER/HEAVY(+ARTILLERY) and cut RANGER, or keep all 5?
2. **Beasts** - cut DRONE+TRIPOD confirmed? Add BUTTERFLY healer (flight + heal beam)?
   Want MANTIS / TURTLE too, or hold?
3. **Flight** - butterfly holds altitude (true flight: floats, dodgy, over hazards)
   vs just a big jump? (I'd do true flight - it's the interesting mechanic.)
4. **Signature weapons / muzzles** - OK to assign per the table above (my picks),
   and you refine specific ones after seeing them?
