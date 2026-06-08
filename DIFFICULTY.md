# Difficulty Settings — design

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
hardcoded `turretRate` / `botFireRange` / `botAimTol` / `botFireDelay`. The
unset/zero default is **HARD** (today's behavior), so tests and any path that
doesn't set difficulty are unchanged. Difficulty is a **sim parameter** — it never
rides the wire. (Arena bot difficulty is the sysop's server config; the menu
setting governs offline single-player.)

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

**HARD ≈ today's bots** (current consts: track 2.6, reload 1.0, full sight, ~perfect
aim, instant react) and is the **default for any unset/test path**, so existing
behavior and tests don't regress. **NORMAL is the default for a new player** (gentler
than today). EASY is meant to be beatable by anyone; ULTIMATE is the one step up — a
wall that also seeks pickups.

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
