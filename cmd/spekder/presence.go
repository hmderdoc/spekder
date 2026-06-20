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

// autoJoinArmed gates "follow my party into the arena": true by default, cleared
// when you leave an arena match (so a mate still lingering there can't instantly
// yank you back), re-armed by the menu once no party-mate is in the arena. Main
// goroutine only (menu / match loops run sequentially), so no lock needed.
var autoJoinArmed = true

// partyMateInArena reports whether another member of our party is currently in
// the online arena (presence State "online arena") - the cue to follow them in.
func partyMateInArena(pres []proto.Presence, mySession string) bool {
	mine := getParty()
	if mine == "" {
		return false
	}
	for _, p := range pres {
		if p.Session != mySession && p.Party == mine && p.State == "online arena" {
			return true
		}
	}
	return false
}

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
