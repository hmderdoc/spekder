// Spekder arena server: the authoritative game. It owns one World, accepts door
// clients (token-gated), folds their inputs into a fixed-rate simulation, and
// broadcasts a STATE snapshot every tick. The wire carries state, never pixels.
//
// Run it yourself (e.g. via the systemd unit); the door connects out to it.
package main

import (
	"flag"
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

// version is stamped at build time by the release workflow (-X main.version).
var version = "dev"

type client struct {
	id   int
	tank int // index into world.Tanks
	conn net.Conn

	mu sync.Mutex
	in gm.Input
}

type server struct {
	token   string
	mapsDir string // where published maps are written

	mu      sync.Mutex // guards world + clients + the map pool (all mutation here)
	world   *gm.World
	clients map[int]*client
	nextID  int
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	hello, err := proto.ReadMsg(conn)
	if err != nil {
		return
	}
	token, bbsid, handle, vehicle, ok := proto.DecodeHello(hello)
	if !ok || token != s.token {
		log.Printf("rejected %v (bad hello/token)", conn.RemoteAddr())
		return
	}
	if vehicle == proto.PublishVehicle { // publish-only connection: no tank
		s.handlePublish(conn, bbsid, handle)
		return
	}
	conn.SetReadDeadline(time.Time{})

	s.mu.Lock()
	tank := s.world.AddPlayer([3]float64{}, vehicle)
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
	proto.WriteMsg(conn, proto.EncodeMap(curMap)) // so the client can render/collide it
	proto.WriteMsg(conn, lobby)                   // votable map+mode pairings
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
		}
	}
	s.drop(id, tank)
	log.Printf("leave: tank %d (%d online)", tank, len(s.clients))
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
		conns := make([]net.Conn, 0, len(s.clients))
		for _, c := range s.clients {
			c.mu.Lock()
			inputs[c.tank] = c.in
			c.mu.Unlock()
			conns = append(conns, c.conn)
		}
		s.world.Update(dt, inputs)
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
		for _, conn := range conns {
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if mapMsg != nil {
				_ = proto.WriteMsg(conn, mapMsg)
			}
			if lobbyMsg != nil {
				_ = proto.WriteMsg(conn, lobbyMsg)
			}
			_ = proto.WriteMsg(conn, state) // a failing write trips the reader loop, which drops the client
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
	flag.Parse()

	if n := gm.LoadMapDir(*mapsDir); n > 0 {
		log.Printf("loaded %d author map(s) from %s", n, *mapsDir)
	}
	mode := parseMode(*modeName)
	world := gm.NewWorld(*bots, mode)
	world.Lobby = true // server arenas run the between-match vote lobby + rotation
	s := &server{
		token:   *token,
		mapsDir: *mapsDir,
		world:   world,
		clients: make(map[int]*client),
	}
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
