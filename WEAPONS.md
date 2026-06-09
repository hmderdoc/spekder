# Weapons & Effects — design

A data-driven weapon system (like the trait palette and the ruleset table): a
weapon is *data*, firing is parameterized by a `WeaponDef`, and a projectile
carries an *effect payload* that does something on hit — damage, but also heal,
slow, boost, shield-bust, knockback, teleport, etc. The payoff the user keeps
circling: **assigning these effects in the level editor** (a heal-turret, a
slow-field mine), and **support/debuff roles** for players and bots.

## What exists today (build on this)

- `Projectile{Pos, vel, life, owner, dmg}` — a single straight bolt. `fire(owner)`
  spawns one along the aim; cooldown = the vehicle's `FireDelay` (× rapid/bot mul).
  `fireEntity(e)` is the turret version (owner −1, dmg from `TurretTrait`).
- `stepProjectiles` moves shots and resolves hits → `hurt(ti, dmg, owner, cause)`.
- **Effects already half-exist as timed buffs:** `Tank.shieldT/rapidT/cloakT`,
  `PickRepair/Shield/Rapid/Cloak`, `applyPickup`, decayed in `stepPickups`. New
  effects extend this same machinery.
- Online is **server-authoritative**: clients render shots, the server resolves
  hits. So effect logic lives server-side and barely touches the wire.

## Model

### WeaponDef (the palette)

```
type WeaponDef struct {
    Name     string
    Delivery Delivery // how it reaches the target
    Damage   int      // 0 = no damage (pure-effect weapons, e.g. a heal gun)
    Speed    float64  // projectile speed (0 = hitscan/instant)
    Arc      float64  // gravity for lobbed shots (0 = flat)
    Life     float64  // projectile lifetime / range
    Cooldown float64  // recharge between shots (overrides vehicle FireDelay)
    Blast    float64  // splash radius (0 = direct hit only)
    Ammo     int      // 0 = infinite; else magazine size
    Reload   float64  // sec to refill a spent magazine
    Effect   Effect   // payload applied on hit (see below)
    Affects  Target   // who the effect applies to: foes / allies / both
    Glyph    byte     // wire-cheap visual hint (bolt / grenade / beam / mine)
}

var Weapons []WeaponDef // built-in palette, referenced by INDEX (synced like Rulesets/Maps)
```

`Delivery`: `Bolt` (today's straight shot), `Lob` (grenade arc, different aim
feel), `Mine` (dropped, proximity/timer trigger), `Beam` (hitscan laser, instant).
Start with `Bolt`; the rest are Stage W4.

### Effect (the payload)

```
type Effect struct {
    Kind EffectKind
    Mag  float64 // magnitude (heal amount, slow %, knockback force, ...)
    Dur  float64 // seconds, for timed effects
}
```

`EffectKind` palette (a fixed enum, like `WinKind`):
`EffNone, EffDamage, EffHeal, EffSlow, EffSpeed, EffShield, EffShieldBust,
EffDamageUp, EffDamageDown, EffKnockback, EffTeleport`.

Most map onto **timed Tank fields** (extend the buff set): `slowT/slowMag`,
`dmgMul+timer`, etc., decayed in `stepPickups`, read by movement/`fire`. `EffDamage`
routes through `hurt`; `EffHeal` is negative damage (capped at MaxHP); `EffShieldBust`
zeroes `shieldT`; `EffKnockback` shoves position; `EffTeleport` is the "grab and
move" gun.

### Targeting (friend / foe)

`Affects` = `Foes | Allies | Both`. Support weapons (heal/shield/speed) target
**allies**; debuffs/damage target **foes**. This only has teeth in **team modes**
(FFA has no allies, so a heal-gun there is just inert/self) — acceptable; that's
where support roles belong anyway.

## Where weapons attach

- **Tanks/vehicles:** a `primary` and optional `secondary` weapon index. Most
  vehicles = `{CANNON, none}` today. Secondary fires on a new key.
- **Turret entities:** reference a weapon index (the `TurretTrait` becomes a thin
  wrapper over a WeaponDef, or gains a `Weapon int`). A "heal turret" or "slow
  mine" is just a turret/entity pointing at that palette entry.
- **Pickups:** weapon swaps / ammo / damage-up drops extend `PickKind`.

## Editor integration (the payoff)

- The entity edit panel gains a **weapon field** (cycle the `Weapons` palette) on
  turrets — so a mapper drops a turret and sets it to `SLOW FIELD` or `HEAL`.
- New catalog entries can be weapon-flavored turrets/mines for the fast path.
- **Custom weapon authoring** (define new `WeaponDef`s in-editor, point-buy style)
  is deferred — v1 assigns from the fixed palette by index, which is enough to get
  "interesting stuff" in maps.

## Online / wire

- Effects are **resolved server-side** in `stepProjectiles` (clients don't compute
  hits), so the wire barely changes. Projectiles optionally carry a 1-byte `Glyph`
  so the client can render a grenade/beam/mine differently from a bolt (pure
  polish; can land later). Weapon *assignments* travel inside the map (`MsgMap`),
  which already extends cleanly.

## Controls

- **`b` = fire secondary** (the user's pick — under the off-hand, near space). New
  `aFire2` action + `Input.Fire2` + per-tank `cooldown2`. Custom-controls work
  (the OPTIONS stub) will let this be rebound later.

## Build stages (each ends shippable)

- **W1 — model + refactor (no behavior change):** `WeaponDef` + `Weapons[]` with
  one entry (`CANNON` = today's bolt). `fire`/`fireEntity` → `fireWeapon(owner,
  def)`. Projectile carries the effect fields. Tests prove unchanged behavior.
- **W2 — effect palette (server-applied):** wire `EffDamage` (default) + a few
  real ones (`Slow`, `Heal`, `Knockback`, `ShieldBust`) via extended buff timers;
  friend/foe targeting. Add a couple of palette weapons that use them.
- **W3 — secondary weapon + `b` key:** per-tank primary/secondary indices, second
  cooldown, HUD indicator.
- **W4 — delivery kinds:** `Lob` (arc), `Mine` (drop+trigger), `Beam` (hitscan),
  `Blast` AoE; projectile `Glyph` on the wire for distinct rendering.
- **W5 — editor assignment + ammo/pickups:** weapon field on turret entities;
  weapon/ammo/damage-up drops; magazine + reload.

Deferred: custom weapon authoring in-editor; role-based teamwork AI (bots wield
support weapons as support — needs this system first); custom vehicles (point-buy).

## Open decisions

1. **First visible payoff** — build editor-assigned **turret/map effects** first
   (W1→W2→editor field), or **player secondary weapons** first (W1→W3)? The notes
   lean on the editor ("where you'd see interesting stuff"); I'd do that first.
2. **Palette scope for v1** — fixed built-in weapon palette (assign by index) now,
   custom authoring later. (Recommended.)
3. **Effect set for W2** — which 3-4 effects to wire first as the proof
   (suggested: Slow, Heal, Knockback, ShieldBust).
