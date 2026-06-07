# Phase B — Rules / win-conditions as data

The goal of the whole project: a flexible sandbox where **game modes emerge from
data**, not hardcoded Go. Phase A gave us a data-driven *entity/trait* palette;
Phase B does the same for *modes* — the win conditions, team structure, scoring,
and spawn mechanics that today live in four `switch w.Mode` sites.

Principle (same as Phase A): **palette, not scripting.** A mode is a fixed set of
parameters and a list of win conditions chosen from a fixed enum — never a script.

## Core idea: `Mode` is an index into a `Ruleset` table

`Mode` stops being an enum-with-behavior and becomes an **index** into a table of
`Ruleset` data values:

```go
var Rulesets = []Ruleset{
    ModeDeathmatch: {...},
    ModeFlagRun:    {...},
    ModeCTF:        {...},
    ModeSurvival:   {...},
}
func (w *World) rules() Ruleset { return Rulesets[w.Mode] }
```

The wire (`MatchSnap.Mode`), HUD, and lobby already key off the mode *index*, so
they keep working with near-zero change — the same index-sync trick maps use. A
**new mode is a new table entry** (data); no new sim code.

## The Ruleset shape

```go
type Ruleset struct {
    Name      string     // "DEATHMATCH" - HUD / lobby label
    Teams     int        // 0 = free-for-all, 2 = two teams
    TimeLimit float64    // match seconds; 0 = endless
    Lives     int        // per-human lives; 0 = infinite respawn
    Bots      BotSpawn   // BotFill (top up a pool) | BotWaves (survival)
    Objective ObjKind    // ObjNone | ObjNeutralFlags | ObjTeamFlags
    Win       []WinCond  // early-end triggers; first satisfied ends the match
    CoOp      bool        // no per-tank winner (result = progress/wave reached)
}
type WinCond struct { Kind WinKind; Count int }
// WinKind: WinFrags | WinCaptures | WinCollectAll | WinElimination
// BotSpawn: BotFill | BotWaves
// ObjKind:  ObjNone | ObjNeutralFlags | ObjTeamFlags
```

Timeout is implicit: when `TimeLimit>0` and the clock hits 0 the match ends and
the winner is resolved by scoring (most frags / most captures / the collector).

## The four current modes as data (behavior-preserving target)

| Ruleset    | Teams | TimeLimit | Lives | Bots  | Objective    | Win (early)        | CoOp |
|------------|-------|-----------|-------|-------|--------------|--------------------|------|
| DEATHMATCH | 0     | 180       | 0     | Fill  | None         | Frags ≥ 20         | –    |
| FLAG RUN   | 0     | 180       | 0     | Fill  | NeutralFlags | CollectAll         | –    |
| CTF        | 2     | 180       | 0     | Fill  | TeamFlags    | Captures ≥ 3       | –    |
| SURVIVAL   | 0     | 0         | 3     | Waves | None         | Elimination (humans out of lives) | ✓ |

Re-expressing the four modes this way must reproduce today's behavior exactly —
that is the Stage 1 correctness bar (existing CTF/survival/pickup tests stay green).

## Objectives as entity traits — with a procedural fallback

Per the locked decision, objectives join the trait palette so the editor can place
them. But every existing map has zero objective entities and must keep working:

- **Authoring**: a `flag` trait (later `zone`/capture) marks an objective's spot +
  team. Editor places them; documented in `SCHEMA.md`.
- **Runtime**: at match start the ruleset instantiates objectives **from the map's
  flag entities if present, else from today's procedural placement** (scatter N
  neutral flags / two team flags at bases). The dynamic flag *behavior*
  (carry / return / capture) is unchanged; only its *source* becomes data.

## Stages (each ends green, the four modes intact)

1. **Ruleset table + route the switches.** Behavior-preserving refactor: define the
   table, replace every `switch w.Mode` / `w.Mode == ModeX` (startMatch, simulate,
   checkEnd, computeWinner, bot AI targeting, projectile friendly-fire) with reads
   off `w.rules()`. No gameplay change.
2. **Objectives from data.** Drive flag/CTF setup off `Objective` + map flag-entities
   (procedural fallback). Add the `flag` trait to schema + validator.
3. **Prove emergence.** Add a brand-new mode as a *pure table entry* (e.g.
   ELIMINATION: FFA, lives 3, win = last-standing) with no new sim code. Generalize
   the lobby vote tally from a fixed `[4]` to N rulesets.
4. **(Stretch)** `zone`/capture-point trait + a King-of-the-Hill ruleset.

## Notes / risks

- Stage 1 touches working mode code; correctness is guarded by the existing mode
  tests plus new ruleset tests.
- `MatchSnap.Votes [4]int` generalizes to N in Stage 3 (small wire-adjacent change).
- All new knobs are fixed enums parameterized by data — no scripting.
