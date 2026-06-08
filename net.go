package main

import (
	"bufio"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gm "spekder/internal/game"
	"spekder/internal/proto"
)

const (
	offlineBots = 3
	interpDelay = 110 * time.Millisecond // render remote tanks this far in the past
	snapKeep    = 1 * time.Second        // snapshot history retained for interpolation
	reconcileK  = 0.18                   // per-tick blend of predicted -> authoritative
	snapDist    = 4.0                    // error above this snaps (respawn/teleport)
)

// arenaConfigured reports whether the sysop set an arena server in spekder.ini.
// spekder.ini only says WHERE the arena is; the caller chooses to join via the menu.
func arenaConfigured() bool { return loadINI(defaultINIPath())["server"] != "" }

// connectArena dials the configured arena server and joins as a client with the
// chosen vehicle.
func connectArena(vehicle int) (*netSession, error) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return nil, &netErr{"no arena server configured"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	bbsid, handle := door32Identity("DOOR32.SYS")
	ns, err := dialServer(host, port, ini["token"], bbsid, handle, vehicle)
	if err != nil {
		return nil, err
	}
	logf("connected to arena %s:%s as tank %d", host, port, ns.me)
	return ns, nil
}

type stamped struct {
	t       time.Time
	tanks   []gm.TankSnap
	shots   []gm.V3
	flags   []gm.FlagSnap
	pickups []gm.PickupSnap
	ents    []gm.EntitySnap
	zones   []gm.ZoneSnap
}

// netSession mirrors the server's broadcast and forwards our input. Remote tanks
// are interpolated from a short snapshot history (smooth at 30fps despite the
// 20Hz feed); our own tank is client-predicted from local input and reconciled
// toward the authoritative state so steering feels instant.
type netSession struct {
	conn net.Conn
	me   int

	mu        sync.Mutex
	buf       []stamped
	lastSelf  gm.TankSnap
	lastMatch gm.MatchSnap
	curMap    gm.Map // active map, sent by the server (MsgMap)
	haveSelf  bool
	dead      bool

	// prediction state — main goroutine only
	predPos   gm.V3
	predHull  float64
	predTur   float64
	predPitch float64
	predVy    float64
	predInit  bool
}

func dialServer(host, port, token, bbsid, handle string, vehicle int) (*netSession, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return nil, err
	}
	if err := proto.WriteMsg(conn, proto.EncodeHello(token, bbsid, handle, vehicle)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := proto.ReadMsg(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	me, ok := proto.DecodeWelcome(msg)
	if !ok {
		conn.Close()
		return nil, &netErr{"server did not send WELCOME"}
	}
	conn.SetReadDeadline(time.Time{})
	ns := &netSession{conn: conn, me: me}
	go ns.readLoop()
	return ns, nil
}

type netErr struct{ s string }

func (e *netErr) Error() string { return e.s }

func (s *netSession) readLoop() {
	for {
		msg, err := proto.ReadMsg(s.conn)
		if err != nil {
			s.mu.Lock()
			s.dead = true
			s.mu.Unlock()
			logf("arena connection lost: %v", err)
			return
		}
		if len(msg) > 0 && msg[0] == proto.MsgMap {
			if m, ok := proto.DecodeMap(msg); ok {
				s.mu.Lock()
				s.curMap = m
				s.mu.Unlock()
				logf("received map %q (%d obstacles, %d ramps)", m.Name, len(m.Obstacles), len(m.Ramps))
			}
			continue
		}
		_, match, tanks, shots, flags, pickups, ents, zones, ok := proto.DecodeState(msg)
		if !ok {
			continue
		}
		now := time.Now()
		s.mu.Lock()
		s.lastMatch = match
		s.buf = append(s.buf, stamped{t: now, tanks: tanks, shots: shots, flags: flags, pickups: pickups, ents: ents, zones: zones})
		// trim history older than snapKeep, but always keep the last two.
		cut := now.Add(-snapKeep)
		drop := 0
		for drop < len(s.buf)-2 && s.buf[drop].t.Before(cut) {
			drop++
		}
		if drop > 0 {
			s.buf = s.buf[drop:]
		}
		for _, t := range tanks {
			if t.ID == s.me {
				s.lastSelf, s.haveSelf = t, true
				break
			}
		}
		s.mu.Unlock()
	}
}

func (s *netSession) step(dt float64, in gm.Input) viewState {
	s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = proto.WriteMsg(s.conn, proto.EncodeInput(in)) // best-effort; readLoop notices a dead link

	s.mu.Lock()
	haveSelf := s.haveSelf
	self := s.lastSelf
	match := s.lastMatch
	cmap := s.curMap
	buf := append([]stamped(nil), s.buf...) // shallow copy of headers (entries immutable)
	s.mu.Unlock()

	var latestEnts []gm.EntitySnap
	if len(buf) > 0 {
		latestEnts = buf[len(buf)-1].ents // entity HP/dead/facing: latest authoritative
	}

	if !haveSelf {
		return viewState{me: s.me, ready: false}
	}

	// --- own-tank prediction + reconciliation (camera) ---
	if !s.predInit {
		s.predPos, s.predHull, s.predTur, s.predPitch = self.Pos, self.HullYaw, self.TurretYaw, self.TurretPitch
		s.predInit = true
	}
	if self.Dead || match.Phase != gm.PhaseActive {
		// no movement while dead or between matches (countdown/scoreboard); ride
		// the authoritative spot so prediction doesn't fight a frozen server.
		s.predPos, s.predHull, s.predTur, s.predPitch, s.predVy = self.Pos, self.HullYaw, self.TurretYaw, self.TurretPitch, 0
	} else {
		solids := gm.SolidBoxes(cmap, latestEnts) // block on alive solid entities, matching the server
		s.predPos, s.predHull, s.predTur, s.predPitch, s.predVy = gm.Predict(s.predPos, s.predHull, s.predTur, s.predPitch, s.predVy, in, dt, gm.Veh(self.Vehicle), cmap, solids)
		dx, dz := self.Pos.X-s.predPos.X, self.Pos.Z-s.predPos.Z
		if dx*dx+dz*dz > snapDist*snapDist {
			s.predPos, s.predHull, s.predTur, s.predPitch, s.predVy = self.Pos, self.HullYaw, self.TurretYaw, self.TurretPitch, 0 // respawn/teleport
		} else {
			s.predPos.X += dx * reconcileK
			s.predPos.Z += dz * reconcileK
			s.predPos.Y += (self.Pos.Y - s.predPos.Y) * reconcileK
			s.predHull += angShort(self.HullYaw, s.predHull) * reconcileK
			s.predTur += angShort(self.TurretYaw, s.predTur) * reconcileK
			s.predPitch += (self.TurretPitch - s.predPitch) * reconcileK
		}
	}

	tanks, shots := interpolate(buf)
	var flags []gm.FlagSnap
	var pickups []gm.PickupSnap
	ents := latestEnts // entity facing/HP: snap to latest (no interp)
	var zones []gm.ZoneSnap
	if len(buf) > 0 {
		flags = buf[len(buf)-1].flags     // flags follow carriers but need no interpolation
		pickups = buf[len(buf)-1].pickups // static until grabbed
		zones = buf[len(buf)-1].zones     // zone control: snap to latest
	}
	return viewState{
		ready: true, tanks: tanks, shots: shots, me: s.me, self: self,
		camPos: s.predPos, camYaw: s.predHull + s.predTur, viewTurret: s.predTur, viewPitch: s.predPitch,
		mode: match.Mode, phase: match.Phase, timer: match.Timer, winnerID: match.WinnerID,
		flags: flags, pickups: pickups, ents: ents, zones: zones, flagsLeft: match.FlagsLeft, flagsTotal: match.FlagsTotal, votes: match.Votes,
		mapIdx: match.MapIdx, wave: match.Wave, teamScore: match.TeamScore,
		winnerTeam: match.WinnerTeam, myTeam: self.Team, gmap: cmap,
	}
}

func (s *netSession) close() { s.conn.Close() }

// interpolate renders remote tanks at (now - interpDelay) by lerping the two
// snapshots straddling that time. Projectiles use the newest snapshot (their
// list has no stable identity to interpolate across).
func interpolate(buf []stamped) ([]gm.TankSnap, []gm.V3) {
	n := len(buf)
	if n == 0 {
		return nil, nil
	}
	newest := buf[n-1]
	if n == 1 {
		return newest.tanks, newest.shots
	}
	target := time.Now().Add(-interpDelay)
	if !target.Before(newest.t) {
		return newest.tanks, newest.shots // starved: show the latest
	}
	if target.Before(buf[0].t) {
		return buf[0].tanks, newest.shots // older than history: clamp
	}
	var s0, s1 stamped
	for i := 0; i < n-1; i++ {
		if !buf[i].t.After(target) && !buf[i+1].t.Before(target) {
			s0, s1 = buf[i], buf[i+1]
			break
		}
	}
	span := s1.t.Sub(s0.t).Seconds()
	a := 0.0
	if span > 0 {
		a = target.Sub(s0.t).Seconds() / span
	}
	out := make([]gm.TankSnap, 0, len(s1.tanks))
	for _, t1 := range s1.tanks {
		t := t1
		if t0, ok := findSnap(s0.tanks, t1.ID); ok {
			t.Pos.X = t0.Pos.X + (t1.Pos.X-t0.Pos.X)*a
			t.Pos.Y = t0.Pos.Y + (t1.Pos.Y-t0.Pos.Y)*a
			t.Pos.Z = t0.Pos.Z + (t1.Pos.Z-t0.Pos.Z)*a
			t.HullYaw = t0.HullYaw + angShort(t1.HullYaw, t0.HullYaw)*a
			t.TurretYaw = t0.TurretYaw + angShort(t1.TurretYaw, t0.TurretYaw)*a
		}
		out = append(out, t)
	}
	return out, newest.shots
}

func findSnap(ts []gm.TankSnap, id int) (gm.TankSnap, bool) {
	for i := range ts {
		if ts[i].ID == id {
			return ts[i], true
		}
	}
	return gm.TankSnap{}, false
}

// angShort returns the shortest signed angular delta from b to a (-pi..pi).
func angShort(a, b float64) float64 { return math.Atan2(math.Sin(a-b), math.Cos(a-b)) }

// --- config / identity ---

// defaultINIPath looks for spekder.ini next to the binary (then the working
// directory), falling back to the legacy door.ini name so an existing install
// keeps working without renaming its config.
func defaultINIPath() string {
	var dir string
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	for _, name := range []string{"spekder.ini", "door.ini"} {
		if dir != "" {
			if p := filepath.Join(dir, name); fileExists(p) {
				return p
			}
		}
		if fileExists(name) {
			return name
		}
	}
	// Nothing on disk: return the preferred path so callers log a sensible name.
	if dir != "" {
		return filepath.Join(dir, "spekder.ini")
	}
	return "spekder.ini"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// loadINI reads a flat key=value file (#/; comments and [sections] ignored).
func loadINI(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return m
}

// door32Identity pulls the caller's BBS id (line 4) and handle (line 7) from
// DOOR32.SYS for the server's join log. Best-effort; defaults if absent.
func door32Identity(path string) (bbsid, handle string) {
	bbsid, handle = "local", "player"
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, strings.TrimSpace(sc.Text()))
	}
	if len(lines) >= 4 && lines[3] != "" {
		bbsid = lines[3]
	}
	if len(lines) >= 7 && lines[6] != "" {
		handle = lines[6]
	}
	return
}
