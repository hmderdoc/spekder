# Difficulty Settings — design

> **Status: shipped.** BotProfile tiers (EASY..ULTIMATE) + per-bot variation drive
> the bot AI; selectable from OPTIONS → Difficulty; saved per BBS user. The doc
> below is the design of record. Deferred: evasion, teleporter-use, smarter
> pathfinding (a later behavioral pass).


Adds an **OPTIONS** menu with **Difficulty** (this doc), plus Map Editor and
Controls as later items. Difficulty makes the bots tunable across five tiers so
single-player isn't punishing — the game's main "it's too hard" complaint.

Principle (same as the rest): a fixed set of parameters (a `BotProfile`) the bot
AI reads, selected by tier. Not scripting.

## BotProfile

```go
type BotProfile struct {
    Name         string
    Sight        float64 // target acquire/track range (0 = unlimited)
    ReactDelay   float64 // sec to react when a target is (re)acquired
    TrackRate    float64 // turret tracking speed (rad/s)
    Wobble       float64 // per-shot random aim error (radians, ~stddev)
    FireDelayMul float64 // bot reload multiplier (>1 = slower)
    SeekPickups  bool    // diverts to grab nearby power-ups
}
var BotProfiles = []BotProfile{ /* Easy … Ultimate, indexed by tier */ }
```

The World holds the active profile; the bot AI reads it instead of today's
hardcoded `turretRate` / `botFireRange` / `botAimTol` / `botFireDelay` /
`botKeepDist`. Difficulty is a **sim parameter** — it never rides the wire.
(Arena bot difficulty is the sysop's server config; the menu setting governs
offline single-player.)

**Tune for fun, not for the past.** The current behavior is a starting point, not a
baseline to preserve — we change it freely to improve playability and rewrite tests
to match the *intended* behavior. Tests assert tier *relationships* (e.g. EASY bots
miss more and react slower than HARD) and mechanics, not a frozen legacy feel. The
new-player default is NORMAL (gentler than today).

## Per-bot variation (don't be static)

Tiers alone would just give a *uniform* wall at five heights — every bot identical
and predictable, which is a big part of why it isn't fun today. So each bot also
rolls **per-bot jitter** within its tier at spawn, varying the static-feeling knobs
so opponents feel individual and unpredictable:

- engagement distance (today every bot holds the exact same `botKeepDist`),
- reaction delay and turret track rate,
- aim wobble,
- a small aggression lean (push-in vs hold-and-poke).

Same idea over time, not just per-bot: a bot's aim/aggression can drift slightly so
it doesn't behave like a fixed turret. The profile sets the tier's *center*; the
jitter spreads bots around it. This is what turns "five static walls" into five
bands of varied opponents.

## Tiers (rough starting values; tune in playtest)

Today's bots are already *hard*, so the current behavior maps to **HARD**, with only
one tier above it (ULTIMATE) and the ladder ramping easier below — that's where the
real need is.

| Tier      | Sight | ReactDelay | TrackRate | Wobble | FireDelayMul | SeekPickups |
|-----------|-------|------------|-----------|--------|--------------|-------------|
| EASY      | 14    | 0.9        | 1.2       | 0.20   | 1.9          | no          |
| BEGINNER  | 19    | 0.6        | 1.7       | 0.12   | 1.5          | no          |
| NORMAL    | 26    | 0.35       | 2.1       | 0.07   | 1.2          | no          |
| HARD      | 0     | 0.10       | 2.6       | 0.02   | 1.0          | no          |
| ULTIMATE  | 0     | 0.0        | 3.3       | 0.00   | 0.8          | yes          |

**HARD** is roughly today's lethality (track 2.6, full sight, near-perfect aim) — but
even HARD gets per-bot variation so it's not the current uniform wall. **NORMAL is
the new-player default** (gentler than today). EASY is beatable by anyone; ULTIMATE
is the one step up — a wall that also seeks pickups. These are *centers*; per-bot
jitter spreads each tier into a band. All values are starting points to tune in
playtest, not anything to freeze.

## Bot-AI integration points (v1: cheap four + pickups)

- **Sight** → `nearestEnemy`: ignore targets beyond `Sight` (0 = unlimited). Low
  tiers go blind at distance, so opening range loses them.
- **Wobble** → at the moment of firing, perturb the shot direction by a random
  offset ~`Wobble`. Bots miss by tier regardless of alignment. (Biggest lever.)
- **ReactDelay + TrackRate** → per-bot re-acquire timer: when the target changes,
  hold tracking/fire for `ReactDelay`; otherwise track the turret at `TrackRate`.
  Together = "you can juke them."
- **FireDelayMul** → multiply a bot's reload (only bots; humans unchanged).
- **SeekPickups** → if set and a power-up is within a seek radius, divert toward
  it (prioritise repair when hurt). Smart tiers heal/shield; dumb tiers ignore.

Deferred (later behavioral pass): evasive maneuvering, teleporter use, smarter
pathfinding (current whisker-avoid stays).

## Persistence — per BBS user

Each caller's pick is saved keyed by the DOOR32 user (record number / handle from
DOOR32.SYS), so difficulty is personal on a multi-user board:

- Read the user id/handle from the dropfile (extend the existing DOOR32 reader).
- Store under a settings dir next to the door, e.g. `data/<userkey>.ini` with
  `difficulty=<tier>`. Read at launch (default NORMAL), written when changed.
- Offline-only concern; no wire involvement.

## Options menu shell

A top-level **OPTIONS** entry on the main menu opens a sub-menu:

1. **Difficulty** — tier picker (this doc); writes the per-user setting.
2. **Map Editor** — Phase C (stub "soon" until built).
3. **Controls / Key bindings** — later.

The chosen difficulty flows into `newOfflineSession` → the World's `BotProfile`.
