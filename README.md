# Spekder

**Spekder** is a cross-BBS, terminal 3D tank-combat door game. It renders a
filled-polygon, wireframe-edged 3D arena in **truecolor half-blocks**, drawn
from each player's own first-person camera, and plays online across boards
against a shared arena server (or solo against bots with no setup at all).

It is inspired by the vector tank games of the early 1990s. Four modes ship:
**Deathmatch**, **Flag Run**, **Survival**, and **Capture the Flag**, with map
rotation, vehicles, and power-ups.

Spekder is a single self-contained binary written in Go. **No runtime, no DLLs,
no interpreter** -- drop it in and wire it to a menu.

---

## How it works (and why it's portable)

Spekder has two pieces:

| Binary | Role | Who runs it |
|---|---|---|
| `spekder` | The **door** a caller launches from your BBS | Every sysop |
| `spekder-server` | The optional **arena server** for cross-board multiplayer | One host, shared by many boards |

The wire between them carries **game state** (tank/projectile/flag positions),
never a video signal -- every node renders its own view locally. That keeps
bandwidth low enough for a BBS link and means a 32-bit machine renders the same
arena as a 64-bit one.

**An unconfigured install just works:** with no config file, the door runs
local single-player against bots. Multiplayer is opt-in (point it at an arena
server). You do not need to run the server to offer Spekder.

---

## Getting it

Download the archive for your platform from the
[Releases](https://github.com/hmderdoc/spekder/releases) page and unzip it next
to wherever you keep your door programs.

| Platform | Archive | Notes |
|---|---|---|
| 64-bit Windows | `..._windows_amd64.zip` | Windows 10/11, Server 2016+ |
| **32-bit Windows** | `..._windows_386.zip` | Windows 7/8/10/11 (32-bit) |
| 64-bit Linux | `..._linux_amd64.tar.gz` | most servers |
| 32-bit Linux | `..._linux_386.tar.gz` | older / embedded |
| ARM Linux | `..._linux_arm64.tar.gz`, `..._linux_arm.tar.gz` | Raspberry Pi etc. |
| macOS | `..._darwin_arm64.tar.gz`, `..._darwin_amd64.tar.gz` | Apple Silicon / Intel |
| FreeBSD | `..._freebsd_amd64.tar.gz` | |

Each archive contains `spekder` (the door), `spekder-server` (the arena server),
`spekder.ini.example`, and this README.

> **Oldest Windows supported:** the 32-bit build targets Windows 7 and newer.
> Truly ancient systems (XP/2000, or DOS/FOSSIL doors) are out of scope -- the
> Go toolchain can't target them. DOS BBSes can still offer Spekder by running
> it on a companion Linux/Windows box.

---

## How the door connects (this is the important part)

Spekder gets its connection one of two ways, decided automatically at launch:

1. **Socket mode (DOOR32.SYS).** If a `DOOR32.SYS` dropfile is present and says
   the caller is on a socket (comm type `2`), Spekder takes over that socket
   handle directly. This is the standard, BBS-agnostic path:
   - On **Linux/Unix** the handle is a file descriptor the BBS inherited to us.
   - On **Windows** it is a Winsock socket handle, which Spekder wraps natively.

   Virtually every modern BBS emits `DOOR32.SYS`: **Synchronet, Mystic,
   ENiGMA½, Talisman, WWIV, DoorParty/-style launchers, and others.**

2. **Standard I/O mode (stdin/stdout).** If there's no socket dropfile, Spekder
   talks over stdin/stdout. Use this when your BBS or a door launcher already
   redirects the caller's telnet stream to the door's standard I/O (common with
   FOSSIL-to-socket bridges and redirector front-ends).

You don't choose -- Spekder detects the dropfile and falls back to stdio when
there isn't one.

### Dropfile location

By default Spekder reads `DOOR32.SYS` from its **working directory** (the
convention: BBSes launch a door from the node's temp/work directory and drop the
file there). If your BBS writes it elsewhere, point Spekder at it:

```
spekder -dropfile /path/to/DOOR32.SYS
```

or set the `SPEKDER_DROPFILE` environment variable.

### Terminal size

Spekder sizes itself from, in order: the OS (for a local terminal), an ANSI
size probe over the connection (the door path), then a default of 80x25. You can
force a size by passing it positionally:

```
spekder 132 60          # cols rows
spekder -dropfile DOOR32.SYS 132 60
```

---

## Setting it up on your BBS

Spekder is a normal external program / door. Add it to your external programs
menu the same way you add any DOOR32-style door. A few examples:

### Synchronet

In `SCFG -> External Programs`, add a program with the command line:

```
?spekder
```

(or the absolute path to the binary). Recommended settings: **Native
executable**, **no I/O intercept**, and **place a DOOR32.SYS in the node
directory** (the same options you'd use for any native socket door). Spekder
reads the inherited telnet socket from `DOOR32.SYS`.

### Mystic

In the door editor, set the command line to the binary and enable a `DOOR32.SYS`
dropfile for the node, e.g.:

```
Command Line : /mystic/doors/spekder/spekder
Dropfile     : DOOR32.SYS
```

### Generic DOOR32 BBS

1. Configure the door to **write a DOOR32.SYS** into the node/work directory.
2. Launch `spekder` from that directory (or pass `-dropfile <path>`).
3. That's it -- Spekder reads the socket handle from the dropfile.

### stdio-redirector front-ends

If your setup pipes the telnet session to the door's standard input/output
(rather than handing over a socket), just launch `spekder` with no dropfile and
it will use stdin/stdout.

---

## Cross-board multiplayer (optional)

Run **one** arena server somewhere reachable; doors on any number of boards
connect out to it and share the arena. The server is a persistent lobby that
rotates and votes modes between matches.

### Run the server

Linux (systemd) -- a unit is included:

```
cp spekder-server.service /etc/systemd/system/
# edit it: set User/paths and a real -token
systemctl daemon-reload
systemctl enable --now spekder-server
```

Or just run it directly (any platform):

```
spekder-server -addr :7700 -token YOURSECRET -mode dm -bots 4 -tick 20
```

Flags: `-addr` listen address, `-token` shared secret (must match each door),
`-mode` starting mode (`dm` | `flag` | `ctf` | `survival`), `-bots` fill count,
`-tick` simulation Hz, `-maps <dir>` optional directory of author map JSON.

### Point doors at it

Copy `spekder.ini.example` to `spekder.ini` next to each door binary:

```ini
server = arena.example.com
port   = 7700
token  = YOURSECRET
```

With no `spekder.ini` (or no `server =` line) the door stays single-player. The
config is read at launch, so changes apply to the next caller with no restart.
(The legacy filename `door.ini` is also still honored.)

---

## Terminal requirements

Spekder draws with **24-bit truecolor** ANSI and CP437 half-block glyphs, plus
cursor/auto-wrap control sequences. Callers need a terminal that supports
truecolor SGR (`ESC[38;2;r;g;b m`). Modern truecolor terminals (Windows
Terminal, xterm, iTerm2, and truecolor-capable web terminals like fTelnet) work
well; strictly 16-color clients will render but the 3D shading will band badly.

---

## Controls

```
W / S ............ drive forward / reverse
A / D ............ turn hull left / right
, / .  or arrows . aim turret
SPACE ............ fire
ENTER ............ jump
C ................ recenter turret to hull-forward
TAB .............. toggle top-down view
</ > ............. vote next/prev mode (in the lobby)
Q / Ctrl-C ....... quit
```

---

## Building from source

Requires Go (see `go.mod` for the version). No CGO, no external build tools.

```
git clone https://github.com/hmderdoc/spekder
cd spekder
go test ./...
go build -o spekder ./cmd/spekder
go build -o spekder-server ./cmd/server
```

Cross-compile for any target the usual Go way, e.g. 32-bit Windows:

```
CGO_ENABLED=0 GOOS=windows GOARCH=386 go build -o spekder.exe ./cmd/spekder
```

Tagged releases are built automatically for all supported platforms by GitHub
Actions (see `.github/workflows/release.yml`).

---

## Troubleshooting

- **The door writes `spekder.log`** next to its binary on every run (connection
  type, detected terminal size, errors, FPS). Check it first.
- **Caller sees a garbled screen / it scrolls:** the terminal isn't honoring
  auto-wrap-off or isn't truecolor. Try a truecolor client.
- **Online arena never connects:** confirm `server`/`port`/`token` in
  `spekder.ini` match the server's `-token`, and that the port is reachable.
- **Wrong size:** pass `spekder <cols> <rows>` explicitly.

---

## License

See `LICENSE` if present in this repository.
