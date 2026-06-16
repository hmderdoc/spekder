// Spekder arena server: the authoritative game. It owns one World, accepts door
// clients (token-gated), folds their inputs into a fixed-rate simulation, and
// broadcasts a STATE snapshot every tick. The wire carries state, never pixels.
//
// Run it yourself (e.g. via the systemd unit); the door connects out to it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gm "spekder/internal/game"
	"spekder/internal/proto"
)

// loadBalance applies a roster-tuning override file, if present, to the live
// stat tables - which CurrentBalance() then pushes to every joining client. A
// missing file is fine (use the built-in numbers). The file is a complete
// Balance dump (generate a template with -dumpbalance); editing its numbers and
// restarting the server retunes the roster with no rebuild and no client update.
func loadBalance(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // optional
	}
	if err != nil {
		return err
	}
	var bal gm.Balance
	if err := json.Unmarshal(data, &bal); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(bal.Rows) == 0 && len(bal.Bodies) == 0 {
		return fmt.Errorf("%s has no character rows or body entries", path)
	}
	gm.ApplyBalance(bal)
	log.Printf("balance: applied tuning from %s (%d rows, %d bodies)", path, len(bal.Rows), len(bal.Bodies))
	return nil
}

// dumpBalance writes the current (built-in) tuning to path as a JSON template a
// sysop can edit. It reflects the compiled numbers verbatim - no opinion baked in.
func dumpBalance(path string) error {
	data, err := json.MarshalIndent(gm.CurrentBalance(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// version is stamped at build time by the release workflow (-X main.version).
var version = "dev"

type client struct {
	id   int
	tank int // index into world.Tanks
	conn net.Conn

	mu  sync.Mutex
	in  gm.Input
	out [][]byte // queued server->client messages (e.g. lazy map previews), drained by run()
}

type server struct {
	token       string
	mapsDir     string // where published maps are written
	scoresPath  string // global high-score board file (JSON)
	playersPath string // per-player career aggregate file (JSON)
	board       map[string][]proto.ScoreRow
	players     map[string]*proto.PlayerRow // keyed by Name@BBS (playerKey)

	mu       sync.Mutex // guards world + clients + presence + the map pool (all mutation here)
	world    *gm.World
	clients  map[int]*client
	presence map[string]presenceEntry
	kicks    map[string]time.Time // party\x00handle -> expiry: members an owner has booted
	chat     []proto.ChatMessage
	nextChat uint32
	nextID   int
}

type presenceEntry struct {
	rec proto.Presence
	at  time.Time
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	hello, err := proto.ReadMsg(conn)
	if err != nil {
		return
	}
	if token, ok := proto.DecodeStatusQuery(hello); ok {
		if token != s.token {
			log.Printf("rejected %v (bad status token)", conn.RemoteAddr())
			return
		}
		s.handleStatus(conn)
		return
	}
	if token, pr, ok := proto.DecodePresence(hello); ok {
		if token != s.token {
			log.Printf("rejected %v (bad presence token)", conn.RemoteAddr())
			return
		}
		s.handlePresence(pr)
		return
	}
	if token, msg, ok := proto.DecodeChat(hello); ok {
		if token != s.token {
			log.Printf("rejected %v (bad chat token)", conn.RemoteAddr())
			return
		}
		s.handleChat(msg)
		return
	}
	if token, ownerHandle, party, target, ok := proto.DecodePartyKick(hello); ok {
		if token != s.token {
			log.Printf("rejected %v (bad party-kick token)", conn.RemoteAddr())
			return
		}
		s.handlePartyKick(ownerHandle, party, target)
		return
	}
	if sub, ok := proto.DecodeScore(hello); ok {
		if sub.Token != s.token {
			log.Printf("rejected %v (bad score token)", conn.RemoteAddr())
			return
		}
		s.recordScore(sub)
		return
	}
	if token, ok := proto.DecodeScoreQuery(hello); ok {
		if token != s.token {
			log.Printf("rejected %v (bad score-query token)", conn.RemoteAddr())
			return
		}
		_ = proto.WriteMsg(conn, proto.EncodeScores(s.scoreRows()))
		return
	}
	if token, ok := proto.DecodePlayersQuery(hello); ok {
		if token != s.token {
			log.Printf("rejected %v (bad players-query token)", conn.RemoteAddr())
			return
		}
		_ = proto.WriteMsg(conn, proto.EncodePlayers(s.playerRows()))
		return
	}
	token, bbsid, handle, vehicle, color, custom, body, clientProto, clientVer, party, ok := proto.DecodeHello(hello)
	if !ok || token != s.token {
		log.Printf("rejected %v (bad hello/token)", conn.RemoteAddr())
		return
	}
	if vehicle == proto.PublishVehicle { // publish-only connection: no tank
		s.handlePublish(conn, bbsid, handle)
		return
	}
	// Wire-compatibility guard: a join whose protocol doesn't match would
	// half-talk and corrupt state, so refuse it with a reason the client shows
	// (naming the version to update to, on whichever side is behind).
	if clientProto != proto.ProtocolVersion {
		if clientVer == "" {
			clientVer = "an older build"
		}
		reason := fmt.Sprintf("Version mismatch: this arena runs spekder %s (protocol %d); your client is %s (protocol %d). ",
			version, proto.ProtocolVersion, clientVer, clientProto)
		if clientProto < proto.ProtocolVersion {
			reason += "Update your client to " + version + "."
		} else {
			reason += "Ask the sysop to update this arena to " + clientVer + "."
		}
		_ = proto.WriteMsg(conn, proto.EncodeReject(reason))
		log.Printf("rejected %v (protocol %d != %d)", conn.RemoteAddr(), clientProto, proto.ProtocolVersion)
		return
	}
	conn.SetReadDeadline(time.Time{})

	var customVeh *gm.Vehicle // custom build: chassis renders, these stats sim
	if custom != nil {
		v := gm.MakeCustom(vehicle, *custom)
		customVeh = &v
	}
	s.mu.Lock()
	if s.isKickedLocked(party, handle) {
		party = "" // booted from that party: they join solo, not on the owner's side
	}
	tank := s.world.AddPlayerCustom(color, vehicle, handle, customVeh, body) // dedups to a free color
	s.world.SetPlayerParty(tank, party)                                      // team-grouping hint
	id := s.nextID
	s.nextID++
	c := &client{id: id, tank: tank, conn: conn}
	s.clients[id] = c
	if s.world.HumanCount() == 1 {
		s.world.ForceLobby() // first human in a bot arena: open the pick lobby
	}
	curMap := s.world.ActiveMap()
	lobby := proto.EncodeLobby()
	s.mu.Unlock()

	if err := proto.WriteMsg(conn, proto.EncodeWelcome(tank)); err != nil {
		s.drop(id, tank)
		return
	}
	proto.WriteMsg(conn, proto.EncodeBalance(gm.CurrentBalance())) // authoritative roster tuning (additive; old clients ignore)
	proto.WriteMsg(conn, proto.EncodeMap(curMap))                  // so the client can render/collide it
	proto.WriteMsg(conn, lobby)                                    // votable map+mode pairings
	log.Printf("join: %q@%q -> tank %d (%d online)", handle, bbsid, tank, len(s.clients))

	for {
		conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		msg, err := proto.ReadMsg(conn)
		if err != nil {
			break
		}
		if in, ok := proto.DecodeInput(msg); ok {
			c.mu.Lock()
			c.in = in
			c.mu.Unlock()
			continue
		}
		if tok, veh, col, cust, bod, ok := proto.DecodeChangeChar(msg); ok && tok == s.token {
			var cv *gm.Vehicle
			if cust != nil {
				v := gm.MakeCustom(veh, *cust)
				cv = &v
			}
			s.mu.Lock()
			s.world.SetPlayerLoadout(c.tank, col, veh, cv, bod)
			s.mu.Unlock()
			continue
		}
		if idx, ok := proto.DecodeMapReq(msg); ok { // lazy lobby preview request
			s.mu.Lock()
			var preview []byte
			if idx >= 0 && idx < len(gm.Maps) {
				preview = proto.EncodeMapPreview(idx, gm.Maps[idx])
			}
			s.mu.Unlock()
			if preview != nil { // queued; run() writes it on the same conn (no write race)
				c.mu.Lock()
				c.out = append(c.out, preview)
				c.mu.Unlock()
			}
		}
	}
	s.drop(id, tank)
	log.Printf("leave: tank %d (%d online)", tank, len(s.clients))
}

func (s *server) handleStatus(conn net.Conn) {
	s.mu.Lock()
	s.prunePresenceLocked(time.Now())
	match := s.world.Match()
	curMap := s.world.ActiveMap()
	st := proto.ArenaStatus{
		Humans:   s.world.HumanCount(),
		Phase:    match.Phase,
		Mode:     match.Mode,
		Map:      curMap.Name,
		Presence: s.presenceListLocked(),
		Chat:     append([]proto.ChatMessage(nil), s.chat...),
		// The server and the matching client release ship together, so this
		// build's version is also the client version the arena recommends.
		ServerVersion: version,
		LatestClient:  version,
	}
	s.mu.Unlock()
	_ = proto.WriteMsg(conn, proto.EncodeStatus(st))
}

func (s *server) handlePresence(pr proto.Presence) {
	s.mu.Lock()
	s.setPresenceLocked(pr)
	s.prunePresenceLocked(time.Now())
	s.mu.Unlock()
}

func (s *server) setPresenceLocked(pr proto.Presence) {
	if pr.Session == "" {
		return
	}
	if pr.State == "offline" {
		delete(s.presence, pr.Session)
		return
	}
	now := time.Now()
	pr.Updated = uint32(now.Unix())
	if s.isKickedLocked(pr.Party, pr.Handle) {
		pr.Party = "" // booted from that party: report (and treat) them as solo
	}
	s.presence[pr.Session] = presenceEntry{rec: pr, at: now}
}

func kickKey(party, handle string) string { return party + "\x00" + handle }

func (s *server) isKickedLocked(party, handle string) bool {
	if party == "" {
		return false
	}
	exp, ok := s.kicks[kickKey(party, handle)]
	return ok && time.Now().Before(exp)
}

// handlePartyKick records an owner booting a member. Ownership is by naming
// convention: a party is named after its creator's handle, so the kick is valid
// only when the sender's handle matches the party name. The booted member's
// presence is masked to solo immediately and stays barred from that party for a
// couple of minutes (long enough for their client to notice and re-pick).
func (s *server) handlePartyKick(ownerHandle, party, target string) {
	if ownerHandle == "" || party == "" || target == "" || ownerHandle != party {
		return // not the owner of that party
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kicks[kickKey(party, target)] = time.Now().Add(2 * time.Minute)
	for sess, ent := range s.presence { // boot them now if they're online
		if ent.rec.Handle == target && ent.rec.Party == party {
			ent.rec.Party = ""
			s.presence[sess] = ent
		}
	}
}

func (s *server) prunePresenceLocked(now time.Time) {
	// 25s ~= 2.5 missed heartbeats (the door beats every 10s in-match, ~5s in
	// menus), so a disconnected demo caller drops off the online count quickly
	// without pruning a live player between beats.
	for k, ent := range s.presence {
		if now.Sub(ent.at) > 25*time.Second {
			delete(s.presence, k)
		}
	}
	for k, exp := range s.kicks {
		if now.After(exp) {
			delete(s.kicks, k)
		}
	}
}

func (s *server) presenceListLocked() []proto.Presence {
	out := make([]proto.Presence, 0, len(s.presence))
	for _, ent := range s.presence {
		out = append(out, ent.rec)
	}
	return out
}

// appendSystemChatLocked posts a server-generated notice into the chat stream
// (empty handle => the client renders it without a "name:" prefix). Caller holds
// s.mu. Used for lobby vote announcements ("X voted for MAP").
func (s *server) appendSystemChatLocked(text string) {
	s.nextChat++
	s.chat = append(s.chat, proto.ChatMessage{Seq: s.nextChat, Time: uint32(time.Now().Unix()), Text: text})
	if len(s.chat) > 50 {
		s.chat = s.chat[len(s.chat)-50:]
	}
}

func (s *server) handleChat(msg proto.ChatMessage) {
	if strings.TrimSpace(msg.Text) == "" {
		return
	}
	s.mu.Lock()
	s.nextChat++
	msg.Seq = s.nextChat
	msg.Time = uint32(time.Now().Unix())
	s.chat = append(s.chat, msg)
	if len(s.chat) > 50 {
		s.chat = s.chat[len(s.chat)-50:]
	}
	s.mu.Unlock()
	log.Printf("chat: %q@%q: %.80q", msg.Handle, msg.BBSID, msg.Text)
}

// handlePublish receives one map from a publish-only connection, validates it,
// persists it to the maps repo (keyed by BBS id so peers don't collide), adds it
// to the live pool, and replies with a status. Open/trusted: any door with the
// connect token can publish.
func (s *server) handlePublish(conn net.Conn, bbsid, handle string) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	msg, err := proto.ReadMsg(conn)
	if err != nil {
		return
	}
	m, ok := proto.DecodePublish(msg)
	if !ok {
		_ = proto.WriteMsg(conn, proto.EncodePubAck(false, "could not decode map"))
		return
	}
	if gm.FatalIssues(gm.ValidateMap(m)) {
		_ = proto.WriteMsg(conn, proto.EncodePubAck(false, "map has fatal errors; fix and retry"))
		return
	}
	fname := pubSlug(bbsid) + "-" + pubSlug(m.Name) + ".json"
	data, err := gm.MapJSON(m)
	if err != nil {
		_ = proto.WriteMsg(conn, proto.EncodePubAck(false, "encode failed"))
		return
	}
	if err := os.MkdirAll(s.mapsDir, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(s.mapsDir, fname), data, 0o644)
	}
	s.mu.Lock()
	idx := gm.UpsertMap(m)
	s.mu.Unlock()
	log.Printf("publish: %q@%q -> %q (pool index %d, file %s)", handle, bbsid, m.Name, idx, fname)
	_ = proto.WriteMsg(conn, proto.EncodePubAck(true, "published "+m.Name+" to the arena"))
}

// pubSlug makes a filename-safe fragment from a name/id.
func pubSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

func (s *server) drop(id, tank int) {
	s.mu.Lock()
	if _, ok := s.clients[id]; ok {
		delete(s.clients, id)
		s.world.RemovePlayer(tank)
	}
	s.mu.Unlock()
}

func (s *server) run(tickHz float64) {
	dt := 1.0 / tickHz
	ticker := time.NewTicker(time.Duration(float64(time.Second) / tickHz))
	defer ticker.Stop()
	var tick uint32
	lastMap := -1
	lastPhase := gm.Phase(-1)
	for range ticker.C {
		s.mu.Lock()
		inputs := make(map[int]gm.Input, len(s.clients))
		type outbound struct {
			conn    net.Conn
			pending [][]byte
		}
		outs := make([]outbound, 0, len(s.clients))
		for _, c := range s.clients {
			c.mu.Lock()
			inputs[c.tank] = c.in
			pending := c.out
			c.out = nil
			c.mu.Unlock()
			outs = append(outs, outbound{c.conn, pending})
		}
		s.world.Update(dt, inputs)
		for _, ve := range s.world.DrainVoteLog() { // lobby votes -> chat toasts
			name := "a map"
			if ve.MapIdx >= 0 && ve.MapIdx < len(gm.Maps) {
				if name = gm.Maps[ve.MapIdx].Name; name == "" {
					name = "an unnamed map"
				}
			}
			s.appendSystemChatLocked(ve.Who + " voted for " + name)
		}
		tanks, shots, flags, pickups := s.world.Snapshot()
		ents := s.world.Entities()
		zones := s.world.Zones()
		match := s.world.Match()
		var mapMsg []byte
		if s.world.MapIdx != lastMap { // map rotated: tell everyone the new layout
			mapMsg = proto.EncodeMap(s.world.ActiveMap())
			lastMap = s.world.MapIdx
		}
		var lobbyMsg []byte
		if match.Phase == gm.PhaseLobby && lastPhase != gm.PhaseLobby {
			lobbyMsg = proto.EncodeLobby() // entering the vote lobby: refresh candidates
		}
		lastPhase = match.Phase
		s.mu.Unlock()

		state := proto.EncodeState(tick, match, tanks, shots, flags, pickups, ents, zones)
		tick++
		for _, o := range outs {
			o.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if mapMsg != nil {
				_ = proto.WriteMsg(o.conn, mapMsg)
			}
			if lobbyMsg != nil {
				_ = proto.WriteMsg(o.conn, lobbyMsg)
			}
			for _, m := range o.pending { // lazy map previews this client asked for
				_ = proto.WriteMsg(o.conn, m)
			}
			_ = proto.WriteMsg(o.conn, state) // a failing write trips the reader loop, which drops the client
		}
	}
}

func parseMode(s string) gm.Mode {
	switch s {
	case "flag", "flagrun":
		return gm.ModeFlagRun
	case "ctf":
		return gm.ModeCTF
	case "survival":
		return gm.ModeSurvival
	default:
		return gm.ModeDeathmatch
	}
}

func main() {
	addr := flag.String("addr", ":7700", "listen address")
	token := flag.String("token", "", "shared secret a door must present in HELLO")
	bots := flag.Int("bots", 4, "AI tanks that fill the arena")
	tickHz := flag.Float64("tick", 20, "simulation/broadcast rate (Hz)")
	modeName := flag.String("mode", "dm", "starting arena mode: dm | flag | ctf | survival (the lobby rotates/votes among all modes)")
	mapsDir := flag.String("maps", "maps", "directory of extra author maps (*.json), sent to clients over the wire")
	scoresFile := flag.String("scores", "scores.json", "global high-score board file")
	playersFile := flag.String("players", "players.json", "per-player career aggregate file")
	balanceFile := flag.String("balance", "balance.json", "optional roster tuning overrides (JSON); pushed to clients. Missing = use built-in numbers")
	dumpBal := flag.Bool("dumpbalance", false, "write the current tuning to -balance as an editable template, then exit")
	flag.Parse()

	if *dumpBal {
		if err := dumpBalance(*balanceFile); err != nil {
			log.Fatalf("dumpbalance: %v", err)
		}
		log.Printf("wrote balance template to %s", *balanceFile)
		return
	}
	if n := gm.LoadMapDir(*mapsDir); n > 0 {
		log.Printf("loaded %d author map(s) from %s", n, *mapsDir)
	}
	if err := loadBalance(*balanceFile); err != nil {
		log.Printf("balance: %v (using built-in numbers)", err)
	}
	mode := parseMode(*modeName)
	world := gm.NewWorld(*bots, mode)
	world.Lobby = true // server arenas run the between-match vote lobby + rotation
	s := &server{
		token:       *token,
		mapsDir:     *mapsDir,
		scoresPath:  *scoresFile,
		playersPath: *playersFile,
		world:       world,
		clients:     make(map[int]*client),
		presence:    make(map[string]presenceEntry),
		kicks:       make(map[string]time.Time),
		chat:        make([]proto.ChatMessage, 0, 50),
	}
	s.loadScores()
	go s.run(*tickHz)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("spekder arena %s on %s (%s, %d bots, %.0f Hz)", version, *addr, mode, *bots, *tickHz)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go s.handle(conn)
	}
}
