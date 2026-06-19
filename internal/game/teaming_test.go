package game

import "testing"

// TestFlagRunBotsTargetPlayer: in Flag Run (SoloTeam), the player is team 0 and
// every bot team 1, so a bot's nearest ENEMY is the player even when a fellow bot
// is physically closer - they gang up instead of fighting each other.
func TestFlagRunBotsTargetPlayer(t *testing.T) {
	w := &World{Mode: ModeFlagRun, demoHero: -1}
	w.Tanks = []Tank{
		{Bot: false, Pos: V3{X: 0}},  // 0: player
		{Bot: true, Pos: V3{X: 10}},  // 1: bot
		{Bot: true, Pos: V3{X: 11}},  // 2: bot, right next to bot 1
	}
	w.assignSoloTeams()
	if w.Tanks[0].Team != 0 || w.Tanks[1].Team != 1 || w.Tanks[2].Team != 1 {
		t.Fatalf("solo teams want player=0 bots=1, got %d %d %d", w.Tanks[0].Team, w.Tanks[1].Team, w.Tanks[2].Team)
	}
	if got := w.nearestEnemy(1); got != 0 {
		t.Errorf("flag-run bot 1 should target the player (0), not the adjacent bot; got %d", got)
	}
}

// TestFlagRunAllies: a map with Rules.Allies fields allied collectors on team 0
// (the first that-many bots), who CAN take neutral flags (enemy bots can't), and
// the level fails (alliesLost) once every ally is permanently down.
func TestFlagRunAllies(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "A", Rules: &MapRules{Allies: 1}}}
	w := &World{Mode: ModeFlagRun, MapIdx: 0, demoHero: -1}
	w.Tanks = []Tank{
		{Bot: true, Pos: V3{X: 0}},  // 0: first bot -> ally (team 0)
		{Bot: true, Pos: V3{X: 5}},  // 1: enemy (team 1)
		{Bot: false, Pos: V3{X: 9}}, // 2: player (team 0)
	}
	w.assignSoloTeams()
	if w.Tanks[0].Team != 0 || w.Tanks[1].Team != 1 || w.Tanks[2].Team != 0 {
		t.Fatalf("teams want ally=0 enemy=1 player=0, got %d %d %d", w.Tanks[0].Team, w.Tanks[1].Team, w.Tanks[2].Team)
	}
	if !w.allyCollector(0) || w.allyCollector(1) {
		t.Fatal("bot 0 should be an ally collector, bot 1 an enemy")
	}
	// the allied collector takes a neutral flag on its position...
	w.flags = []Flag{{Pos: V3{X: 0}}}
	w.collectFlags(0.1)
	if !w.flags[0].Taken {
		t.Error("allied collector should be able to take a neutral flag")
	}
	// ...but an enemy bot on a flag cannot.
	w.flags = []Flag{{Pos: V3{X: 5}}}
	w.collectFlags(0.1)
	if w.flags[0].Taken {
		t.Error("enemy bots must not collect flags")
	}
	// protect-fail: the level is lost only once every ally is permanently down.
	if w.alliesLost() {
		t.Fatal("ally still alive: not lost yet")
	}
	w.Tanks[0].Dead, w.Tanks[0].lives = true, 0
	if !w.alliesLost() {
		t.Error("all allies permanently down -> alliesLost")
	}
}

// TestFlagSnapsToPlatform: a flag authored on a platform deck must sit on the
// deck surface at match start (so it renders there and is collected from on top),
// not be flattened to ground level under the platform.
func TestFlagSnapsToPlatform(t *testing.T) {
	var hg Map
	found := false
	for _, m := range CampaignMaps {
		if m.Name == "HIGH GROUND" {
			hg, found = m, true
			break
		}
	}
	if !found {
		t.Skip("HIGH GROUND campaign map not present")
	}
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = append(Maps, hg)
	w := &World{Mode: ModeFlagRun, MapIdx: len(Maps) - 1}
	w.entities = hg.NewEntities()
	w.setupNeutralFlags()
	high := 0
	for i := range w.flags {
		if w.flags[i].Pos.Y > 1.0 {
			high++
		}
	}
	if high == 0 {
		t.Errorf("deck flags should snap onto the platform top; all sit at ground. flags=%+v", w.flags)
	}
}

// TestFlagRunHoldToClaim: a DwellReq flag is a mini-KoTH - not grabbed on touch,
// it fills while a collector stands on it, and an enemy on the spot pauses it.
func TestFlagRunHoldToClaim(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "H"}}
	w := &World{Mode: ModeFlagRun, MapIdx: 0, demoHero: -1}
	w.Tanks = []Tank{
		{Bot: false, Pos: V3{X: 0}},  // player (collector), team 0
		{Bot: true, Pos: V3{X: 99}},  // enemy, far away, team 1
	}
	w.assignSoloTeams()
	w.flags = []Flag{{Pos: V3{X: 0}, DwellReq: 1.0}} // hold 1s; player is standing on it
	w.collectFlags(0.4)
	if w.flags[0].Taken {
		t.Fatal("a hold flag must not be grabbed on touch")
	}
	w.collectFlags(0.4) // 0.8s held
	if w.flags[0].Taken {
		t.Fatal("a hold flag should still be claiming at 0.8s")
	}
	w.collectFlags(0.4) // 1.2s -> claimed
	if !w.flags[0].Taken {
		t.Error("a hold flag should be claimed after its dwell time")
	}
	// contest: an enemy on the spot pauses the claim entirely.
	w.flags = []Flag{{Pos: V3{X: 0}, DwellReq: 1.0}}
	w.Tanks[1].Pos = V3{X: 0}
	w.collectFlags(2.0)
	if w.flags[0].Taken {
		t.Error("an enemy contesting the spot must pause the claim")
	}
}

// TestDeathmatchStaysFFA: deathmatch is not teamed, so a bot still targets the
// nearest tank regardless of who it is (the teaming change must not leak into FFA).
func TestDeathmatchStaysFFA(t *testing.T) {
	w := &World{Mode: ModeDeathmatch}
	w.Tanks = []Tank{
		{Bot: true, Pos: V3{X: 0}},
		{Bot: true, Pos: V3{X: 1}},  // nearest to bot 0
		{Bot: false, Pos: V3{X: 9}},
	}
	if w.teamed() {
		t.Fatal("deathmatch must not be teamed")
	}
	if got := w.nearestEnemy(0); got != 1 {
		t.Errorf("deathmatch FFA: bot 0 targets the nearest tank (1); got %d", got)
	}
}
