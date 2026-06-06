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
	}
	tanks := []gm.TankSnap{
		{ID: 0, Pos: gm.V3{X: 1, Y: 0, Z: -3}, HP: 80, Team: 0, Carrying: true, Lives: 0, Vehicle: 1, Shield: true},
		{ID: 1, Pos: gm.V3{X: -2, Y: 0, Z: 4}, HP: 100, Team: 1, Carrying: false, Bot: true, Vehicle: 2, Cloak: true, Rapid: true},
	}
	shots := []gm.V3{{X: 0, Y: gm.EyeHeight, Z: 0}}
	flags := []gm.FlagSnap{
		{Pos: gm.V3{X: 1, Y: 0, Z: -3}, Home: gm.V3{X: 0, Y: 0, Z: -10}, Team: 1, Carried: true},
		{Pos: gm.V3{X: 0, Y: 0, Z: 10}, Home: gm.V3{X: 0, Y: 0, Z: 10}, Team: 0, Carried: false},
	}

	pickups := []gm.PickupSnap{
		{Pos: gm.V3{X: 3, Y: 0, Z: 3}, Kind: gm.PickShield},
		{Pos: gm.V3{X: -3, Y: 0, Z: -3}, Kind: gm.PickCloak},
	}
	buf := EncodeState(7, m, tanks, shots, flags, pickups)
	tick, dm, dt, ds, df, dp, ok := DecodeState(buf)
	if !ok {
		t.Fatal("DecodeState failed")
	}
	if len(dp) != 2 || dp[0].Kind != gm.PickShield || dp[1].Kind != gm.PickCloak {
		t.Fatalf("pickups lost over wire: %+v", dp)
	}
	if dp[0].Pos.X != 3 || dp[1].Pos.Z != -3 {
		t.Fatalf("pickup positions lost: %+v", dp)
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
	if len(ds) != 1 {
		t.Fatalf("want 1 shot, got %d", len(ds))
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
