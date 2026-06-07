// Package proto is the Spekder wire protocol: length-prefixed binary messages
// carrying game STATE and INPUT (never pixels). Framing: uint32 big-endian
// length + payload; payload[0] is the message type.
package proto

import (
	"encoding/binary"
	"errors"
	"io"
	"math"

	gm "spekder/internal/game"
)

const (
	MsgHello   = 0x01 // client->server: token, bbsid, handle
	MsgWelcome = 0x02 // server->client: your tank id
	MsgInput   = 0x10 // client->server: held-button bitfield
	MsgState   = 0x20 // server->client: full snapshot
	MsgMap     = 0x21 // server->client: full map definition (on join / map change)
)

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

func EncodeHello(token, bbsid, handle string, vehicle int) []byte {
	b := []byte{MsgHello}
	b = appendStr(b, token)
	b = appendStr(b, bbsid)
	b = appendStr(b, handle)
	return append(b, byte(vehicle))
}

func DecodeHello(p []byte) (token, bbsid, handle string, vehicle int, ok bool) {
	if len(p) == 0 || p[0] != MsgHello {
		return "", "", "", 0, false
	}
	i := 1
	if token, i, ok = readStr(p, i); !ok {
		return
	}
	if bbsid, i, ok = readStr(p, i); !ok {
		return
	}
	handle, i, ok = readStr(p, i)
	if !ok {
		return
	}
	if i < len(p) {
		vehicle = int(p[i])
	}
	return
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
		Recenter: b2&1 != 0,
		Vote:     vote,
	}, true
}

// ---- MAP ----

func EncodeMap(m gm.Map) []byte {
	w := &cursor{}
	w.u8(MsgMap)
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
	return w.b
}

// trait-presence bits in the entity wire byte.
const (
	traitTurret   = 1 << 0
	traitHazard   = 1 << 1
	traitTeleport = 1 << 2
	traitDestruct = 1 << 3
	traitRespawn  = 1 << 4
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
}

func (r *cursor) rentity() gm.Entity {
	var e gm.Entity
	e.Kind = r.rstr()
	e.Pos = r.rv3()
	e.Half = r.rv3()
	e.Color = r.rcol3()
	e.Yaw = r.rf32()
	e.Solid = r.ru8() != 0
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
	return e
}

func DecodeMap(p []byte) (gm.Map, bool) {
	if len(p) == 0 || p[0] != MsgMap {
		return gm.Map{}, false
	}
	r := &cursor{b: p, i: 1}
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
	if r.err {
		return gm.Map{}, false
	}
	return m, true
}

// ---- STATE ----

func EncodeState(tick uint32, m gm.MatchSnap, tanks []gm.TankSnap, shots []gm.V3, flags []gm.FlagSnap, pickups []gm.PickupSnap, ents []gm.EntitySnap) []byte {
	w := &cursor{}
	w.u8(MsgState)
	w.u32(tick)
	w.u8(byte(m.Mode))
	w.u8(byte(m.Phase))
	w.f32(m.Timer)
	w.i16(m.WinnerID)
	w.u16(m.FlagsLeft)
	w.u16(m.FlagsTotal)
	for _, v := range m.Votes {
		w.u8(byte(min255(v)))
	}
	w.u8(byte(m.MapIdx))
	w.u16(m.Wave)
	w.u16(m.TeamScore[0])
	w.u16(m.TeamScore[1])
	w.i16(m.WinnerTeam)
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
		w.u8(fl)
		w.u16(t.Kills)
		w.u16(t.Deaths)
		w.f32(t.RespawnIn)
		w.u8(byte(t.Vehicle))
		w.i16(t.Lives)
		w.i16(t.Team)
	}
	w.u16(len(shots))
	for _, s := range shots {
		w.f32(s.X)
		w.f32(s.Z)
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
	}
	w.u16(len(ents))
	for _, e := range ents {
		w.i16(e.HP)
		var fl byte
		if e.Dead {
			fl |= 1
		}
		w.u8(fl)
		w.f32(e.Yaw)
		w.f32(e.Pitch)
	}
	return w.b
}

func DecodeState(p []byte) (tick uint32, m gm.MatchSnap, tanks []gm.TankSnap, shots []gm.V3, flags []gm.FlagSnap, pickups []gm.PickupSnap, ents []gm.EntitySnap, ok bool) {
	if len(p) == 0 || p[0] != MsgState {
		return 0, gm.MatchSnap{}, nil, nil, nil, nil, nil, false
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
	for i := 0; i < 4; i++ {
		m.Votes[i] = int(r.ru8())
	}
	m.MapIdx = int(r.ru8())
	m.Wave = r.ru16()
	m.TeamScore[0] = r.ru16()
	m.TeamScore[1] = r.ru16()
	m.WinnerTeam = r.ri16()
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
		t.Kills = r.ru16()
		t.Deaths = r.ru16()
		t.RespawnIn = r.rf32()
		t.Vehicle = int(r.ru8())
		t.Lives = r.ri16()
		t.Team = r.ri16()
		tanks = append(tanks, t)
	}
	ns := r.ru16()
	for k := 0; k < ns; k++ {
		x := r.rf32()
		z := r.rf32()
		shots = append(shots, gm.V3{X: x, Y: gm.EyeHeight, Z: z})
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
		pickups = append(pickups, pk)
	}
	ne := r.ru16()
	for k := 0; k < ne; k++ {
		var e gm.EntitySnap
		e.HP = r.ri16()
		e.Dead = r.ru8()&1 != 0
		e.Yaw = r.rf32()
		e.Pitch = r.rf32()
		ents = append(ents, e)
	}
	if r.err {
		return 0, gm.MatchSnap{}, nil, nil, nil, nil, nil, false
	}
	return tick, m, tanks, shots, flags, pickups, ents, true
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
