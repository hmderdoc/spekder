# Spectre - Design Document

A cross-BBS, terminal-native, truecolor 3D tank-combat door, inspired by
Velocity's *Spectre* (1991) and its sequels (*Spectre VR*, *Spectre Supreme*):
flat-shaded vector tanks in a walled arena, capture-the-flag and deathmatch,
networked play - here, players from *different* BBSes sharing one arena.

This doc supersedes the seed in [idea.md](idea.md). The proof-of-concept that
backs the key claims is [main.go](main.go) (see "What's already proven").

---

## 1. The one principle everything hangs off

**The wire carries game STATE, never pixels. Every node renders its own 3D
view locally.**

This is the deliberate opposite of a video door. asciicam/telnetvision proved a
truecolor half-block framebuffer with delta redraws sustains ~10-15fps over a
real BBS link - we reuse that *rendering technique* and its *door plumbing*, and
discard its data model entirely. The reasons:

- A single shared pixel stream can only show one camera. A tank game needs N
  cameras, one per player. Local rendering is the only way each player sees the
  world from their own tank.
- State is tiny (positions, orientations, events). Pixels are not. Local render
  moves the only heavy work (raster -> half-block frame) onto each node's
  process, where it's already proven cheap.

Everything below follows from this.

---

## 2. What's already proven (the spike)

[main.go](main.go) is a runnable, drivable, flat-shaded + wireframe arena that
renders from the player's camera. Measured on the BBS box:

| scenario | bytes/frame | sustained fps |
|---|---|---|
| near-static view | ~1.8 KB | 30 (CPU-capped, easily) |
| worst case: whole view changing every frame | ~13.6 KB | 30 |

Conclusions:
- Rasterizing flat-shaded + edge geometry is **not** the bottleneck; it pegs the
  frame cap with headroom.
- Worst-case full repaint (~13.6 KB) sits inside asciicam's proven envelope. At
  a 12-15fps game cap that's ~165-205 KB/s worst case; typical partial-change
  frames are a fraction of it.
- Blocking writes give correct backpressure: a slow link throttles effective fps
  gracefully (as asciicam's logs show: target 15, effective ~10).

Reusable, already in the spike: the z-buffered triangle rasterizer, Bresenham
silhouette edges, flat lighting + distance fog, the truecolor half-block delta
encoder (cp437 `0xDF`), `DOOR32.SYS` socket inheritance, raw single-key input
with arrow + telnet-IAC handling.

---

## 3. Topology: cross-BBS universal

Modeled on the avatar_chat_universal / asciicam fan-out shape: a central
**arena server** that any number of **door clients** (one per caller, on each
participating BBS) connect out to.

```
   BBS A                         BBS B
  +--------+   node1 door --\   /-- node1 door   +--------+
  | SyncBBS|   node2 door ---\ /--- node3 door   | Mystic |
  +--------+                  X                  +--------+
                              |
                     +------------------+
                     |   arena server   |  authoritative game state
                     | - match/lobby    |  ~20 Hz simulation tick
                     | - physics/hits   |  per-board shared-secret auth
                     | - anti-cheat     |  binary length-prefixed wire
                     +------------------+
```

- **Door client** runs on each BBS box, launched per node. It renders locally,
  sends the caller's inputs, receives authoritative state, predicts/interpolates.
- **Arena server** is the single source of truth: it owns positions, runs
  physics and hit detection, resolves CTF/scoring, and broadcasts state. It runs
  wherever we host it (one well-connected box), like the CheeseNet QWK hub or the
  asciicam fan-out service.

### Identity and trust
- Each participating board holds a **shared secret token** (per-board, in
  `door.ini`), exactly like the asciicam `-token` model and the CheeseNet
  per-node secret. The board authenticates to the server once per connection.
- The door reads the caller's identity from `DOOR32.SYS` (handle, BBSID, node)
  and asserts it to the server. The board vouches for its own users; display
  name is `handle@BBSID`. The server never trusts client-asserted *game* state,
  only the board-vouched *identity*.
- Untrusted clients are assumed: anyone can run a patched door. The server is
  authoritative for all gameplay (movement validation, fire rate, hit reg), so a
  hacked client can cheat only within server-enforced bounds. See section 9.

---

## 4. Wire protocol

Framing reuses telnetvision's shape: `uint32 length (big-endian)` + payload,
`payload[0]` = message type. Binary, not JSON - this is hot-path real-time.

### Client -> server
| type | payload |
|---|---|
| `HELLO` 0x01 | token, bbsid, handle, node, term cols/rows |
| `INPUT` 0x10 | seq:u32, buttons:u8 (fwd/back/left/right/turretL/turretR/fire), dt:u16 |
| `CHAT`  0x11 | utf8 text (server relays; ASCII-safe, see section 7) |
| `PING`  0x1F | nonce:u32 |

### Server -> client
| type | payload |
|---|---|
| `WELCOME` 0x81 | yourID:u16, arenaID, mapSeed:u32, tickRate:u8 |
| `STATE`   0x90 | tick:u32, count:u8, then per entity: id:u16, x/z:i16 (fixed-pt), yaw:u8, turretYaw:u8, hp:u8, flags:u8 |
| `EVENT`   0x91 | kind (hit/kill/flagtake/flagcap/spawn/leave), actor, target, data |
| `PONG`    0x9F | nonce:u32, serverTick:u32 |

Notes:
- **Tick model:** server simulates at ~20 Hz, broadcasts `STATE` each tick.
  Client renders at its own (capped) fps, **interpolating** other tanks between
  the last two `STATE`s and **predicting** its own tank from local `INPUT`
  (reconciled when authoritative state arrives - standard rollback-lite).
- **STATE size:** ~9 bytes/entity. A 16-tank arena is ~150 bytes/tick * 20 Hz =
  ~3 KB/s down per client. Trivial. Optional per-field delta later if needed.
- The arena geometry is static and procedurally generated from `mapSeed`, so it
  never travels - every client builds the identical world locally.

---

## 5. Renderer

Filled flat-shaded polygons + wireframe silhouettes (the chosen fidelity), as in
the spike. Refinements for the real door:

- **Adaptive resolution/fps:** read term size from `DOOR32.SYS` term hint or
  ioctl; pick a cell grid; cap fps (default ~15, sysop-tunable in `door.ini`).
  If the link can't keep up (write backpressure), drop fps before dropping
  detail.
- **Tank model:** hull + rotating turret + barrel (turret yaw is its own wire
  field so you can aim independent of heading). Distinct per-player hull color
  keyed off playerID; name tag billboarded above each tank.
- **HUD:** radar/compass, hp, ammo, score, flag status, kill feed. Bottom rows,
  rewritten cheaply each frame.
- **CP437 / no-Unicode discipline:** everything drawn into cells is CP437 bytes
  (`0xDF` half-block) and HUD/name text is ASCII only - no smart quotes, em
  dashes, or U+2580, which become garbage on a SyncTERM caller. Never emit
  `0x1B` inside cell content. (Project conventions; learned the hard way.)
- **16-color fallback:** keep the asciicam CGA path available for callers that
  can't do truecolor, behind a `color=16` config axis.

---

## 6. Game design (v1 scope)

- **Modes:** Deathmatch (frags to N) and Capture-the-Flag (grab enemy flag,
  return to base). Spectre's signature is CTF.
- **Arena:** procedurally generated from `mapSeed` - flat grid floor, perimeter
  walls, scattered block cover, two flag bases for CTF. Same seed = same map for
  all clients.
- **Combat:** forward-firing main gun with cooldown; projectiles are
  server-simulated; flat damage; respawn after a few seconds at your base.
- **Match lifecycle:** lobby -> warmup -> match (timer or score cap) ->
  scoreboard -> rotate map. Players join/leave mid-match (BBS callers come and
  go); empty arena idles.
- **Persistence (later):** cross-BBS leaderboard, per-player stats, ELO. The
  ledger/stat-store can mirror the append-only pattern used elsewhere in the
  system.

---

## 7. Input

Raw single-key, already in the spike: WASD drive/strafe, arrows or H/L turn,
plus turret-aim and fire keys to add. Telnet IAC stripping is handled. Auto-
repeat is treated as "held" via per-action timestamps so motion is smooth
without key-up events (terminals don't send them). Chat is a line-entry mode
toggled by a key; chat text is relayed by the server and must be ASCII-safe
before it lands on any CP437 terminal.

---

## 8. Door integration

- **Launch:** Synchronet external program, command line points at
  `/sbbs/xtrn/spectre/door` (the stable path the spike already builds to, so the
  sysop wires it once). Start-up dir holds `door.ini` (server host/port, token,
  fps, color depth, encoding) - read per-launch so edits take effect for the
  next caller with no BBS restart, exactly like the asciicam door.
- **Connection:** when `DOOR32.SYS` line 1 == 2 (telnet), the BBS-inherited
  socket is used directly; otherwise stdio (local testing). Already implemented.
- **Clean exit:** restore tty, re-enable autowrap, show cursor, clear - on
  Q/Ctrl-C, signal, or caller disconnect (input EOF). Already implemented.

---

## 9. Security

- Server-authoritative for **all** gameplay: it validates movement deltas
  against max speed, enforces fire-rate cooldowns server-side, does hit
  detection server-side. A patched client cannot move faster, fire faster, or
  fake hits.
- Per-board shared-secret auth gates who can connect; a leaked token is revoked
  by rotating it in that board's `door.ini`.
- Identity is board-vouched (the BBS asserts its caller); the server does not
  trust client-claimed identity beyond what the authenticated board sends.
- Rate-limit and size-cap every inbound message; never trust a length field
  without a max (the telnetvision `maxMsg` guard pattern).
- Chat is relay-only and sanitized to ASCII before redistribution.

---

## 10. Cross-platform build & distribution

Per idea.md: Go, with builds for linux / windows / mac, 32-bit where it makes
sense for older BBS hosts, and a README with per-OS install steps.

- The **door client** must build for all targets (BBS hosts run varied OSes).
  Non-blocking I/O and the DOOR32 socket handling are the platform-specific
  bits - split per-build-tag (`io_unix.go` / `io_windows.go`) as telnetvision
  already does.
- The **arena server** only needs to build for wherever we host it (one box).
- CI: cross-compile matrix + release artifacts (the telnetvision repo's
  `release.yml` is a working template to adapt).
- Build artifacts are git-ignored with anchored patterns (`/door`, not `door`),
  since the binary and source dir share a name.

---

## 11. Roadmap

1. **Spike (DONE)** - local flat-shaded drivable arena; bandwidth/fps de-risked.
2. **Single-player feel (DONE)** - tank hull + independent turret, fire +
   projectiles, collision, respawn/spawn-protection, bots, damage flash.
3. **Arena server + protocol (DONE)** - module split into `internal/game`
   (shared sim), `internal/proto` (HELLO/INPUT/STATE), `cmd/server`
   (authoritative ~20 Hz, token auth, broadcast). Door is a thin client with
   offline single-player fallback. Verified: two doors in one arena.
4. **Netcode quality (DONE)** - remote tanks interpolated from a snapshot buffer
   (rendered ~110 ms in the past); own tank client-predicted from local input via
   `game.Predict` and reconciled toward authoritative state (snap on
   respawn/teleport). No input-seq replay yet (mild rubber-band possible at high
   latency) - a later refinement if needed.
5. **Game systems (IN PROGRESS)** - DONE: mode menu (TDF title + list) with
   ONLINE ARENA as a menu item, radar HUD, jump, server-authoritative match
   lifecycle over the wire (countdown -> play -> scoreboard), Deathmatch, and
   Flag Run (flag entities, collect-vs-clock, on the wire), and the lobby (server
   = persistent lobby; between matches players vote the next mode, else it
   rotates), data-driven maps (JSON schema; collision; jump-over; elevation/ramps
   you drive up and stand on; per-map size; rotation; over-the-wire so author
   maps in the server's -maps dir work cross-BBS), top-down (TAB) view, vehicles,
   and enemy hit-flash. TODO: CTF, Survival; power-up drops (schema pickups[]
   reserved).
6. **Cross-BBS hardening** - multi-board auth, identity, anti-cheat bounds,
   rate limits; chat relay.
7. **Distribution** - cross-compile matrix, 32-bit builds, README, release CI,
   16-color fallback polish.
8. **Persistence** - cross-BBS leaderboard and stats.

---

## 12. Open questions (for later)

- Hosting box for the arena server, and its public host/port.
- Match size cap (how many tanks per arena before it gets unreadable at terminal
  resolution?).
- Single global arena vs. multiple named arenas / lobbies.
- How much Spectre-authentic geometry (ramps, pyramids, the classic "Spectre"
  obelisks) vs. a cleaner modern arena.
- Whether to share any identity/presence plumbing with the existing chat/bridge
  infrastructure or keep this fully standalone.
