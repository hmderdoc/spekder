# Event-driven behavior system — design

Goal: a **no-code way for map authors to script complex games/levels** in the
editor — boss fights with health-based phases and adds, doors that open when a
zone flips, escort/payload, ambush triggers, custom win logic — without touching
Go. The whole range from "boss changes attack at 50% HP" to "Overwatch payload."

## Why this fits what we already have

The sim is already a pile of hard-coded mini event-handlers (game.go):

- Teleporter = *occupy → warp → cooldown* (`stepTeleport`)
- Zone = *occupy → capture progress → flip → score* (`stepZones`)
- Hazard = *occupy → damage* (`stepHazard`)
- Survival = *wave cleared → spawn next* (`spawnWave`)
- Win = *condition polled → end match* (`winCondMet`/`checkEnd`)

These are data-driven (trait structs) but fixed in wiring. The behavior system
generalizes the wiring: **author-defined signals, conditions, and actions** on the
same entities, evaluated in the same server-authoritative tick.

## Model: Signals -> Conditions -> Actions (+ a blackboard)

A small reactive rule engine (the Source-engine I/O model crossed with GameMaker's
"when X, if Y, do Z"). Three nouns + state:

### 1. Signals (the event bus)

A signal is a named message put on a per-tick bus: `{name, source, subject}` where
`source` = the emitting entity (or -1 = world) and `subject` = a tank involved
(killer/victim/enterer), when relevant.

**Built-in signals** the engine emits at the hook points that already exist:

| signal        | emitted where                          | subject     |
|---------------|----------------------------------------|-------------|
| `start`       | match begins / first active tick       | -           |
| `killed`      | `hurt()` takes a tank to 0             | victim (+killer) |
| `destroyed`   | entity HP -> 0 in `stepEntities`       | -           |
| `captured`    | zone flips in `stepZones`             | team        |
| `entered`/`exited` | tank crosses a trigger/zone footprint | the tank |
| `picked`      | pickup taken in `stepPickups`          | the tank    |
| `wave_cleared`| Survival wave empties                  | -           |
| `hp_below`/`hp_above` | an entity/tank HP crosses a watched threshold | the actor |
| `timer`       | a named author timer elapses           | -           |

**Custom signals**: any action can `emit "phase2"`, and any behavior can trigger
`on: phase2`. This is the composition glue — it's how you build state machines and
sequences without new engine code.

### 2. Behaviors (rules) — entities and the world are emitters AND subscribers

Every entity (and a world-level "director", below) carries a list of behaviors:

```
Behavior {
  on:    "<signal>"        // built-in or custom name
  when:  [Condition...]    // optional; ALL must pass (AND)
  do:    [Action...]       // ordered
  once:  bool              // fire at most once per match
}
```

"Emitter and subscriber" falls out naturally: an entity *subscribes* via its `on:`
triggers and *emits* via `emit` actions (and the engine emits the built-ins for it,
e.g. a boss entity gets `hp_below` when its HP crosses a threshold it registered).

### 3. Conditions (gates)

Evaluated when the trigger fires, against world state + the blackboard:

- `var <name> <op> <n>` — blackboard compare (`op` = `== != < > <= >=`)
- `hp <op> <pct>` — of the owning entity (or a tagged one)
- `count <selector> <op> <n>` — alive bots / players / entities-with-tag
- `owner == <team>` — zone ownership
- `chance <pct>` — randomness
- `timer <name> elapsed`

This is what lets one signal branch: `on killed, when count(bots)==0, do nextwave`.

### 4. Actions

- `emit <signal> [after <sec>]` — fire a (custom) signal, optionally delayed
- `spawn <what> at <where>` — a creature/tank/entity at a spawn point or tag pos
- `setvar <name> = <n>` / `addvar <name> += <n>` — blackboard writes
- `setstat <target> <stat>=<v>` — change weapon/speed/hp/firedelay/**body** of a
  tank or entity (this is a boss phase: swap weapon, speed up, change silhouette)
- `enable`/`disable <tag>` — toggle a hazard/turret/wall (doors, traps)
- `move <tag> along <path> [speed]` — escort/payload (Phase 3)
- `message "<text>" [to <who>]` — on-screen announce ("PHASE 2")
- `settimer <name> <sec>` — starts a timer that emits `timer` on elapse (sequencing)
- `damage`/`heal <target> <amt>`, `teleport <target> to <where>`
- `win [team]` / `lose` / `nextwave`

**Targets/selectors** (how actions and conditions point at things): entities get an
optional author **`tag`** ("door_A", "boss"); actions reference `#tag`. Tanks via
roles: `self`, `killer`, `victim`, `nearest`, `all`, `players`, `bots`, `team:0`.

### 5. Blackboard (variables)

A per-match `map[string]int` seeded by the map's `vars`. Conditions read it, actions
write it. This is what turns one-shot triggers into real logic — phase counters,
payload progress, "3 generators destroyed", etc.

### 6. The "director" (world-level logic)

Match-level behaviors that don't belong to a physical object (wave director, payload
controller, intro sequence) live in a map-level `logic: [Behavior...]` list with
`source = -1`. No new entity kind needed.

## Where it runs

Server-authoritative, in the existing tick (and the offline door runs the identical
code). The behavior graph is **part of the Map**, so it travels in `MsgMap` exactly
like entities do today. Effects replicate through the snapshots we already send:
spawns show up as tanks/entities, `setstat`/`enable` show up in `TankSnap`/
`EntitySnap`, zone flips already replicate. The only genuinely new wire is a small
**announce/toast channel** for `message` (a new `MsgEvent`), and possibly a "play
this sound" later.

## Schema (JSON, editor-authorable) — bump to v4

Additions, all optional (tolerant decode; absent = today's behavior):

```json
{
  "version": 4,
  "vars": { "phase": 1, "downed": 0 },
  "logic": [
    { "on": "start", "do": [{ "act": "message", "text": "Survive the warden." }] }
  ],
  "entities": [
    {
      "kind": "turret", "tag": "boss", "pos": [0,0,0], "...": "...",
      "watch": [50, 25],                        // HP% thresholds -> hp_below signals
      "behaviors": [
        { "on": "hp_below", "when": [{ "v":"phase","op":"==","n":1 }],
          "do": [ { "act":"setvar","v":"phase","n":2 },
                  { "act":"setstat","target":"self","stat":"weapon","n":7 },
                  { "act":"message","text":"The warden draws its laser!" } ],
          "once": true },
        { "on": "hp_below", "when": [{ "v":"phase","op":"==","n":2 }],
          "do": [ { "act":"spawn","what":"spider","at":"#nest","count":3 },
                  { "act":"message","text":"It calls for help!" } ],
          "once": true }
      ]
    }
  ]
}
```

Schema additions: `Map.Vars`, `Map.Logic`, and per-entity `Tag`, `Watch[]`,
`Behaviors[]`. Behavior/Condition/Action are small typed structs with the same
`jXxx` marshaling pattern entities already use.

## Editor authoring (the no-code surface)

Terminal, so no node graph — a **form-based behavior builder** instead, reusing the
existing `editField`/`fieldsFor` machinery:

- Select an entity (or a new **LOGIC** pseudo-item for the director) -> a BEHAVIORS
  sub-panel listing its rules.
- Add a rule: pick **On** from a menu (built-in signals + "custom name..."), add
  **When** conditions (pick type/op/value), add **Do** actions (pick act; params via
  field editors; targets picked from a live list of existing tags).
- A **`tag`** field on entities (name them so actions can reference them).
- Paths placed like spawns (Phase 3, for `move`).

This is the biggest chunk of work and the part that makes it "no-code."

## Build phases (each shippable + provable)

1. **Engine core, JSON-authored (no editor UI yet).** Signal bus + Behavior/
   Condition/Action types + blackboard; emit the built-in signals at existing hooks;
   implement core actions (`emit`, `spawn`, `setvar`, `setstat`, `enable/disable`,
   `message`, `settimer`, `win/lose/nextwave`); schema v4 load/validate; the
   `MsgEvent` toast channel. **Prove it with a hand-written boss-fight map** (phases
   via `hp_below` + `setstat` + `spawn`). Runs on server + offline door.
2. **Editor authoring UI.** Tags + the behavior builder forms, so a non-coder can
   make the boss fight entirely in-door.
3. **Advanced.** Paths + `move` (payload/escort), trigger volumes (`entered`/
   `exited`), richer selectors/conditions. Build the payload-escort example.

## Shipped — Phase 1 (engine core, JSON-authored)

Implemented in `internal/game/behavior.go` + hooks in `game.go`:

- **Types**: `Behavior{On,When,Do,Once}`, `Condition{Kind,Sel,Var,Op,N}`,
  `Action{Act,...}`, `Signal{Name,Source,Subject}`. `Map.Vars`/`Map.Logic`,
  `Entity.Tag`/`Watch`/`Behaviors` (schema **v4**, tolerant JSON).
- **Blackboard** (`World.vars`), **bus** + delayed emits, **dispatch** (self-scoped
  vs broadcast), conditions (`var`/`hp`/`count`/`chance`), actions (`emit[after]`,
  `setvar`/`addvar`, `message`, `setstat`, `spawn`, `enable`/`disable`, `win`/`lose`/
  `nextwave`). Tags via `#tag`; spawn archetypes by creature name.
- **Built-in signals** emitted at existing hooks: `start`, `killed`, `destroyed`,
  `captured`, `picked`, `wave_cleared`, and `hp_below` via per-entity `Watch[]`.
- **Toasts**: `message` rides `MatchSnap.Events` (in MsgState) -> a top banner.
- **Proof**: `maps/11-warden.json` (a boss turret that enrages + swaps to the laser
  at 66% HP, summons a swarm at 33%, and ends the match when destroyed). Covered by
  tests in `behavior_test.go`.

Runs identically on the server and the offline door. Logic lives in the map JSON;
clients only receive effects, so **no behavior data is added to the client wire**
(only the small `Events` toast list).

### Authoring note: sequential phases

Conditions read **live** vars, and all of an entity's rules matching a signal fire
in order within one dispatch. So don't gate phase N+1 on a var that phase N's rule
just wrote on the *same* trigger - it cascades. Gate sequential phases on the
entity's **HP + `once`** (HP doesn't change mid-dispatch) or on **distinct custom
signals** (which dispatch on the next pass). The Warden uses HP+once.

## Shipped — Phase 2 (in-editor authoring)

`editor_behavior.go`: modal, form-based behavior authoring (no JSON). Reached two
ways in the editor:

- Select an entity, press **ENTER** -> its **BEHAVIORS** editor (also edits the
  entity's **TAG** and **WATCH%** thresholds).
- File menu -> **LOGIC** -> the map-level **director** rules.

The flow is pick-driven: a behavior list (add/edit/delete) -> per-behavior ON
(signal vocab + custom), ONCE, WHEN conditions, DO actions -> per-field pickers
(op/stat/archetype/count-sel from fixed lists) or text entry (var/tag/message/custom
signal). Helpers `pickFromList` / `textEntry` handle the terminal quirk that letter
keys push both a rune and a nav event (text entry consumes runes, ignores nav).

Publish now sends the map as **JSON** (`EncodePublish`/`DecodePublish`), so an
authored map's vars/logic/tags/watch/behaviors reach the arena repo intact (the lean
wire `EncodeMap` is still used for client render/collision). Build a boss in the
editor, PLAYTEST it, PUBLISH it.

## Shipped — Phase 3 (payload + triggers)

- **Trigger volumes**: a `TriggerTrait` entity (`TRIGGER` in the catalog) emits
  `entered`/`exited` (self-scoped, subject = the tank) as tanks cross its footprint.
  Renders as a faint pad; select it + ENTER to attach behaviors.
- **Paths + move**: `Map.Paths` (named waypoint lists; place `PATH POINT` to build
  the default `main` path). Actions `move <target> along <path> [speed]` and `stop`;
  a moved entity's position rides `EntitySnap.Pos`, and emits `arrived` at the end.
- **Periodic `tick`** signal (every 0.5s) + **`near`** condition (count a selector
  within radius R of the owning entity) - the per-tick gating payload needs.
- **Editor**: TRIGGER + PATH POINT catalog items; behavior vocab gains the new
  signals (`tick`/`entered`/`exited`/`arrived`), the `near` condition, and `move`/
  `stop` actions.
- **Scripted maps are pick-only**: `Map.Scripted()` (has logic/paths/behaviors)
  keeps WARDEN/ESCORT out of random rotation + auto-vote (reach them via the picker).
- **Proof**: `maps/12-escort.json` - a cart that advances along a path while a
  player is near and no enemies are (`tick`+`near`+`move`/`stop`), and `win`s on
  `arrived`. Tests in `behavior_test.go`.

## Shipped — Phase 3 remainder

- **Mobile bosses**: behaviors + Watch on **tanks**, not just entities. Signal
  sources share one int space - entity = index, tank = index + `tankSrcBase`, -1 =
  broadcast - so `self` stays an int and dispatch tells them apart. Refs
  (`resolveRef`/`refPos`/`refHPpct`) resolve `self`/`#tag`/`victim` to either.
  `setstat` on a tank tunes weapon2/HP/speed/firedelay/maxhp (via a lazy custom
  stat block). **Actor templates** (`Map.Actors`: name+vehicle+body+maxHp+watch+
  behaviors) are spawned with `spawn @<name>`. Demo: `maps/13-stalker.json` - a
  roaming HEAVY-chassis tripod boss that rages at 50% (faster, laser, summons
  spiders) and `win`s the map when slain.
- **Per-map bot-fill**: `MapRules.Bots` (RULES panel + JSON; pointer-encoded so old
  maps default). WARDEN = 0 fill, ESCORT = 4 defenders.
- **`killed` is now a broadcast** (was mis-scoped to the killer's index); subject =
  victim.
- **Editor**: path points are selectable + deletable (`selPath`); RULES has a `bots`
  row.
- **`ValidateMap`** warns on unknown signal/condition/action, a `move` to a missing
  path, and `#tag`/path references that don't resolve.

## Actor templates in the editor (shipped)

File menu -> **ACTORS** -> a list of mobile-boss templates (add/edit/delete). Each
actor edits NAME (`spawn @name`), CHASSIS (stats/frame), BODY (silhouette incl.
creatures), MAX HP, WATCH% thresholds, and BEHAVIORS (the same behavior-builder
entities use). So the whole STALKER fight - roaming boss, phases, summons, win - is
now authorable in-door with zero JSON. (`runActorEditor` in editor_behavior.go.)

## Vocabulary round-out (shipped)

- **`killer` selector**: `killed` now carries the killer too (`Signal.Other`);
  selectors are `self` / `#tag` / `victim` (subject) / `killer`. Custom `emit`s
  forward both, so chains keep context. Enables "reward the killer", "the boss
  taunts on a kill", etc.
- **`side` condition**: is a referenced tank a `player` or a `bot` -
  e.g. `on killed, when side victim is player -> message`.
- **`damage` / `heal` actions**: hurt or restore a tank ref by an amount (traps,
  scripted hits, healing zones). `damage` credits the kill to the owning actor if
  it's a tank. Both in the editor's action vocab + ValidateMap.

## Still open

- condition-on-`subject` beyond side/position/hp (e.g. ref equality "killer ==
  self"); an `award`/`score` action feeding the match score directly.

## Open decisions (for you)

1. **Model shape** — Signals->Conditions->Actions + blackboard (my recommendation;
   it's terminal-authorable and covers all your examples) vs a visual node graph
   (too heavy for a BBS terminal) vs a state-machine-first model. Agree on the rule
   model?
2. **Build order** — engine-core-first proven by a hand-authored JSON boss map, then
   the editor UI (recommended), vs editor-first.
3. **Phase-1 vertical slice** — the **boss fight** (HP-phase -> swap weapon/body +
   spawn adds + announce) as the first concrete target? (Payload is Phase 3 — it
   needs paths + a contested moving zone, which is more machinery.)
4. **Evaluation model** — event-driven on the bus, with the engine emitting a small
   set of "watcher" signals (HP thresholds via per-entity `watch[]`, counts) so
   authors never write per-tick polling. OK?
5. **Toast/announce wire** — add a small `MsgEvent` so `message` can reach players
   (and later sounds)? It's the one new piece of protocol.
