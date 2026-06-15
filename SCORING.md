# Scoring, stats, high scores, player card — design

Goal: a points system + persistent records. A **HIGH SCORES** board (outstanding
single games) and a **PLAYER CARD** (your cumulative accomplishments) - local
always, and **global via the arena server when one is configured** (even for
single-player), per the user's ask.

## The atom: a per-match result

At match end (PhaseEnded) the door builds one `MatchResult` from the player's tank
+ the match state:

```
mode, map, won, durationSec, score,
kills, deaths, shotsFired, shotsHit, pickups,
waveReached (Survival), objective (captures/holds),
vehicle, body, when (unix)
```

Everything else (high scores, player card) is derived from a stream of these.

## Score formula (per match — transparent, mode-aware)

A simple base everyone can reason about, plus per-mode bonuses; components are
stored so the breakdown is shown, not just a number:

```
score = kills*10 - deaths*4 + pickups*2
      + (won ? 50 : 0)
      + Survival:  waveReached*40
      + Flag/CTF:  captures*30
      + KotH:      holdPoints*2
      + time bonus (objective modes you win): max(0, par - durationSec) * 2
score = max(0, score)
```

Custom maps + event maps use the same base (kills/deaths/pickups/win/time) on the
map's effective mode. A future `score` **action** could let authored behaviors award
points directly (e.g. a boss kill) - noted, not in v1.

## Cumulative vs single-game

- **PLAYER CARD = cumulative** totals across every match: games, wins, losses,
  kills, deaths, shotsFired/Hit, timePlayed, perMode{games,wins}, perVehicle{games},
  bestWave, totalScore, bestScore. Derived for display: **W/L, K/D, accuracy %,
  favorite mode** (most-played), **favorite vehicle**, total time.
- **HIGH SCORES = records**: the best *single* match `score` per mode (top-N), each
  with map + date + name. The "one outstanding game."

## Storage

- **Local (always):** a per-BBS-user file `data/spekder-<handle>.stats` (JSON) - the
  cumulative StatLine + local per-mode top-N. Written after every match, online or
  off. PLAYER CARD reads this.
- **Global (when an arena is set):** the door submits each `MatchResult` to the arena
  in a one-shot connection (like map publish) via a new `MsgScore`; the server keeps
  a global per-mode top-N + per-player aggregate, persisted to disk, queryable via
  `MsgScoreQuery`. HIGH SCORES shows the global board when reachable, else local.

So single-player results count toward the global board too, exactly as asked - the
door just submits whenever an arena is configured.

**Trust:** door-submitted scores are trust-based, same model as map publish on this
semi-trusted BBS net. Online matches are server-simulated so could later be
server-recorded for integrity; single-player is inherently client-trusted. Fine for
a door; flagged.

## Shipped — Phase 1 (local)

`stats.go`: `matchResult`, `scoreMatch` (the formula above), `statLine` (cumulative +
per-mode `High` top-10), `loadStats`/`saveStats` (per-user `data/spekder-<handle>.stats`
JSON), `record`, `recordMatchLocal`, `matchResultFrom`. The sim tracks
`shotsFired`/`shotsHit` (direct hits) + `pickups` per tank, riding `TankSnap` so the
door reads its own. `playMatch` records one result on the Active->Ended edge (skipped
for editor playtest). Menu screens: **PLAYER CARD** (W/L, K/D, accuracy, time,
favorites, per-mode) and **HIGH SCORES** (local per-mode top runs). Tested in
`stats_test.go`.

## Shipped — Phase 2 (global) + richer stats

- **Stat additions:** accuracy now counts direct **and** blast/beam hits (one per
  connecting shot); **total damage dealt** (effective, no overkill) and **healing
  done** (medic support) are tracked per tank, ride `TankSnap`, and feed the score
  (`+dmg/25 +heal/15`) and the player card.
- **Global board:** the door submits every finished match (single-player included)
  to the arena one-shot via `MsgScore` when `server` is set; the arena keeps a
  per-mode top-20 board persisted to `scores.json` (`-scores` flag), queried via
  `MsgScoreQuery`. **HIGH SCORES shows the global board when reachable** (with the
  source BBS per row), else the local tables.
- **Source BBS:** `door.ini` **`bbsname`** labels your players on the global board
  (defaults to the DOOR32 bbs id). Submitted with each score.
- Trust: same semi-trusted model as map publish (token-gated; door-reported).

## Build phases

1. **Local (P1):** `MatchResult` at match end + score formula + accuracy counters on
   the tank + `StatLine` + local persistence + record-on-finish; **PLAYER CARD** and
   **local HIGH SCORES** screens (menu already routes to them).
2. **Global (P2):** `MsgScore` submit + `MsgScoreQuery`; server tables + disk
   persistence; HIGH SCORES prefers the global board.

## Open decisions (for you)

1. **Formula** - the components/weights above (esp. the time bonus + mode bonuses):
   good baseline, or adjust?
2. **Global submission** - submit *all* games (incl single-player) when an arena is
   configured (recommended / your lean), vs online-only?
3. **High-score granularity** - per-mode top-N (recommended; custom maps would
   explode a per-map table), with maybe a separate "this map" best later?
4. **Accuracy** - add `shotsFired`/`shotsHit` counters to the tank (small sim
   change) so weapon accuracy is real? (Else drop accuracy from the card for now.)
5. **Build order** - local P1 then global P2 (recommended).
