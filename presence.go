package main

import (
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"spekder/internal/proto"
)

// currentParty is the party name this session has opted into ("" = solo). It
// rides every presence heartbeat and the arena join, so the server groups
// same-party callers onto one team. Set from the PARTY menu; in-memory only.
var (
	partyMu      sync.Mutex
	currentParty string
)

func getParty() string  { partyMu.Lock(); defer partyMu.Unlock(); return currentParty }
func setParty(p string) { partyMu.Lock(); currentParty = p; partyMu.Unlock() }

func presenceSession(dropfile string) string {
	bbsid, handle := door32Identity(dropfile)
	return bbsid + ":" + handle + ":" + strconv.Itoa(os.Getpid())
}

func updatePresence(dropfile, state, detail string) {
	ini := loadINI(defaultINIPath())
	host := ini["server"]
	if host == "" {
		return
	}
	port := ini["port"]
	if port == "" {
		port = "7700"
	}
	bbsid, handle := door32Identity(dropfile)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 500*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = proto.WriteMsg(conn, proto.EncodePresence(ini["token"], proto.Presence{
		Session: presenceSession(dropfile),
		BBSID:   bbsid,
		Handle:  handle,
		State:   state,
		Detail:  detail,
		Party:   getParty(),
	}))
}

func clearPresence(dropfile string) {
	updatePresence(dropfile, "offline", "")
}
