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

func arenaStatus() (proto.ArenaStatus, error) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return proto.ArenaStatus{}, &netErr{"no arena server configured"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 750*time.Millisecond)
	if err != nil {
		return proto.ArenaStatus{}, err
	}
	defer conn.Close()
	if err := proto.WriteMsg(conn, proto.EncodeStatusQuery(ini["token"])); err != nil {
		return proto.ArenaStatus{}, err
	}
	conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	msg, err := proto.ReadMsg(conn)
	if err != nil {
		return proto.ArenaStatus{}, err
	}
	st, ok := proto.DecodeStatus(msg)
	if !ok {
		return proto.ArenaStatus{}, &netErr{"server did not send STATUS"}
	}
	return st, nil
}

func sendChat(dropfile, text string) error {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return &netErr{"no arena server configured"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	bbsid, handle := door32Identity(dropfile)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 750*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(750 * time.Millisecond))
	return proto.WriteMsg(conn, proto.EncodeChat(ini["token"], proto.ChatMessage{
		BBSID:  bbsid,
		Handle: handle,
		Text:   text,
	}))
}

// sendPartyKick asks the arena to boot target from the caller's party (one-shot,
// best-effort). The server validates that the caller owns the party.
func sendPartyKick(dropfile, party, target string) error {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return &netErr{"no arena server configured"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	_, handle := door32Identity(dropfile)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 750*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(750 * time.Millisecond))
	return proto.WriteMsg(conn, proto.EncodePartyKick(ini["token"], handle, party, target))
}

// syncPartyFromStatus reconciles our local party with the server's view (found by
// our own session in the presence list) - so being kicked, which the server
// reflects by reporting our party as empty, takes effect locally.
func syncPartyFromStatus(pres []proto.Presence, mySession string) {
	for _, p := range pres {
		if p.Session == mySession {
			setParty(p.Party)
			return
		}
	}
}

// connectArena dials the configured arena server and joins as a client with the
// chosen vehicle.
func connectArena(dropfile string, body int, color [3]float64) (*netSession, error) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return nil, &netErr{"no arena server configured"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	bbsid, handle := door32Identity(dropfile) // the real dropfile, not a cwd guess
	ns, err := dialServer(host, port, ini["token"], bbsid, handle, color, body)
	if err != nil {
		return nil, err
	}
	logf("connected to arena %s:%s as tank %d", host, port, ns.me)
	return ns, nil
}

// publishMap uploads an author map to the configured arena's repository. It's a
// one-shot connection: HELLO (with the publish sentinel, so no tank spawns) ->
// MsgPublish -> MsgPubAck. Returns the server's status message.
func publishMap(m gm.Map) (string, error) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return "", &netErr{"no arena server configured (set server= in spekder.ini)"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	bbsid, handle := door32Identity(doorDropfile)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := proto.WriteMsg(conn, proto.EncodeHello(ini["token"], bbsid, handle, [3]float64{}, gm.BodyTank, true, proto.ProtocolVersion, version, "")); err != nil {
		return "", err
	}
	if err := proto.WriteMsg(conn, proto.EncodePublish(m)); err != nil {
		return "", err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := proto.ReadMsg(conn)
	if err != nil {
		return "", err
	}
	ok, text, good := proto.DecodePubAck(msg)
	if !good {
		return "", &netErr{"unexpected reply from server"}
	}
	if !ok {
		return "", &netErr{text}
	}
	return text, nil
}

// bbsSource is the source-board name for the global board (door.ini bbsname,
// else the DOOR32 bbs id).
func bbsSource(ini map[string]string) string {
	if n := ini["bbsname"]; n != "" {
		return n
	}
	bbsid, _ := door32Identity(doorDropfile)
	return bbsid
}

// submitScore sends a finished match's score to the arena's global board (one-shot,
// best-effort; safe to run in a goroutine - it just returns on any error).
func submitScore(r matchResult, name string) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = proto.WriteMsg(conn, proto.EncodeScore(proto.ScoreSubmit{
		Token: ini["token"], Mode: r.ModeName, Name: name, BBS: bbsSource(ini),
		Map: r.MapName, Score: r.Score, When: uint32(r.When),
		Won: r.Won, Kills: r.Kills, Deaths: r.Deaths,
		ShotsFired: r.ShotsFired, ShotsHit: r.ShotsHit, Wave: r.Wave,
	}))
}

// queryGlobalScores fetches the arena's global high-score board.
func queryGlobalScores() ([]proto.ScoreRow, error) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return nil, &netErr{"no arena server configured"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := proto.WriteMsg(conn, proto.EncodeScoreQuery(ini["token"])); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := proto.ReadMsg(conn)
	if err != nil {
		return nil, err
	}
	rows, ok := proto.DecodeScores(msg)
	if !ok {
		return nil, &netErr{"unexpected reply from server"}
	}
	return rows, nil
}

// queryGlobalPlayers fetches the arena's aggregated career-stats board (winningest
// / skill / survival-wave boards). An older arena that predates the message simply
// closes the connection, which surfaces here as an error the caller skips over.
func queryGlobalPlayers() ([]proto.PlayerRow, error) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return nil, &netErr{"no arena server configured"}
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := proto.WriteMsg(conn, proto.EncodePlayersQuery(ini["token"])); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := proto.ReadMsg(conn)
	if err != nil {
		return nil, err
	}
	rows, ok := proto.DecodePlayers(msg)
	if !ok {
		return nil, &netErr{"unexpected reply from server"}
	}
	return rows, nil
}

type stamped struct {
	t       time.Time
	tanks   []gm.TankSnap
	shots   []gm.ShotSnap
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
	curMap    gm.Map             // active map, sent by the server (MsgMap)
	pairings  []proto.LobbyEntry // votable map+mode candidates (MsgLobby)
	previews  map[int]gm.Map     // lazy lobby previews by map index (MsgMapPreview)
	reqPrev   map[int]bool       // indices already requested this connection (dedupe)
	haveSelf  bool
	dead      bool

	wmu sync.Mutex // serializes conn writes (step input + async preview requests)

	// prediction state — main goroutine only
	predPos   gm.V3
	predHull  float64
	predTur   float64
	predPitch float64
	predVy    float64
	predInit  bool
}

func dialServer(host, port, token, bbsid, handle string, color [3]float64, body int) (*netSession, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return nil, err
	}
	if err := proto.WriteMsg(conn, proto.EncodeHello(token, bbsid, handle, color, body, false, proto.ProtocolVersion, version, getParty())); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := proto.ReadMsg(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if reason, ok := proto.DecodeReject(msg); ok { // version/compat refusal: show the reason verbatim
		conn.Close()
		return nil, &rejectErr{reason}
	}
	me, ok := proto.DecodeWelcome(msg)
	if !ok {
		conn.Close()
		return nil, &netErr{"server did not send WELCOME"}
	}
	conn.SetReadDeadline(time.Time{})
	ns := &netSession{conn: conn, me: me, previews: map[int]gm.Map{}, reqPrev: map[int]bool{}}
	go ns.readLoop()
	go ns.prefetchLoop()
	return ns, nil
}

// prefetchLoop warms the lobby map-preview cache in the background: it trickles a
// request for each not-yet-fetched map in the pool so the lobby is instant
// instead of paying a round-trip per highlight. It throttles (one request at a
// time) and pauses during an active match, so it never competes with the live
// state stream over a slow link. The on-demand path still front-runs it for the
// highlighted map (requestPreview dedupes), so a selection is never stuck behind
// the trickle. Exits once every known map is requested or the link dies.
func (s *netSession) prefetchLoop() {
	for {
		s.mu.Lock()
		dead, n, phase := s.dead, len(s.pairings), s.lastMatch.Phase
		next := -1
		for i := 0; i < n; i++ {
			if !s.reqPrev[i] {
				next = i
				break
			}
		}
		s.mu.Unlock()
		switch {
		case dead:
			return
		case next < 0 && n > 0:
			return // every map in the pool has been resolved; cache is warm
		case next < 0 || phase == gm.PhaseActive:
			time.Sleep(500 * time.Millisecond) // pool not known yet, or hold during live play
		default:
			if s.ensurePreview(next) {
				time.Sleep(150 * time.Millisecond) // a real download: be gentle on the wire
			} // a disk-cache hit is instant - move straight to the next map
		}
	}
}

type netErr struct{ s string }

func (e *netErr) Error() string { return e.s }

// rejectErr is a server MsgReject reason (e.g. a version mismatch). The menu
// shows its text verbatim - it's already a complete, actionable sentence - so
// callers can distinguish it from a plain connection failure.
type rejectErr struct{ s string }

func (e *rejectErr) Error() string { return e.s }

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
		if len(msg) > 0 && msg[0] == proto.MsgLobby {
			if ps, ok := proto.DecodeLobby(msg); ok {
				s.mu.Lock()
				s.pairings = ps
				s.mu.Unlock()
				logf("received %d lobby pairings", len(ps))
			}
			continue
		}
		if len(msg) > 0 && msg[0] == proto.MsgMapPreview {
			if idx, m, ok := proto.DecodeMapPreview(msg); ok {
				s.mu.Lock()
				s.previews[idx] = m
				name, hash, haveMan := "", uint32(0), idx >= 0 && idx < len(s.pairings)
				if haveMan {
					name, _ = splitCapTag(s.pairings[idx].Name)
					hash = s.pairings[idx].Hash
				}
				s.mu.Unlock()
				if haveMan { // persist to the shared on-disk cache for next time / other users
					mapCacheStore(name, hash, m)
				}
				logf("received preview for map %d (%q, %d obstacles)", idx, m.Name, len(m.Obstacles))
			}
			continue
		}
		if len(msg) > 0 && msg[0] == proto.MsgBalance {
			if bal, ok := proto.DecodeBalance(msg); ok {
				gm.ApplyBalance(bal) // adopt the arena's authoritative tuning over our compiled defaults
				logf("received roster balance (%d rows, %d bodies)", len(bal.Rows), len(bal.Bodies))
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

// sendChangeChar tells the arena to swap our character; it takes effect on our
// next respawn. Best-effort on the live game connection (serialized with step).
func (s *netSession) sendChangeChar(body int, color [3]float64) {
	token := loadINI(defaultINIPath())["token"]
	s.wmu.Lock()
	s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = proto.WriteMsg(s.conn, proto.EncodeChangeChar(token, color, body))
	s.wmu.Unlock()
}

func (s *netSession) step(dt float64, in gm.Input) viewState {
	s.wmu.Lock()
	s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = proto.WriteMsg(s.conn, proto.EncodeInput(in)) // best-effort; readLoop notices a dead link
	s.wmu.Unlock()

	s.mu.Lock()
	haveSelf := s.haveSelf
	self := s.lastSelf
	match := s.lastMatch
	cmap := s.curMap
	pairings := s.pairings
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
		pveh := gm.VehBody(self.Body)
		pveh.Speed *= gm.BodySpeedMul(self.Body) // match the sim's per-body speed (e.g. the fast insect)
		s.predPos, s.predHull, s.predTur, s.predPitch, s.predVy = gm.Predict(s.predPos, s.predHull, s.predTur, s.predPitch, s.predVy, in, dt, pveh, cmap, solids)
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
		flags: flags, pickups: pickups, ents: ents, zones: zones, flagsLeft: match.FlagsLeft, flagsTotal: match.FlagsTotal, votes: match.Votes, lobbyReady: match.Ready,
		pairings: pairings, kills: match.Kills, events: match.Events, mapIdx: match.MapIdx, wave: match.Wave, teamScore: match.TeamScore,
		winnerTeam: match.WinnerTeam, myTeam: self.Team, gmap: cmap,
	}
}

func (s *netSession) close() { s.conn.Close() }

// ensurePreview makes map idx available for the lobby, cheapest source first:
// already in memory -> nothing; on the shared on-disk cache (matching name+hash)
// -> load it, no network; otherwise -> request it from the arena. Returns true
// only when it had to hit the network, so the background prefetcher throttles
// downloads but blows through cache hits instantly. Safe to call every frame.
func (s *netSession) ensurePreview(idx int) (fetched bool) {
	if idx < 0 {
		return false
	}
	s.mu.Lock()
	_, inMem := s.previews[idx]
	done := inMem || s.reqPrev[idx]
	name, hash, haveMan := "", uint32(0), idx < len(s.pairings)
	if haveMan {
		name, _ = splitCapTag(s.pairings[idx].Name)
		hash = s.pairings[idx].Hash
	}
	s.mu.Unlock()
	if done {
		return false // already have it, or already fetching/loaded
	}
	if haveMan {
		if m, ok := mapCacheLoad(name, hash); ok { // served from disk; no download
			s.mu.Lock()
			s.previews[idx] = m
			s.reqPrev[idx] = true
			s.mu.Unlock()
			return false
		}
	}
	s.requestPreview(idx)
	return true
}

// requestPreview asks the server for map idx's geometry once (lobby preview). The
// write runs on its own goroutine so a slow socket can never stall the render
// loop - the reply arrives later via readLoop and the pane swaps it in. Dedupes
// per connection: one ask is enough on a reliable TCP link.
func (s *netSession) requestPreview(idx int) {
	if idx < 0 {
		return
	}
	s.mu.Lock()
	if s.reqPrev[idx] {
		s.mu.Unlock()
		return
	}
	s.reqPrev[idx] = true
	s.mu.Unlock()
	go func() {
		s.wmu.Lock()
		s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = proto.WriteMsg(s.conn, proto.EncodeMapReq(idx)) // best-effort
		s.wmu.Unlock()
	}()
}

// preview returns a cached lobby map preview by index (ok=false until it lands).
func (s *netSession) preview(idx int) (gm.Map, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.previews[idx]
	return m, ok
}

// interpolate renders remote tanks at (now - interpDelay) by lerping the two
// snapshots straddling that time. Projectiles use the newest snapshot (their
// list has no stable identity to interpolate across).
func interpolate(buf []stamped) ([]gm.TankSnap, []gm.ShotSnap) {
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

// iniTruthy interprets a flat-ini boolean value ("true"/"yes"/"on"/"1";
// anything else, including absent, is false).
func iniTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// doorDropfile is the resolved dropfile path (set once at startup). Identity
// lookups that run off the main flow - score submission and map publishing, which
// don't carry the path - read it instead of guessing "DOOR32.SYS" in the cwd,
// which fails (and yields the "player" default) when the door is launched with
// -dropfile pointing at a node directory.
var doorDropfile = "DOOR32.SYS"

// door32Identity pulls the caller's BBS id (line 4) and handle (line 7) from
// the DOOR32.SYS dropfile for the server's join log. Best-effort; defaults if absent.
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

// dropfileHasUser reports whether DOOR32.SYS names a logged-in user (a handle on
// line 7). A pre-login launch (login.js bbs.exec with no dropfile, or a dropfile
// with no alias) returns false, so the door can attract-loop on entry and skip
// recording scores under the shared anonymous "player" bucket.
func dropfileHasUser(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, strings.TrimSpace(sc.Text()))
	}
	return len(lines) >= 7 && lines[6] != ""
}
