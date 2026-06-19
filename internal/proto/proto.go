// Package proto is the Spekder wire protocol: length-prefixed binary messages
// carrying game STATE and INPUT (never pixels). Framing: uint32 big-endian
// length + payload; payload[0] is the message type.
package proto

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"strconv"

	gm "spekder/internal/game"
)

// MapHash is a content fingerprint of a map (FNV-1a over its wire encoding),
// used to version the client-side preview cache: same bytes -> same hash -> the
// cached copy is reused; an edited map changes the hash and is refetched.
func MapHash(m gm.Map) uint32 {
	h := fnv.New32a()
	h.Write(EncodeMap(m))
	return h.Sum32()
}

// ProtocolVersion is the wire-compatibility number. Bump it ONLY on a
// wire-breaking change (a message's layout changes incompatibly). It is sent in
// MsgHello and the server hard-rejects a join whose number doesn't match, so a
// client and server from mismatched releases never half-talk and corrupt state.
// It is independent of the human-facing release version (which can move without
// a wire break). Start: 1.
//
//	2: TankSnap gained the minotaur barrier (fl2 bit + a barrier-charge byte).
//	3: added MsgMapReq/MsgMapPreview for lazy lobby map previews.
//	4: MsgLobby entries carry a content hash (cache versioning).
//	5: Input gained the lobby Ready bit; MatchSnap gained the locked-vote count.
//	6: removed the wire vehicle byte and the CustomStats tail; HELLO/CHANGECHAR carry body + a publish flag (the chassis system retired).
//	7: TankSnap gained the secondary gauge (reload2 + charge count) and a slip flag.
//	8: EntitySnap can be self-describing (Spawned bit + kind/half/colour/maxhp) so runtime-spawned entities (crab turret) render without an authored template.
//	9: MatchSnap gained PayloadPct (ESCORT payload progress) for the objective HUD.
//
// MsgBalance (0x46) was added WITHOUT bumping this: it is a server->client push
// an older client silently drops (unknown type -> ignored), so it neither
// half-talks nor corrupts state. Bumping would hard-reject every deployed client
// - the opposite of the goal (deploy balance without a client wave).
const ProtocolVersion = 9

const (
	MsgHello      = 0x01 // client->server: token, bbsid, handle, client version
	MsgWelcome    = 0x02 // server->client: your tank id
	MsgReject     = 0x03 // server->client: connection refused + human-readable reason
	MsgInput      = 0x10 // client->server: held-button bitfield
	MsgState      = 0x20 // server->client: full snapshot
	MsgMap        = 0x21 // server->client: full map definition (on join / map change)
	MsgLobby      = 0x22 // server->client: votable (map name + implied mode) candidates
	MsgStatusQ    = 0x23 // client->server: arena status request for the main menu
	MsgStatus     = 0x24 // server->client: arena status for the main menu
	MsgPresence   = 0x25 // client->server: Spekder-wide presence heartbeat
	MsgChat       = 0x26 // client->server: Spekder-wide chat message
	MsgPartyKick  = 0x27 // client->server: party owner boots a member by handle
	MsgMapReq     = 0x28 // client->server: lazy lobby preview - send map[i]'s geometry
	MsgMapPreview = 0x29 // server->client: a map's geometry for preview (does NOT swap arena)
	MsgPublish    = 0x30 // client->server: publish an author map to the arena repo
	MsgPubAck     = 0x31 // server->client: publish result (ok flag + message)
	MsgScore      = 0x40 // client->server: submit a finished match's score (one-shot)
	MsgScoreQ     = 0x41 // client->server: request the global high-score board
	MsgScores     = 0x42 // server->client: global high-score rows
	MsgPlayersQ   = 0x43 // client->server: request the aggregated career-stats board
	MsgPlayers    = 0x44 // server->client: aggregated per-player career rows
	MsgChangeChar = 0x45 // client->server: swap the caller's character (applied on next respawn)
	MsgBalance    = 0x46 // server->client: authoritative roster tuning (additive; older clients ignore it)
)

// ScoreSubmit is one finished-match score sent to the arena for the global board.
// BBS is the source board (set via door.ini bbsname) so players are distinguishable.
// The Won/Kills/.../Wave tail feeds the server's per-player career aggregation; it
// is an optional appended block so an older door (which omits it) still decodes.
type ScoreSubmit struct {
	Token, Mode, Name, BBS, Map string
	Score                       int
	When                        uint32
	Won                         bool
	Kills, Deaths               int
	ShotsFired, ShotsHit        int
	Wave                        int
	// Second optional tail (still additive): kill split + captures for the richer
	// global boards. An older door omits it; an older server ignores it.
	KillsHuman, KillsBot, Captures int
}

// ScoreRow is one global high-score entry returned to the door. Kills/Caps ride
// an optional trailing block so the per-mode boards can show those columns.
type ScoreRow struct {
	Mode, Name, BBS, Map string
	Score                int
	When                 uint32
	Kills, Caps          int
}

// PlayerRow is one player's aggregated career stats on the arena, keyed by
// Name@BBS. Drives the cumulative (winningest), skill-rate (K/D, accuracy) and
// survival-wave global boards. KillsHuman/KillsBot/Captures ride an optional
// trailing block (additive; older peers omit it).
type PlayerRow struct {
	Name, BBS                                                                         string
	Games, Wins, Kills, Deaths, ShotsFired, ShotsHit, BestWave, BestScore, TotalScore int
	KillsHuman, KillsBot, Captures                                                    int
	Modes                                                                             []ModeAgg // per-mode record (drives the per-mode K/D & win% boards)
}

// ModeAgg is one player's record in a single mode, for the per-mode skill boards.
type ModeAgg struct {
	Mode                       string
	Games, Wins, Kills, Deaths int
}

func EncodeScore(s ScoreSubmit) []byte {
	c := cursor{b: []byte{MsgScore}}
	c.str(s.Token)
	c.str(s.Mode)
	c.str(s.Name)
	c.str(s.BBS)
	c.str(s.Map)
	c.u32(uint32(s.Score))
	c.u32(s.When)
	// Aggregation tail (older doors stop here; the decoder treats it as optional).
	var won byte
	if s.Won {
		won = 1
	}
	c.u8(won)
	c.u16(s.Kills)
	c.u16(s.Deaths)
	c.u16(s.ShotsFired)
	c.u16(s.ShotsHit)
	c.u16(s.Wave)
	// Second additive tail: kill split + captures.
	c.u16(s.KillsHuman)
	c.u16(s.KillsBot)
	c.u16(s.Captures)
	return c.b
}

func DecodeScore(p []byte) (ScoreSubmit, bool) {
	if len(p) == 0 || p[0] != MsgScore {
		return ScoreSubmit{}, false
	}
	c := cursor{b: p, i: 1}
	s := ScoreSubmit{Token: c.rstr(), Mode: c.rstr(), Name: c.rstr(), BBS: c.rstr(), Map: c.rstr()}
	s.Score = int(c.ru32())
	s.When = c.ru32()
	// Optional aggregation tail (absent from an older door): read only if present
	// so a legacy submit still validates.
	if len(c.b)-c.i >= 1 {
		s.Won = c.ru8() != 0
	}
	if len(c.b)-c.i >= 2 {
		s.Kills = c.ru16()
	}
	if len(c.b)-c.i >= 2 {
		s.Deaths = c.ru16()
	}
	if len(c.b)-c.i >= 2 {
		s.ShotsFired = c.ru16()
	}
	if len(c.b)-c.i >= 2 {
		s.ShotsHit = c.ru16()
	}
	if len(c.b)-c.i >= 2 {
		s.Wave = c.ru16()
	}
	if len(c.b)-c.i >= 2 {
		s.KillsHuman = c.ru16()
	}
	if len(c.b)-c.i >= 2 {
		s.KillsBot = c.ru16()
	}
	if len(c.b)-c.i >= 2 {
		s.Captures = c.ru16()
	}
	return s, !c.err
}

func EncodePlayersQuery(token string) []byte {
	c := cursor{b: []byte{MsgPlayersQ}}
	c.str(token)
	return c.b
}

func DecodePlayersQuery(p []byte) (string, bool) {
	if len(p) == 0 || p[0] != MsgPlayersQ {
		return "", false
	}
	c := cursor{b: p, i: 1}
	t := c.rstr()
	return t, !c.err
}

func EncodePlayers(rows []PlayerRow) []byte {
	c := cursor{b: []byte{MsgPlayers}}
	c.u16(len(rows))
	for _, r := range rows {
		c.str(r.Name)
		c.str(r.BBS)
		c.u32(uint32(r.Games))
		c.u32(uint32(r.Wins))
		c.u32(uint32(r.Kills))
		c.u32(uint32(r.Deaths))
		c.u32(uint32(r.ShotsFired))
		c.u32(uint32(r.ShotsHit))
		c.u32(uint32(r.BestWave))
		c.u32(uint32(r.BestScore))
		c.u32(uint32(r.TotalScore))
	}
	// Additive trailing block (older doors read the fixed rows above and ignore
	// these bytes): kill split + captures, in the same row order.
	for _, r := range rows {
		c.u32(uint32(r.KillsHuman))
		c.u32(uint32(r.KillsBot))
		c.u32(uint32(r.Captures))
	}
	// Second additive trailing block: per-mode records (variable length per row).
	for _, r := range rows {
		c.u16(len(r.Modes))
		for _, m := range r.Modes {
			c.str(m.Mode)
			c.u32(uint32(m.Games))
			c.u32(uint32(m.Wins))
			c.u32(uint32(m.Kills))
			c.u32(uint32(m.Deaths))
		}
	}
	return c.b
}

func DecodePlayers(p []byte) ([]PlayerRow, bool) {
	if len(p) == 0 || p[0] != MsgPlayers {
		return nil, false
	}
	c := cursor{b: p, i: 1}
	n := c.ru16()
	out := make([]PlayerRow, 0, n)
	for i := 0; i < n; i++ {
		var r PlayerRow
		r.Name = c.rstr()
		r.BBS = c.rstr()
		r.Games = int(c.ru32())
		r.Wins = int(c.ru32())
		r.Kills = int(c.ru32())
		r.Deaths = int(c.ru32())
		r.ShotsFired = int(c.ru32())
		r.ShotsHit = int(c.ru32())
		r.BestWave = int(c.ru32())
		r.BestScore = int(c.ru32())
		r.TotalScore = int(c.ru32())
		out = append(out, r)
	}
	if c.err {
		return nil, false
	}
	// Additive trailing block from a newer server (absent from an older one).
	if len(c.b)-c.i >= n*12 {
		for i := 0; i < n; i++ {
			out[i].KillsHuman = int(c.ru32())
			out[i].KillsBot = int(c.ru32())
			out[i].Captures = int(c.ru32())
		}
		// Second block: per-mode records (only if more bytes follow).
		if len(c.b)-c.i > 0 {
			for i := 0; i < n && !c.err; i++ {
				cnt := c.ru16()
				for k := 0; k < cnt && !c.err; k++ {
					m := ModeAgg{Mode: c.rstr()}
					m.Games = int(c.ru32())
					m.Wins = int(c.ru32())
					m.Kills = int(c.ru32())
					m.Deaths = int(c.ru32())
					out[i].Modes = append(out[i].Modes, m)
				}
			}
		}
	}
	return out, true
}

func EncodeScoreQuery(token string) []byte {
	c := cursor{b: []byte{MsgScoreQ}}
	c.str(token)
	return c.b
}

func DecodeScoreQuery(p []byte) (string, bool) {
	if len(p) == 0 || p[0] != MsgScoreQ {
		return "", false
	}
	c := cursor{b: p, i: 1}
	t := c.rstr()
	return t, !c.err
}

func EncodeScores(rows []ScoreRow) []byte {
	c := cursor{b: []byte{MsgScores}}
	c.u16(len(rows))
	for _, r := range rows {
		c.str(r.Mode)
		c.str(r.Name)
		c.str(r.BBS)
		c.str(r.Map)
		c.u32(uint32(r.Score))
		c.u32(r.When)
	}
	// Additive trailing block: per-row kills + captures (older doors ignore it).
	for _, r := range rows {
		c.u16(r.Kills)
		c.u16(r.Caps)
	}
	return c.b
}

func DecodeScores(p []byte) ([]ScoreRow, bool) {
	if len(p) == 0 || p[0] != MsgScores {
		return nil, false
	}
	c := cursor{b: p, i: 1}
	n := c.ru16()
	out := make([]ScoreRow, 0, n)
	for i := 0; i < n; i++ {
		r := ScoreRow{Mode: c.rstr(), Name: c.rstr(), BBS: c.rstr(), Map: c.rstr()}
		r.Score = int(c.ru32())
		r.When = c.ru32()
		out = append(out, r)
	}
	if c.err {
		return nil, false
	}
	// Additive trailing block from a newer server (per-row kills + captures).
	if len(c.b)-c.i >= n*4 {
		for i := 0; i < n; i++ {
			out[i].Kills = int(c.ru16())
			out[i].Caps = int(c.ru16())
		}
	}
	return out, true
}

// EncodeChangeChar carries a mid-session character swap on the live game
// connection: the new character (body) + color. The server applies it to the
// caller's tank so the next respawn comes up as it.
func EncodeChangeChar(token string, color [3]float64, body int) []byte {
	c := cursor{b: []byte{MsgChangeChar}}
	c.str(token)
	c.col3(color)
	c.u8(byte(body))
	return c.b
}

func DecodeChangeChar(p []byte) (token string, color [3]float64, body int, ok bool) {
	if len(p) == 0 || p[0] != MsgChangeChar {
		return "", [3]float64{}, 0, false
	}
	c := cursor{b: p, i: 1}
	token = c.rstr()
	color = c.rcol3()
	body = int(c.ru8())
	if c.err {
		return "", [3]float64{}, 0, false
	}
	return token, color, body, true
}

const maxMsg = 1 << 20

// WriteMsg frames and writes one payload.
func WriteMsg(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadMsg reads one framed payload.
func ReadMsg(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxMsg {
		return nil, errors.New("proto: bad message length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ---- HELLO ----

func EncodeHello(token, bbsid, handle string, color [3]float64, body int, publish bool, clientProto int, clientVer, party string) []byte {
	c := cursor{b: []byte{MsgHello}}
	c.str(token)
	c.str(bbsid)
	c.str(handle)
	c.col3(color)
	c.u8(byte(body)) // render silhouette + stat row (BodyTank or a creature)
	var pub byte
	if publish { // publish-only connection: no tank is spawned
		pub = 1
	}
	c.u8(pub)
	// Version tail: the wire-compat number (for the join guard) + the human
	// release string (so the server can name a target version in a reject) + the
	// party name to team up with ("" = solo).
	c.u16(clientProto)
	c.str(clientVer)
	c.str(party)
	return c.b
}

func DecodeHello(p []byte) (token, bbsid, handle string, color [3]float64, body int, publish bool, clientProto int, clientVer, party string, ok bool) {
	if len(p) == 0 || p[0] != MsgHello {
		return "", "", "", [3]float64{}, 0, false, 0, "", "", false
	}
	c := cursor{b: p, i: 1}
	token = c.rstr()
	bbsid = c.rstr()
	handle = c.rstr()
	if c.err {
		return "", "", "", [3]float64{}, 0, false, 0, "", "", false
	}
	// color/body/publish/version are an optional tail; guard with explicit length
	// checks so a short HELLO still decodes (clientProto stays 0 for a pre-versioning
	// client, which the join guard rejects).
	if len(p)-c.i >= 3 {
		color = c.rcol3()
	}
	if len(p)-c.i >= 1 {
		body = int(c.ru8())
	}
	if len(p)-c.i >= 1 {
		publish = c.ru8() != 0
	}
	if len(p)-c.i >= 2 {
		clientProto = c.ru16()
	}
	if len(p)-c.i >= 1 {
		clientVer = c.rstr()
	}
	if len(p)-c.i >= 1 {
		party = c.rstr()
	}
	ok = true
	return
}

// ---- REJECT ----

func EncodeReject(reason string) []byte {
	c := cursor{b: []byte{MsgReject}}
	c.str(reason)
	return c.b
}

func DecodeReject(p []byte) (reason string, ok bool) {
	if len(p) == 0 || p[0] != MsgReject {
		return "", false
	}
	c := cursor{b: p, i: 1}
	reason = c.rstr()
	if c.err {
		return "", false
	}
	return reason, true
}

// ---- WELCOME ----

func EncodeWelcome(id int) []byte {
	b := []byte{MsgWelcome, 0, 0}
	binary.BigEndian.PutUint16(b[1:], uint16(id))
	return b
}

func DecodeWelcome(p []byte) (id int, ok bool) {
	if len(p) < 3 || p[0] != MsgWelcome {
		return 0, false
	}
	return int(binary.BigEndian.Uint16(p[1:])), true
}

// EncodeBalance carries the server's authoritative roster tuning (chassis +
// per-body scalar overrides) to the client. It is sent once, right after the
// welcome. The message is additive: a client that doesn't recognize MsgBalance
// drops it and keeps its compiled-in numbers, so this does NOT bump
// ProtocolVersion or force a client update - the whole point is that future
// balance tweaks deploy by restarting the server, not by re-shipping the door.
func EncodeBalance(bal gm.Balance) []byte {
	c := cursor{b: []byte{MsgBalance}}
	c.u8(byte(len(bal.Rows)))
	for _, s := range bal.Rows {
		c.u16(s.MaxHP)
		c.f32(s.Speed)
		c.f32(s.HullTurn)
		c.f32(s.AimTurn)
		c.f32(s.FireDelay)
		c.f32(s.Jump)
		c.f32(s.Scale)
		c.f32(s.AmmoMax)
		c.f32(s.AmmoRegen)
	}
	c.u8(byte(len(bal.Bodies)))
	for _, s := range bal.Bodies {
		c.f32(s.Jump)
		c.f32(s.HPRegen)
		c.f32(s.SpeedMul)
	}
	return c.b
}

func DecodeBalance(p []byte) (gm.Balance, bool) {
	if len(p) == 0 || p[0] != MsgBalance {
		return gm.Balance{}, false
	}
	c := cursor{b: p, i: 1}
	nr := int(c.ru8())
	rows := make([]gm.BodyRow, 0, nr)
	for i := 0; i < nr; i++ {
		rows = append(rows, gm.BodyRow{
			MaxHP: c.ru16(), Speed: c.rf32(), HullTurn: c.rf32(), AimTurn: c.rf32(),
			FireDelay: c.rf32(), Jump: c.rf32(), Scale: c.rf32(), AmmoMax: c.rf32(), AmmoRegen: c.rf32(),
		})
	}
	nb := int(c.ru8())
	bodies := make([]gm.BodyStats, 0, nb)
	for i := 0; i < nb; i++ {
		bodies = append(bodies, gm.BodyStats{Jump: c.rf32(), HPRegen: c.rf32(), SpeedMul: c.rf32()})
	}
	if c.err {
		return gm.Balance{}, false
	}
	return gm.Balance{Rows: rows, Bodies: bodies}, true
}

// ---- INPUT ----

func EncodeInput(in gm.Input) []byte {
	var b byte
	for i, on := range []bool{in.Throttle, in.Reverse, in.HullL, in.HullR, in.TurretL, in.TurretR, in.Fire, in.Jump} {
		if on {
			b |= 1 << uint(i)
		}
	}
	vote := byte(0xFF) // none
	if in.Vote >= 0 && in.Vote < 0xFF {
		vote = byte(in.Vote)
	}
	var b2 byte // extra flags (room beyond the 8 button bits)
	if in.Recenter {
		b2 |= 1
	}
	if in.Fire2 {
		b2 |= 2
	}
	if in.Drop {
		b2 |= 4
	}
	if in.StrafeL {
		b2 |= 8
	}
	if in.StrafeR {
		b2 |= 16
	}
	if in.Ready {
		b2 |= 32
	}
	if in.WaitVote {
		b2 |= 64
	}
	if in.HumansOnly {
		b2 |= 128
	}
	return []byte{MsgInput, b, vote, b2}
}

func DecodeInput(p []byte) (gm.Input, bool) {
	if len(p) < 2 || p[0] != MsgInput {
		return gm.Input{}, false
	}
	b := p[1]
	vote := -1
	if len(p) >= 3 && p[2] != 0xFF {
		vote = int(p[2])
	}
	var b2 byte
	if len(p) >= 4 {
		b2 = p[3]
	}
	return gm.Input{
		Throttle: b&1 != 0, Reverse: b&2 != 0, HullL: b&4 != 0, HullR: b&8 != 0,
		TurretL: b&16 != 0, TurretR: b&32 != 0, Fire: b&64 != 0, Jump: b&128 != 0,
		Recenter: b2&1 != 0, Fire2: b2&2 != 0, Drop: b2&4 != 0,
		StrafeL: b2&8 != 0, StrafeR: b2&16 != 0, Ready: b2&32 != 0,
		WaitVote:   b2&64 != 0,
		HumansOnly: b2&128 != 0,
		Vote:       vote,
	}, true
}

// ---- MAP ----

func EncodeMap(m gm.Map) []byte {
	w := &cursor{}
	w.u8(MsgMap)
	w.wmapBody(m)
	return w.b
}

// wmapBody writes a map's full definition (no message tag). Shared by MsgMap and
// the MsgMapPreview lazy-preview message so the two stay byte-compatible.
func (w *cursor) wmapBody(m gm.Map) {
	w.str(m.Name)
	w.f32(m.Size)
	w.u16(len(m.Obstacles))
	for _, b := range m.Obstacles {
		w.v3(b.Pos)
		w.v3(b.Half)
		w.col3(b.Color)
	}
	w.u16(len(m.Ramps))
	for _, r := range m.Ramps {
		w.v3(r.Pos)
		w.v3(r.Half)
		w.f32(r.H)
		w.u8(byte(r.Dir))
		w.col3(r.Color)
	}
	w.u16(len(m.Scenery))
	for _, p := range m.Scenery {
		w.str(p.Kind)
		w.v3(p.Pos)
		w.f32(p.H)
		w.col3(p.Color)
	}
	w.u16(len(m.Spawns))
	for _, s := range m.Spawns {
		w.f32(s.X)
		w.f32(s.Z)
	}
	w.u16(len(m.Entities))
	for i := range m.Entities {
		w.entity(m.Entities[i])
	}
	if m.Rules != nil { // optional per-map victory conditions
		w.u8(1)
		w.f32(float64(m.Rules.Mode))
		w.f32(m.Rules.TimeLimit)
		w.f32(float64(m.Rules.Target))
		w.f32(float64(m.Rules.Lives))
	} else {
		w.u8(0)
	}
}

// EncodeMapReq asks the server for map index i's geometry (lobby preview). The
// index matches the lobby/vote list (== gm.Maps index server-side).
func EncodeMapReq(i int) []byte {
	w := &cursor{}
	w.u8(MsgMapReq)
	w.u16(i)
	return w.b
}

func DecodeMapReq(p []byte) (int, bool) {
	if len(p) == 0 || p[0] != MsgMapReq {
		return 0, false
	}
	r := &cursor{b: p, i: 1}
	i := r.ru16()
	if r.err {
		return 0, false
	}
	return i, true
}

// EncodeMapPreview answers a MsgMapReq: the requested index plus that map's full
// geometry. Distinct from MsgMap so the client renders a preview without swapping
// its active arena.
func EncodeMapPreview(i int, m gm.Map) []byte {
	w := &cursor{}
	w.u8(MsgMapPreview)
	w.u16(i)
	w.wmapBody(m)
	return w.b
}

func DecodeMapPreview(p []byte) (int, gm.Map, bool) {
	if len(p) == 0 || p[0] != MsgMapPreview {
		return 0, gm.Map{}, false
	}
	r := &cursor{b: p, i: 1}
	i := r.ru16()
	m, ok := r.rmapBody()
	if !ok {
		return 0, gm.Map{}, false
	}
	return i, m, true
}

// trait-presence bits in the entity wire byte.
const (
	traitTurret   = 1 << 0
	traitHazard   = 1 << 1
	traitTeleport = 1 << 2
	traitDestruct = 1 << 3
	traitRespawn  = 1 << 4
	traitBounce   = 1 << 5
	traitFlag     = 1 << 6
	traitZone     = 1 << 7
)

// entity encodes an authored map entity: its shape, then a trait bitmask, then
// the params of each present trait. Runtime state (HP/dead/facing) is NOT here -
// that rides MsgState via EntitySnap; only the authored template travels in MsgMap.
func (w *cursor) entity(e gm.Entity) {
	w.str(e.Kind)
	w.v3(e.Pos)
	w.v3(e.Half)
	w.col3(e.Color)
	w.f32(e.Yaw)
	var solid byte
	if e.Solid {
		solid = 1
	}
	w.u8(solid)
	w.u8(byte(e.Weapon)) // turret weapon index
	var mask byte
	if e.Turret != nil {
		mask |= traitTurret
	}
	if e.Hazard != nil {
		mask |= traitHazard
	}
	if e.Teleport != nil {
		mask |= traitTeleport
	}
	if e.Destruct != nil {
		mask |= traitDestruct
	}
	if e.Respawn != nil {
		mask |= traitRespawn
	}
	if e.Bounce != nil {
		mask |= traitBounce
	}
	if e.Flag != nil {
		mask |= traitFlag
	}
	if e.Zone != nil {
		mask |= traitZone
	}
	w.u8(mask)
	if e.Turret != nil {
		w.f32(e.Turret.Range)
		w.f32(e.Turret.FireDelay)
		w.u16(e.Turret.Dmg)
		w.f32(e.Turret.TurnRate)
	}
	if e.Hazard != nil {
		w.f32(e.Hazard.DPS)
	}
	if e.Teleport != nil {
		w.v3(e.Teleport.Dest)
		w.f32(e.Teleport.Cooldown)
	}
	if e.Destruct != nil {
		w.u16(e.Destruct.MaxHP)
	}
	if e.Respawn != nil {
		w.f32(e.Respawn.Delay)
	}
	if e.Bounce != nil {
		w.f32(e.Bounce.Power)
	}
	if e.Flag != nil {
		w.i16(e.Flag.Team)
	}
	if e.Zone != nil {
		w.f32(e.Zone.Capture)
	}
}

func (r *cursor) rentity() gm.Entity {
	var e gm.Entity
	e.Kind = r.rstr()
	e.Pos = r.rv3()
	e.Half = r.rv3()
	e.Color = r.rcol3()
	e.Yaw = r.rf32()
	e.Solid = r.ru8() != 0
	e.Weapon = int(r.ru8())
	mask := r.ru8()
	if mask&traitTurret != 0 {
		e.Turret = &gm.TurretTrait{Range: r.rf32(), FireDelay: r.rf32(), Dmg: r.ru16(), TurnRate: r.rf32()}
	}
	if mask&traitHazard != 0 {
		e.Hazard = &gm.HazardTrait{DPS: r.rf32()}
	}
	if mask&traitTeleport != 0 {
		e.Teleport = &gm.TeleportTrait{Dest: r.rv3(), Cooldown: r.rf32()}
	}
	if mask&traitDestruct != 0 {
		e.Destruct = &gm.DestructTrait{MaxHP: r.ru16()}
	}
	if mask&traitRespawn != 0 {
		e.Respawn = &gm.RespawnTrait{Delay: r.rf32()}
	}
	if mask&traitBounce != 0 {
		e.Bounce = &gm.BounceTrait{Power: r.rf32()}
	}
	if mask&traitFlag != 0 {
		e.Flag = &gm.FlagTrait{Team: r.ri16()}
	}
	if mask&traitZone != 0 {
		e.Zone = &gm.ZoneTrait{Capture: r.rf32()}
	}
	return e
}

func DecodeMap(p []byte) (gm.Map, bool) {
	if len(p) == 0 || p[0] != MsgMap {
		return gm.Map{}, false
	}
	r := &cursor{b: p, i: 1}
	return r.rmapBody()
}

// rmapBody reads a map's full definition (cursor positioned past the tag).
func (r *cursor) rmapBody() (gm.Map, bool) {
	var m gm.Map
	m.Name = r.rstr()
	m.Size = r.rf32()
	for n := r.ru16(); n > 0; n-- {
		m.Obstacles = append(m.Obstacles, gm.Box{Pos: r.rv3(), Half: r.rv3(), Color: r.rcol3()})
	}
	for n := r.ru16(); n > 0; n-- {
		m.Ramps = append(m.Ramps, gm.Ramp{Pos: r.rv3(), Half: r.rv3(), H: r.rf32(), Dir: int(r.ru8()), Color: r.rcol3()})
	}
	for n := r.ru16(); n > 0; n-- {
		m.Scenery = append(m.Scenery, gm.Prop{Kind: r.rstr(), Pos: r.rv3(), H: r.rf32(), Color: r.rcol3()})
	}
	for n := r.ru16(); n > 0; n-- {
		m.Spawns = append(m.Spawns, gm.V3{X: r.rf32(), Z: r.rf32()})
	}
	for n := r.ru16(); n > 0; n-- {
		m.Entities = append(m.Entities, r.rentity())
	}
	if r.i < len(r.b) && r.ru8() == 1 { // optional per-map victory conditions (absent from older servers)
		m.Rules = &gm.MapRules{
			Mode: int(r.rf32()), TimeLimit: r.rf32(), Target: int(r.rf32()), Lives: int(r.rf32()),
		}
	}
	if r.err {
		return gm.Map{}, false
	}
	return m, true
}

// ---- LOBBY (votable map+mode pairings) ----

// LobbyEntry is one votable pairing: a map name and the mode it plays in. The
// vote index (Input.Vote) is the index into this list (== map index server-side).
type LobbyEntry struct {
	Name string
	Mode gm.Mode
	Hash uint32 // content fingerprint (MapHash) for the preview cache
}

// EncodeLobby sends the votable pairings (one per map in the pool). The caller
// must hold whatever lock guards gm.Maps.
func EncodeLobby() []byte {
	w := &cursor{}
	w.u8(MsgLobby)
	w.u16(len(gm.Maps))
	for i := range gm.Maps {
		name := gm.Maps[i].Name
		if c := gm.MapCapacity(gm.Maps[i]); c > 0 {
			name = name + " " + strconv.Itoa(c) + "P" // capacity tag rides the name
		}
		w.str(name)
		w.u8(byte(gm.NaturalMode(gm.Maps[i])))
		w.u32(MapHash(gm.Maps[i]))
	}
	return w.b
}

func DecodeLobby(p []byte) ([]LobbyEntry, bool) {
	if len(p) == 0 || p[0] != MsgLobby {
		return nil, false
	}
	r := &cursor{b: p, i: 1}
	n := r.ru16()
	out := make([]LobbyEntry, 0, n)
	for ; n > 0; n-- {
		out = append(out, LobbyEntry{Name: r.rstr(), Mode: gm.Mode(r.ru8()), Hash: r.ru32()})
	}
	if r.err {
		return nil, false
	}
	return out, true
}

// ---- STATUS ----

type ArenaStatus struct {
	Humans   int
	Phase    gm.Phase
	Mode     gm.Mode
	Map      string
	Presence []Presence
	Chat     []ChatMessage
	// Version tail: the arena's own release string, and the client version it
	// recommends (the released build matching this server). Empty from an older
	// server. Lets the menu nudge "update available" without contacting GitHub.
	ServerVersion string
	LatestClient  string
}

type Presence struct {
	Session string
	BBSID   string
	Handle  string
	State   string
	Detail  string
	Updated uint32
	Party   string // party name the session belongs to ("" = solo); drives team grouping
}

type ChatMessage struct {
	Seq    uint32
	Time   uint32
	BBSID  string
	Handle string
	Text   string
	Party  string // "" = global chat; otherwise the party this message is scoped to
}

func EncodeStatusQuery(token, party string) []byte {
	w := &cursor{}
	w.u8(MsgStatusQ)
	w.str(token)
	w.str(party) // querier's party, so the server can route party-scoped chat (additive)
	return w.b
}

// DecodeStatusQuery returns the token and the querier's party ("" if absent: an
// older door, or a solo caller).
func DecodeStatusQuery(p []byte) (token, party string, ok bool) {
	if len(p) == 0 || p[0] != MsgStatusQ {
		return "", "", false
	}
	r := &cursor{b: p, i: 1}
	token = r.rstr()
	if r.err {
		return "", "", false
	}
	if len(r.b)-r.i >= 1 {
		party = r.rstr()
	}
	return token, party, true
}

func EncodeStatus(st ArenaStatus) []byte {
	w := &cursor{}
	w.u8(MsgStatus)
	w.u16(st.Humans)
	w.u8(byte(st.Phase))
	w.u8(byte(st.Mode))
	w.str(st.Map)
	w.u16(len(st.Presence))
	for _, p := range st.Presence {
		w.str(p.Session)
		w.str(p.BBSID)
		w.str(p.Handle)
		w.str(p.State)
		w.str(p.Detail)
		w.u32(p.Updated)
	}
	w.u16(len(st.Chat))
	for _, m := range st.Chat {
		w.u32(m.Seq)
		w.u32(m.Time)
		w.str(m.BBSID)
		w.str(m.Handle)
		w.str(m.Text)
	}
	w.str(st.ServerVersion) // version tail (older clients ignore the extra bytes)
	w.str(st.LatestClient)
	// Party tail: one name per presence (same order), as a trailing block rather
	// than a per-record field, so an older client that stops after the version
	// tail still decodes the presence list correctly (status is not version-gated).
	w.u16(len(st.Presence))
	for _, p := range st.Presence {
		w.str(p.Party)
	}
	// Chat scope tail (parallel to Chat, by index) - same additive pattern, so an
	// older client that stops after the presence-party tail still decodes chat.
	w.u16(len(st.Chat))
	for _, m := range st.Chat {
		w.str(m.Party)
	}
	return w.b
}

func DecodeStatus(p []byte) (ArenaStatus, bool) {
	if len(p) == 0 || p[0] != MsgStatus {
		return ArenaStatus{}, false
	}
	r := &cursor{b: p, i: 1}
	st := ArenaStatus{
		Humans: int(r.ru16()),
		Phase:  gm.Phase(r.ru8()),
		Mode:   gm.Mode(r.ru8()),
		Map:    r.rstr(),
	}
	for n := r.ru16(); n > 0; n-- {
		st.Presence = append(st.Presence, Presence{
			Session: r.rstr(),
			BBSID:   r.rstr(),
			Handle:  r.rstr(),
			State:   r.rstr(),
			Detail:  r.rstr(),
			Updated: r.ru32(),
		})
	}
	for n := r.ru16(); n > 0; n-- {
		st.Chat = append(st.Chat, ChatMessage{
			Seq:    r.ru32(),
			Time:   r.ru32(),
			BBSID:  r.rstr(),
			Handle: r.rstr(),
			Text:   r.rstr(),
		})
	}
	if r.err {
		return ArenaStatus{}, false
	}
	// Optional version tail (absent from an older server): read only if present
	// so the err check above stays the real validity gate.
	if len(r.b)-r.i >= 1 {
		st.ServerVersion = r.rstr()
	}
	if len(r.b)-r.i >= 1 {
		st.LatestClient = r.rstr()
	}
	// Optional party tail (parallel to Presence, by index). Absent from an older
	// server, in which case every session reads as solo.
	if len(r.b)-r.i >= 2 {
		for i := r.ru16(); i > 0; i-- {
			name := r.rstr()
			if idx := len(st.Presence) - int(i); idx >= 0 && idx < len(st.Presence) {
				st.Presence[idx].Party = name
			}
		}
	}
	// Optional chat-scope tail (parallel to Chat, by index).
	if len(r.b)-r.i >= 2 {
		for i := r.ru16(); i > 0; i-- {
			name := r.rstr()
			if idx := len(st.Chat) - int(i); idx >= 0 && idx < len(st.Chat) {
				st.Chat[idx].Party = name
			}
		}
	}
	return st, true
}

// EncodePartyKick: a party owner boots a member. ownerHandle/party let the
// server verify ownership (the party is named after its creator's handle);
// target is the booted member's handle.
func EncodePartyKick(token, ownerHandle, party, target string) []byte {
	w := &cursor{}
	w.u8(MsgPartyKick)
	w.str(token)
	w.str(ownerHandle)
	w.str(party)
	w.str(target)
	return w.b
}

func DecodePartyKick(p []byte) (token, ownerHandle, party, target string, ok bool) {
	if len(p) == 0 || p[0] != MsgPartyKick {
		return "", "", "", "", false
	}
	r := &cursor{b: p, i: 1}
	token, ownerHandle, party, target = r.rstr(), r.rstr(), r.rstr(), r.rstr()
	if r.err {
		return "", "", "", "", false
	}
	return token, ownerHandle, party, target, true
}

func EncodePresence(token string, p Presence) []byte {
	w := &cursor{}
	w.u8(MsgPresence)
	w.str(token)
	w.str(p.Session)
	w.str(p.BBSID)
	w.str(p.Handle)
	w.str(p.State)
	w.str(p.Detail)
	w.str(p.Party) // appended: older servers ignore the extra bytes
	return w.b
}

func DecodePresence(p []byte) (token string, pr Presence, ok bool) {
	if len(p) == 0 || p[0] != MsgPresence {
		return "", Presence{}, false
	}
	r := &cursor{b: p, i: 1}
	token = r.rstr()
	pr = Presence{
		Session: r.rstr(),
		BBSID:   r.rstr(),
		Handle:  r.rstr(),
		State:   r.rstr(),
		Detail:  r.rstr(),
	}
	if r.err {
		return "", Presence{}, false
	}
	if len(r.b)-r.i >= 1 { // optional appended party (older clients omit it)
		pr.Party = r.rstr()
	}
	return token, pr, true
}

func EncodeChat(token string, m ChatMessage) []byte {
	w := &cursor{}
	w.u8(MsgChat)
	w.str(token)
	w.str(m.BBSID)
	w.str(m.Handle)
	w.str(m.Text)
	w.str(m.Party) // scope (additive: an older server ignores it -> treats as global)
	return w.b
}

func DecodeChat(p []byte) (token string, m ChatMessage, ok bool) {
	if len(p) == 0 || p[0] != MsgChat {
		return "", ChatMessage{}, false
	}
	r := &cursor{b: p, i: 1}
	token = r.rstr()
	m = ChatMessage{
		BBSID:  r.rstr(),
		Handle: r.rstr(),
		Text:   r.rstr(),
	}
	if r.err {
		return "", ChatMessage{}, false
	}
	if len(r.b)-r.i >= 1 { // optional scope tail (absent from an older door)
		m.Party = r.rstr()
	}
	return token, m, true
}

// ---- PUBLISH ----

// EncodePublish wraps a map as a publish request (same body as MsgMap).
// EncodePublish sends the author map as JSON (not the lean wire codec), so the full
// definition - event vars/logic, entity tags/watch/behaviors, rules - reaches the
// arena repo intact. The wire EncodeMap stays lean for client rendering/collision.
func EncodePublish(m gm.Map) []byte {
	data, err := gm.MapJSON(m)
	if err != nil {
		return []byte{MsgPublish}
	}
	return append([]byte{MsgPublish}, data...)
}

// DecodePublish reads a publish request (JSON body) back into a map.
func DecodePublish(p []byte) (gm.Map, bool) {
	if len(p) == 0 || p[0] != MsgPublish {
		return gm.Map{}, false
	}
	m, err := gm.ParseMapJSON(p[1:])
	if err != nil {
		return gm.Map{}, false
	}
	return m, true
}

func EncodePubAck(ok bool, msg string) []byte {
	b := []byte{MsgPubAck, 0}
	if ok {
		b[1] = 1
	}
	return appendStr(b, msg)
}

func DecodePubAck(p []byte) (ok bool, msg string, good bool) {
	if len(p) < 2 || p[0] != MsgPubAck {
		return false, "", false
	}
	ok = p[1] == 1
	msg, _, good = readStr(p, 2)
	return
}

// ---- STATE ----

func EncodeState(tick uint32, m gm.MatchSnap, tanks []gm.TankSnap, shots []gm.ShotSnap, flags []gm.FlagSnap, pickups []gm.PickupSnap, ents []gm.EntitySnap, zones []gm.ZoneSnap) []byte {
	w := &cursor{}
	w.u8(MsgState)
	w.u32(tick)
	w.u8(byte(m.Mode))
	w.u8(byte(m.Phase))
	w.f32(m.Timer)
	w.i16(m.WinnerID)
	w.u16(m.FlagsLeft)
	w.u16(m.FlagsTotal)
	w.u8(byte(min255(len(m.Votes))))
	for _, v := range m.Votes {
		w.u8(byte(min255(v)))
	}
	w.u8(byte(m.MapIdx))
	w.u16(m.Wave)
	w.u16(m.TeamScore[0])
	w.u16(m.TeamScore[1])
	w.i16(m.WinnerTeam)
	w.u8(byte(m.PayloadPct)) // ESCORT: payload progress 0-100
	w.u8(byte(min255(len(m.Kills)))) // kill feed (this tick)
	for _, k := range m.Kills {
		w.i16(k.Killer)
		w.i16(k.Victim)
		w.u8(byte(k.Cause))
	}
	w.u8(byte(min255(len(m.Events)))) // author toast messages (this tick)
	for _, ev := range m.Events {
		w.str(ev)
	}
	w.u8(byte(min255(m.Ready))) // lobby: players locked in
	w.u16(len(tanks))
	for _, t := range tanks {
		w.u16(t.ID)
		w.f32(t.Pos.X)
		w.f32(t.Pos.Y)
		w.f32(t.Pos.Z)
		w.f32(t.HullYaw)
		w.f32(t.TurretYaw)
		w.f32(t.TurretPitch)
		w.i16(t.HP)
		w.u8(c255(t.Color[0]))
		w.u8(c255(t.Color[1]))
		w.u8(c255(t.Color[2]))
		var fl byte
		if t.Dead {
			fl |= 1
		}
		if t.Bot {
			fl |= 2
		}
		if t.Shield {
			fl |= 4
		}
		if t.Hit {
			fl |= 8
		}
		if t.Carrying {
			fl |= 16
		}
		if t.Cloak {
			fl |= 32
		}
		if t.Rapid {
			fl |= 64
		}
		if t.Shell {
			fl |= 128
		}
		w.u8(fl)
		var fl2 byte // second status byte (the first is full)
		if t.Burning {
			fl2 |= 1
		}
		if t.Poisoned {
			fl2 |= 2
		}
		if t.ShieldUp {
			fl2 |= 4
		}
		if t.Bleeding {
			fl2 |= 8
		}
		if t.Healing {
			fl2 |= 16
		}
		if t.Slip {
			fl2 |= 32
		}
		w.u8(fl2)
		w.u8(c255(t.ShieldFrac)) // minotaur barrier charge 0..1 (HUD gauge + fade)
		w.u16(t.Kills)
		w.u16(t.Deaths)
		w.f32(t.RespawnIn)
		w.u8(byte(t.Body))
		w.i16(t.Lives)
		w.i16(t.Team)
		w.u16(t.HoldScore)
		w.u8(byte(t.Ammo * 255))
		w.u8(byte(t.Reload2 * 255)) // secondary recharge 0..1 (0=ready)
		w.u8(byte(t.Charges))       // remaining charge-weapon stock
		w.u8(byte(t.MaxCharges))    // charge-weapon capacity (0 = cooldown weapon)
		w.u16(t.ShotsFired)         // per-match stat tallies
		w.u16(t.ShotsHit)
		w.u16(t.Pickups)
		w.u16(t.DmgDealt)
		w.u16(t.HealDone)
		w.str(t.Name)
	}
	w.u16(len(shots))
	for _, s := range shots {
		w.f32(s.Pos.X)
		w.f32(s.Pos.Y) // Y matters now (grenade arcs, beams)
		w.f32(s.Pos.Z)
		w.u8(s.Vis)
		w.i16(s.Owner)
	}
	w.u16(len(flags))
	for _, f := range flags {
		w.v3(f.Pos)
		w.v3(f.Home)
		w.i16(f.Team)
		var ff byte
		if f.Carried {
			ff |= 1
		}
		w.u8(ff)
	}
	w.u16(len(pickups))
	for _, p := range pickups {
		w.v3(p.Pos)
		w.u8(byte(p.Kind))
		w.u8(byte(p.Weapon))
	}
	w.u16(len(ents))
	for _, e := range ents {
		w.i16(e.HP)
		var fl byte
		if e.Dead {
			fl |= 1
		}
		if e.Spawned {
			fl |= 2
		}
		w.u8(fl)
		w.f32(e.Yaw)
		w.f32(e.Pitch)
		w.v3(e.Pos)    // dynamic position (payload/moved entities)
		if e.Spawned { // runtime entity: carry its descriptor so a template-less client can draw it
			w.u8(e.Kind)
			w.v3(e.Half)
			w.col3(e.Color)
			w.i16(e.MaxHP)
		}
	}
	w.u16(len(zones))
	for _, z := range zones {
		w.v3(z.Pos)
		w.v3(z.Half)
		w.f32(z.Prog)
		w.col3(z.Color)
	}
	return w.b
}

func DecodeState(p []byte) (tick uint32, m gm.MatchSnap, tanks []gm.TankSnap, shots []gm.ShotSnap, flags []gm.FlagSnap, pickups []gm.PickupSnap, ents []gm.EntitySnap, zones []gm.ZoneSnap, ok bool) {
	if len(p) == 0 || p[0] != MsgState {
		return 0, gm.MatchSnap{}, nil, nil, nil, nil, nil, nil, false
	}
	r := &cursor{b: p, i: 1}
	tick = r.ru32()
	m = gm.MatchSnap{
		Mode:       gm.Mode(r.ru8()),
		Phase:      gm.Phase(r.ru8()),
		Timer:      r.rf32(),
		WinnerID:   r.ri16(),
		FlagsLeft:  r.ru16(),
		FlagsTotal: r.ru16(),
	}
	nv := int(r.ru8())
	m.Votes = make([]int, nv)
	for i := 0; i < nv; i++ {
		m.Votes[i] = int(r.ru8())
	}
	m.MapIdx = int(r.ru8())
	m.Wave = r.ru16()
	m.TeamScore[0] = r.ru16()
	m.TeamScore[1] = r.ru16()
	m.WinnerTeam = r.ri16()
	m.PayloadPct = int(r.ru8()) // ESCORT: payload progress 0-100
	nk := int(r.ru8())
	for i := 0; i < nk; i++ {
		m.Kills = append(m.Kills, gm.KillEvent{Killer: r.ri16(), Victim: r.ri16(), Cause: gm.KillCause(r.ru8())})
	}
	nev := int(r.ru8())
	for i := 0; i < nev; i++ {
		m.Events = append(m.Events, r.rstr())
	}
	m.Ready = int(r.ru8())
	nt := r.ru16()
	for k := 0; k < nt; k++ {
		var t gm.TankSnap
		t.ID = r.ru16()
		t.Pos.X = r.rf32()
		t.Pos.Y = r.rf32()
		t.Pos.Z = r.rf32()
		t.HullYaw = r.rf32()
		t.TurretYaw = r.rf32()
		t.TurretPitch = r.rf32()
		t.HP = r.ri16()
		cr, cg, cb := r.ru8(), r.ru8(), r.ru8()
		t.Color = [3]float64{float64(cr) / 255, float64(cg) / 255, float64(cb) / 255}
		fl := r.ru8()
		t.Dead, t.Bot, t.Shield, t.Hit = fl&1 != 0, fl&2 != 0, fl&4 != 0, fl&8 != 0
		t.Carrying = fl&16 != 0
		t.Cloak = fl&32 != 0
		t.Rapid = fl&64 != 0
		t.Shell = fl&128 != 0
		fl2 := r.ru8()
		t.Burning = fl2&1 != 0
		t.Poisoned = fl2&2 != 0
		t.ShieldUp = fl2&4 != 0
		t.Bleeding = fl2&8 != 0
		t.Healing = fl2&16 != 0
		t.Slip = fl2&32 != 0
		t.ShieldFrac = float64(r.ru8()) / 255
		t.Kills = r.ru16()
		t.Deaths = r.ru16()
		t.RespawnIn = r.rf32()
		t.Body = int(r.ru8())
		t.Lives = r.ri16()
		t.Team = r.ri16()
		t.HoldScore = r.ru16()
		t.Ammo = float64(r.ru8()) / 255
		t.Reload2 = float64(r.ru8()) / 255
		t.Charges = int(r.ru8())
		t.MaxCharges = int(r.ru8())
		t.ShotsFired = r.ru16()
		t.ShotsHit = r.ru16()
		t.Pickups = r.ru16()
		t.DmgDealt = r.ru16()
		t.HealDone = r.ru16()
		t.Name = r.rstr()
		tanks = append(tanks, t)
	}
	ns := r.ru16()
	for k := 0; k < ns; k++ {
		x, y, z := r.rf32(), r.rf32(), r.rf32()
		vis := r.ru8()
		owner := r.ri16()
		shots = append(shots, gm.ShotSnap{Pos: gm.V3{X: x, Y: y, Z: z}, Vis: vis, Owner: owner})
	}
	nf := r.ru16()
	for k := 0; k < nf; k++ {
		var f gm.FlagSnap
		f.Pos = r.rv3()
		f.Home = r.rv3()
		f.Team = r.ri16()
		ff := r.ru8()
		f.Carried = ff&1 != 0
		flags = append(flags, f)
	}
	np := r.ru16()
	for k := 0; k < np; k++ {
		var pk gm.PickupSnap
		pk.Pos = r.rv3()
		pk.Kind = int(r.ru8())
		pk.Weapon = int(r.ru8())
		pickups = append(pickups, pk)
	}
	ne := r.ru16()
	for k := 0; k < ne; k++ {
		var e gm.EntitySnap
		e.HP = r.ri16()
		fl := r.ru8()
		e.Dead = fl&1 != 0
		e.Spawned = fl&2 != 0
		e.Yaw = r.rf32()
		e.Pitch = r.rf32()
		e.Pos = r.rv3()
		if e.Spawned {
			e.Kind = r.ru8()
			e.Half = r.rv3()
			e.Color = r.rcol3()
			e.MaxHP = r.ri16()
		}
		ents = append(ents, e)
	}
	nz := r.ru16()
	for k := 0; k < nz; k++ {
		var z gm.ZoneSnap
		z.Pos = r.rv3()
		z.Half = r.rv3()
		z.Prog = r.rf32()
		z.Color = r.rcol3()
		zones = append(zones, z)
	}
	if r.err {
		return 0, gm.MatchSnap{}, nil, nil, nil, nil, nil, nil, false
	}
	return tick, m, tanks, shots, flags, pickups, ents, zones, true
}

// ---- helpers ----

func appendStr(b []byte, s string) []byte {
	if len(s) > 255 {
		s = s[:255]
	}
	b = append(b, byte(len(s)))
	return append(b, s...)
}

func readStr(p []byte, i int) (string, int, bool) {
	if i >= len(p) {
		return "", i, false
	}
	n := int(p[i])
	i++
	if i+n > len(p) {
		return "", i, false
	}
	return string(p[i : i+n]), i + n, true
}

func min255(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func c255(f float64) byte {
	v := int(f * 255)
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	return byte(v)
}

// cursor is a tiny append-on-write / bounds-checked-on-read byte buffer.
type cursor struct {
	b   []byte
	i   int
	err bool
}

func (c *cursor) u8(v byte) { c.b = append(c.b, v) }
func (c *cursor) str(s string) {
	if len(s) > 255 {
		s = s[:255]
	}
	c.u8(byte(len(s)))
	c.b = append(c.b, s...)
}
func (c *cursor) rstr() string {
	n := int(c.ru8())
	if c.err || c.i+n > len(c.b) {
		c.err = true
		return ""
	}
	s := string(c.b[c.i : c.i+n])
	c.i += n
	return s
}
func (c *cursor) col3(col [3]float64) { c.u8(c255(col[0])); c.u8(c255(col[1])); c.u8(c255(col[2])) }
func (c *cursor) rcol3() [3]float64 {
	return [3]float64{float64(c.ru8()) / 255, float64(c.ru8()) / 255, float64(c.ru8()) / 255}
}
func (c *cursor) v3(p gm.V3)   { c.f32(p.X); c.f32(p.Y); c.f32(p.Z) }
func (c *cursor) rv3() gm.V3   { return gm.V3{X: c.rf32(), Y: c.rf32(), Z: c.rf32()} }
func (c *cursor) u16(v int)    { c.b = append(c.b, byte(v>>8), byte(v)) }
func (c *cursor) i16(v int)    { c.u16(int(uint16(int16(v)))) }
func (c *cursor) u32(v uint32) { c.b = append(c.b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
func (c *cursor) f32(v float64) {
	bits := math.Float32bits(float32(v))
	c.u32(bits)
}

func (c *cursor) need(n int) bool {
	if c.err || c.i+n > len(c.b) {
		c.err = true
		return false
	}
	return true
}
func (c *cursor) ru8() byte {
	if !c.need(1) {
		return 0
	}
	v := c.b[c.i]
	c.i++
	return v
}
func (c *cursor) ru16() int {
	if !c.need(2) {
		return 0
	}
	v := int(c.b[c.i])<<8 | int(c.b[c.i+1])
	c.i += 2
	return v
}
func (c *cursor) ri16() int { return int(int16(uint16(c.ru16()))) }
func (c *cursor) ru32() uint32 {
	if !c.need(4) {
		return 0
	}
	v := uint32(c.b[c.i])<<24 | uint32(c.b[c.i+1])<<16 | uint32(c.b[c.i+2])<<8 | uint32(c.b[c.i+3])
	c.i += 4
	return v
}
func (c *cursor) rf32() float64 { return float64(math.Float32frombits(c.ru32())) }
