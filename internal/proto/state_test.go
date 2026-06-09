package proto

import (
	"testing"

	gm "spekder/internal/game"
)

// TestStateRoundTripCTF ensures the STATE wire carries the CTF/Survival fields
// (team scores, winner team, per-tank team + carry flag, and rich flag entities)
// intact across encode/decode — this is the cross-BBS contract.
func TestStateRoundTripCTF(t *testing.T) {
	m := gm.MatchSnap{
		Mode:       gm.ModeCTF,
		Phase:      gm.PhaseActive,
		Timer:      42.5,
		WinnerID:   -1,
		MapIdx:     2,
		Wave:       0,
		TeamScore:  [2]int{2, 1},
		WinnerTeam: -1,
		Kills:      []gm.KillEvent{{Killer: 1, Victim: 0, Cause: gm.CauseCannon}, {Killer: -1, Victim: 1, Cause: gm.CauseHazard}},
	}
	tanks := []gm.TankSnap{
		{ID: 0, Pos: gm.V3{X: 1, Y: 0, Z: -3}, HP: 80, Name: "DERDOK", Team: 0, Carrying: true, Lives: 0, Vehicle: 1, Shield: true, TurretPitch: 0.35, HoldScore: 7},
		{ID: 1, Pos: gm.V3{X: -2, Y: 0, Z: 4}, HP: 100, Name: "RAZOR", Team: 1, Carrying: false, Bot: true, Vehicle: 2, Body: gm.BodySpider, Cloak: true, Rapid: true},
	}
	shots := []gm.ShotSnap{{Pos: gm.V3{X: 0, Y: gm.EyeHeight, Z: 0}, Vis: gm.VisGrenade}}
	flags := []gm.FlagSnap{
		{Pos: gm.V3{X: 1, Y: 0, Z: -3}, Home: gm.V3{X: 0, Y: 0, Z: -10}, Team: 1, Carried: true},
		{Pos: gm.V3{X: 0, Y: 0, Z: 10}, Home: gm.V3{X: 0, Y: 0, Z: 10}, Team: 0, Carried: false},
	}

	pickups := []gm.PickupSnap{
		{Pos: gm.V3{X: 3, Y: 0, Z: 3}, Kind: gm.PickShield},
		{Pos: gm.V3{X: -3, Y: 0, Z: -3}, Kind: gm.PickCloak},
		{Pos: gm.V3{X: 5, Y: 0, Z: 5}, Kind: gm.PickWeapon, Weapon: 6},
	}
	ents := []gm.EntitySnap{
		{HP: 45, Dead: false, Yaw: 1.25, Pitch: -0.4},
		{HP: 0, Dead: true, Yaw: -2.0},
	}
	zones := []gm.ZoneSnap{
		{Pos: gm.V3{X: 0, Z: 0}, Half: gm.V3{X: 4, Y: 1, Z: 4}, Prog: 0.5, Color: [3]float64{0.9, 0.3, 0.3}},
	}
	buf := EncodeState(7, m, tanks, shots, flags, pickups, ents, zones)
	tick, dm, dt, ds, df, dp, de, dz, ok := DecodeState(buf)
	if !ok {
		t.Fatal("DecodeState failed")
	}
	if len(dz) != 1 || dz[0].Half.X != 4 {
		t.Fatalf("zone snap lost over wire: %+v", dz)
	}
	if d := dz[0].Prog - 0.5; d > 1e-3 || d < -1e-3 {
		t.Fatalf("zone progress lost: %+v", dz[0])
	}
	if dt[0].HoldScore != tanks[0].HoldScore {
		t.Fatalf("tank holdScore lost over wire")
	}
	if dt[0].Name != "DERDOK" || dt[1].Name != "RAZOR" {
		t.Fatalf("tank names lost over wire: %q %q", dt[0].Name, dt[1].Name)
	}
	if dt[0].Body != gm.BodyTank || dt[1].Body != gm.BodySpider {
		t.Fatalf("tank body style lost over wire: %d %d", dt[0].Body, dt[1].Body)
	}
	if len(dm.Kills) != 2 || dm.Kills[0].Killer != 1 || dm.Kills[0].Victim != 0 || dm.Kills[0].Cause != gm.CauseCannon ||
		dm.Kills[1].Killer != -1 || dm.Kills[1].Cause != gm.CauseHazard {
		t.Fatalf("kill feed lost over wire: %+v", dm.Kills)
	}
	if len(dp) != 3 || dp[2].Kind != gm.PickWeapon || dp[2].Weapon != 6 {
		t.Fatalf("weapon pickup lost over wire: %+v", dp)
	}
	if dp[0].Kind != gm.PickShield || dp[1].Kind != gm.PickCloak {
		t.Fatalf("pickups lost over wire: %+v", dp)
	}
	if dp[0].Pos.X != 3 || dp[1].Pos.Z != -3 {
		t.Fatalf("pickup positions lost: %+v", dp)
	}
	if len(de) != 2 {
		t.Fatalf("want 2 entity snaps, got %d", len(de))
	}
	if de[0].HP != 45 || de[0].Dead || de[1].HP != 0 || !de[1].Dead {
		t.Fatalf("entity snap hp/dead lost: %+v", de)
	}
	if d := de[0].Yaw - 1.25; d > 1e-3 || d < -1e-3 {
		t.Fatalf("entity snap yaw lost: %+v", de[0])
	}
	if d := de[0].Pitch - (-0.4); d > 1e-3 || d < -1e-3 {
		t.Fatalf("entity snap pitch lost: %+v", de[0])
	}
	if d := dt[0].TurretPitch - 0.35; d > 1e-3 || d < -1e-3 {
		t.Fatalf("tank turret pitch lost over wire: %+v", dt[0])
	}
	if tick != 7 {
		t.Fatalf("tick: want 7 got %d", tick)
	}
	if dm.Mode != gm.ModeCTF || dm.TeamScore != [2]int{2, 1} || dm.WinnerTeam != -1 {
		t.Fatalf("match fields wrong: %+v", dm)
	}
	if len(dt) != 2 {
		t.Fatalf("want 2 tanks, got %d", len(dt))
	}
	if dt[0].Team != 0 || !dt[0].Carrying {
		t.Fatalf("tank0 team/carry lost: %+v", dt[0])
	}
	if dt[1].Team != 1 || dt[1].Carrying {
		t.Fatalf("tank1 team/carry lost: %+v", dt[1])
	}
	if !dt[0].Shield {
		t.Fatalf("tank0 shield bit lost: %+v", dt[0])
	}
	if !dt[1].Cloak || !dt[1].Rapid {
		t.Fatalf("tank1 cloak/rapid bits lost: %+v", dt[1])
	}
	if len(ds) != 1 || ds[0].Vis != gm.VisGrenade {
		t.Fatalf("shot lost over wire (incl. vis): %+v", ds)
	}
	if len(df) != 2 {
		t.Fatalf("want 2 flags, got %d", len(df))
	}
	if df[0].Team != 1 || !df[0].Carried || df[0].Home.Z != -10 {
		t.Fatalf("flag0 fields lost: %+v", df[0])
	}
	if df[1].Team != 0 || df[1].Carried {
		t.Fatalf("flag1 fields lost: %+v", df[1])
	}
}

// TestMapRoundTripEntities ensures authored map entities and their composed
// traits survive the MsgMap wire intact (the client rebuilds geometry/behavior
// from this) — including an object that stacks turret+destruct+respawn.
func TestMapRoundTripEntities(t *testing.T) {
	m := gm.Map{
		Name: "TRAITS",
		Size: 18,
		Entities: []gm.Entity{
			{
				Kind: "turret", Pos: gm.V3{X: 0, Y: 1, Z: 0}, Half: gm.V3{X: 0.8, Y: 1, Z: 0.8},
				Color: [3]float64{0.6, 0.6, 0.65}, Yaw: 0.5, Solid: true, Weapon: 2,
				Turret:   &gm.TurretTrait{Range: 24, FireDelay: 1.4, Dmg: 18, TurnRate: 1.6},
				Destruct: &gm.DestructTrait{MaxHP: 60},
				Respawn:  &gm.RespawnTrait{Delay: 12},
			},
			{
				Kind: "hazard", Pos: gm.V3{X: 5, Y: 0, Z: 5}, Half: gm.V3{X: 2, Y: 0.1, Z: 2},
				Color:  [3]float64{0.9, 0.3, 0.1},
				Hazard: &gm.HazardTrait{DPS: 25},
			},
			{
				Kind: "teleporter", Pos: gm.V3{X: -5, Y: 0, Z: -5}, Half: gm.V3{X: 1, Y: 0.1, Z: 1},
				Teleport: &gm.TeleportTrait{Dest: gm.V3{X: 5, Y: 0, Z: 5}, Cooldown: 2},
			},
			{
				Kind: "trampoline", Pos: gm.V3{X: 8, Y: 0.1, Z: 0}, Half: gm.V3{X: 1.5, Y: 0.1, Z: 1.5},
				Bounce: &gm.BounceTrait{Power: 13},
			},
			{
				Kind: "flag", Pos: gm.V3{X: 0, Y: 0, Z: -12}, Half: gm.V3{X: 0.5, Y: 0.5, Z: 0.5},
				Flag: &gm.FlagTrait{Team: -1},
			},
			{
				Kind: "zone", Pos: gm.V3{X: 0, Y: 0, Z: 0}, Half: gm.V3{X: 4, Y: 1, Z: 4},
				Zone: &gm.ZoneTrait{Capture: 5},
			},
		},
	}
	dm, ok := DecodeMap(EncodeMap(m))
	if !ok {
		t.Fatal("DecodeMap failed")
	}
	if len(dm.Entities) != 6 {
		t.Fatalf("want 6 entities, got %d", len(dm.Entities))
	}
	if b := dm.Entities[3].Bounce; b == nil || b.Power != 13 {
		t.Fatalf("bounce trait lost: %+v", dm.Entities[3])
	}
	if f := dm.Entities[4].Flag; f == nil || f.Team != -1 {
		t.Fatalf("flag trait lost: %+v", dm.Entities[4])
	}
	if z := dm.Entities[5].Zone; z == nil || z.Capture != 5 {
		t.Fatalf("zone trait lost: %+v", dm.Entities[5])
	}
	e := dm.Entities[0]
	if e.Kind != "turret" || !e.Solid || e.Turret == nil || e.Destruct == nil || e.Respawn == nil {
		t.Fatalf("turret entity traits lost: %+v", e)
	}
	if e.Turret.Dmg != 18 || e.Destruct.MaxHP != 60 {
		t.Fatalf("turret/destruct params lost: %+v", e.Turret)
	}
	if e.Weapon != 2 {
		t.Fatalf("turret weapon lost over wire: %d want 2", e.Weapon)
	}
	if e.Hazard != nil || e.Teleport != nil {
		t.Fatalf("absent traits should stay nil: %+v", e)
	}
	if dm.Entities[1].Hazard == nil || dm.Entities[1].Hazard.DPS != 25 {
		t.Fatalf("hazard trait lost: %+v", dm.Entities[1])
	}
	tp := dm.Entities[2].Teleport
	if tp == nil || tp.Dest.X != 5 || tp.Dest.Z != 5 {
		t.Fatalf("teleport trait lost: %+v", dm.Entities[2])
	}
}
