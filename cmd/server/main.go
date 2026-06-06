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
	token string

	mu      sync.Mutex // guards world + clients (all world mutation happens here)
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
	conn.SetReadDeadline(time.Time{})

	s.mu.Lock()
	tank := s.world.AddPlayer([3]float64{}, vehicle)
	id := s.nextID
	s.nextID++
	c := &client{id: id, tank: tank, conn: conn}
	s.clients[id] = c
	s.mu.Unlock()

	if err := proto.WriteMsg(conn, proto.EncodeWelcome(tank)); err != nil {
		s.drop(id, tank)
		return
	}
	s.mu.Lock()
	curMap := s.world.ActiveMap()
	s.mu.Unlock()
	proto.WriteMsg(conn, proto.EncodeMap(curMap)) // so the client can render/collide it
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
		match := s.world.Match()
		var mapMsg []byte
		if s.world.MapIdx != lastMap { // map rotated: tell everyone the new layout
			mapMsg = proto.EncodeMap(s.world.ActiveMap())
			lastMap = s.world.MapIdx
		}
		s.mu.Unlock()

		state := proto.EncodeState(tick, match, tanks, shots, flags, pickups)
		tick++
		for _, conn := range conns {
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if mapMsg != nil {
				_ = proto.WriteMsg(conn, mapMsg)
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
