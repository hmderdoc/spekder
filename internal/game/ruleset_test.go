package game

import "testing"

// TestRulesetTable locks the four built-in modes' data and that Mode.String()
// reads from the table (so a mode IS its Ruleset entry).
func TestRulesetTable(t *testing.T) {
	if len(Rulesets) < 4 {
		t.Fatalf("expected >= 4 rulesets, got %d", len(Rulesets))
	}
	if Rulesets[ModeDeathmatch].Name != "DEATHMATCH" || ModeDeathmatch.String() != "DEATHMATCH" {
		t.Errorf("deathmatch name/String mismatch")
	}
	if Rulesets[ModeCTF].Teams != 2 || Rulesets[ModeCTF].Objective != ObjTeamFlags {
		t.Errorf("CTF should be 2-team team-flags: %+v", Rulesets[ModeCTF])
	}
	if Rulesets[ModeFlagRun].Objective != ObjNeutralFlags {
		t.Errorf("flag run should use neutral flags")
	}
	r := Rulesets[ModeSurvival]
	if !r.CoOp || r.Bots != BotWaves || r.Lives <= 0 {
		t.Errorf("survival should be co-op waves with lives: %+v", r)
	}
}

// TestDeathmatchFragLimitEnds: the WinFrags condition ends the match and the
// frag leader is the winner.
func TestDeathmatchFragLimitEnds(t *testing.T) {
	w, me := startDM(t, 1)
	w.Tanks[me].Kills = DMFragLimit
	w.checkEnd()
	if w.Phase != PhaseEnded {
		t.Fatalf("DM should end at the frag limit, phase=%v", w.Phase)
	}
	if w.WinnerID != me {
		t.Fatalf("winner should be the frag leader (%d), got %d", me, w.WinnerID)
	}
}

// TestTimeoutPicksLeader: when the clock expires, the match ends and the highest
// scorer wins (implicit timeout, not an explicit WinCond).
func TestTimeoutPicksLeader(t *testing.T) {
	w, me := startDM(t, 1)
	w.Tanks[me].Kills = 3 // ahead, but below the frag limit
	w.Timer = 0           // clock expired
	w.checkEnd()
	if w.Phase != PhaseEnded {
		t.Fatalf("timeout should end the match, phase=%v", w.Phase)
	}
	if w.WinnerID != me {
		t.Fatalf("timeout winner should be the leader (%d), got %d", me, w.WinnerID)
	}
}

// TestEliminationLastStanding proves a brand-new mode works almost entirely from
// its Ruleset table entry: ELIMINATION gives every tank lives (BotFill + Lives>0),
// ends when one remains, and the survivor wins.
func TestEliminationLastStanding(t *testing.T) {
	w := NewWorld(2, ModeElimination)
	me := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	if w.Phase != PhaseActive {
		t.Fatalf("expected active, got %v", w.Phase)
	}
	if w.Tanks[me].lives != 3 {
		t.Fatalf("player should start with 3 lives, got %d", w.Tanks[me].lives)
	}
	botGotLives := false
	for i := range w.Tanks {
		if w.Tanks[i].Bot && !w.Tanks[i].gone {
			if w.Tanks[i].lives == 3 {
				botGotLives = true // elimination bots get lives too (unlike survival waves)
			}
			w.Tanks[i].Dead, w.Tanks[i].lives = true, 0 // eliminate them
		}
	}
	if !botGotLives {
		t.Fatal("elimination bots (BotFill + Lives>0) should start with lives")
	}
	w.checkEnd()
	if w.Phase != PhaseEnded {
		t.Fatalf("elimination should end when one tank remains, got %v", w.Phase)
	}
	if w.WinnerID != me {
		t.Fatalf("the survivor should win, got winner %d", w.WinnerID)
	}
}

// TestEliminationRespawnGate: an out-of-lives tank stays dead; one with lives left
// still respawns. (Survival shares this path; this guards the generalization.)
func TestEliminationRespawnGate(t *testing.T) {
	w := NewWorld(1, ModeElimination)
	me := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	w.Tanks[me].Dead, w.Tanks[me].lives, w.Tanks[me].respawn = true, 0, -1
	w.respawns(1.0 / 30)
	if !w.Tanks[me].Dead {
		t.Fatal("out of lives: must not respawn")
	}
	w.Tanks[me].lives, w.Tanks[me].respawn = 1, -1
	w.respawns(1.0 / 30)
	if w.Tanks[me].Dead {
		t.Fatal("with a life left: should respawn")
	}
}

// TestAuthoredNeutralFlags: a Flag Run map with authored neutral `flag` entities
// uses exactly those, not the procedural scatter.
func TestAuthoredNeutralFlags(t *testing.T) {
	idx := len(Maps)
	Maps = append(Maps, Map{
		Name: "TEST-NEUTRAL", Size: 18, Spawns: []V3{{X: -10, Z: -10}},
		Entities: []Entity{
			{Kind: "flag", Pos: V3{X: 3, Z: 4}, Half: V3{X: 0.5, Y: 0.5, Z: 0.5}, Flag: &FlagTrait{Team: -1}},
			{Kind: "flag", Pos: V3{X: -3, Z: -4}, Half: V3{X: 0.5, Y: 0.5, Z: 0.5}, Flag: &FlagTrait{Team: -1}},
		},
	})
	w := NewWorld(0, ModeFlagRun)
	w.PinMap(idx)
	me := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	if len(w.flags) != 2 {
		t.Fatalf("expected 2 authored neutral flags, got %d (procedural fallback?)", len(w.flags))
	}
}

// TestAuthoredTeamFlags: a CTF map with authored team `flag` entities homes each
// team's flag at the authored spot.
func TestAuthoredTeamFlags(t *testing.T) {
	idx := len(Maps)
	Maps = append(Maps, Map{
		Name: "TEST-TEAM", Size: 18, Spawns: []V3{{X: -10, Z: -10}},
		Entities: []Entity{
			{Kind: "flag", Pos: V3{X: 0, Z: -12}, Half: V3{X: 0.5, Y: 0.5, Z: 0.5}, Flag: &FlagTrait{Team: 0}},
			{Kind: "flag", Pos: V3{X: 0, Z: 12}, Half: V3{X: 0.5, Y: 0.5, Z: 0.5}, Flag: &FlagTrait{Team: 1}},
		},
	})
	w := NewWorld(2, ModeCTF)
	w.PinMap(idx)
	me := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	if len(w.flags) != 2 {
		t.Fatalf("expected 2 team flags, got %d", len(w.flags))
	}
	for _, f := range w.flags {
		if f.Team == 0 && f.Home.Z != -12 {
			t.Fatalf("team 0 flag should be homed at authored z=-12, got %v", f.Home)
		}
		if f.Team == 1 && f.Home.Z != 12 {
			t.Fatalf("team 1 flag should be homed at authored z=12, got %v", f.Home)
		}
	}
}

// TestProceduralFlagFallback: a map with no flag entities still gets procedural
// Flag Run flags (every existing map keeps working).
func TestProceduralFlagFallback(t *testing.T) {
	idx := len(Maps)
	Maps = append(Maps, Map{Name: "TEST-EMPTY", Size: 18, Spawns: []V3{{X: -10, Z: -10}}})
	w := NewWorld(0, ModeFlagRun)
	w.PinMap(idx)
	me := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	if len(w.flags) != flagCount {
		t.Fatalf("no authored flags -> expected %d scattered, got %d", flagCount, len(w.flags))
	}
}

// TestFFAKotHCaptureAndScore: a lone tank standing in the hill captures it after
// the capture time, then accrues hold-points each second; reaching the limit wins.
func TestFFAKotHCaptureAndScore(t *testing.T) {
	w := NewWorld(0, ModeFFAKotH)
	me := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	if len(w.zones) == 0 {
		t.Fatal("KotH should have at least a fallback center zone")
	}
	z := &w.zones[0]
	w.Tanks[me].Pos = z.Pos // stand on the hill
	w.Tanks[me].guard = 0
	// Capture: after Cap seconds uncontested, the lone tank owns the zone.
	for i := 0; i < int(z.Cap*30)+5; i++ {
		w.stepZones(1.0 / 30)
		w.Tanks[me].Pos = w.zones[0].Pos // hold position
	}
	if w.zones[0].Owner != me {
		t.Fatalf("lone tank should capture the hill, owner=%d", w.zones[0].Owner)
	}
	// Hold: points accrue ~1/sec.
	before := w.Tanks[me].holdScore
	for i := 0; i < 60; i++ { // ~2s
		w.stepZones(1.0 / 30)
	}
	if w.Tanks[me].holdScore <= before {
		t.Fatalf("holding the hill should accrue points (%d -> %d)", before, w.Tanks[me].holdScore)
	}
	// Win condition fires at the score limit.
	w.Tanks[me].holdScore = kothScoreLimit
	w.checkEnd()
	if w.Phase != PhaseEnded || w.WinnerID != me {
		t.Fatalf("reaching the score limit should win: phase=%v winner=%d", w.Phase, w.WinnerID)
	}
}

// TestKotHContestedNoProgress: two opposing tanks in the hill = contested, so no
// capture progress and no points.
func TestKotHContestedNoProgress(t *testing.T) {
	idx := len(Maps)
	Maps = append(Maps, Map{Name: "TEST-KOTH", Size: 18, Spawns: []V3{{X: -10, Z: -10}}})
	w := NewWorld(0, ModeTeamKotH)
	w.PinMap(idx)
	a := w.AddPlayer([3]float64{}, 1, "P")
	b := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{a: {}, b: {}})
	// Force opposing teams, both on the hill.
	w.Tanks[a].Team, w.Tanks[b].Team = 0, 1
	w.Tanks[a].guard, w.Tanks[b].guard = 0, 0
	z := &w.zones[0]
	w.Tanks[a].Pos, w.Tanks[b].Pos = z.Pos, z.Pos
	for i := 0; i < 90; i++ {
		w.stepZones(1.0 / 30)
		w.Tanks[a].Pos, w.Tanks[b].Pos = w.zones[0].Pos, w.zones[0].Pos
	}
	if w.zones[0].Owner != -1 {
		t.Fatalf("contested hill should stay neutral, owner=%d", w.zones[0].Owner)
	}
	if w.teamScore[0] != 0 || w.teamScore[1] != 0 {
		t.Fatalf("contested hill should award no points, got %v", w.teamScore)
	}
}

// TestEndlessModeNoTimeout: a ruleset with TimeLimit 0 (survival) does not end on
// the clock — only its win condition can end it.
func TestEndlessModeNoTimeout(t *testing.T) {
	w := NewWorld(2, ModeSurvival)
	me := w.AddPlayer([3]float64{}, 1, "P")
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	if w.Phase != PhaseActive {
		t.Fatalf("expected active, got %v", w.Phase)
	}
	w.Timer = -5 // clock well past zero
	w.checkEnd()
	if w.Phase == PhaseEnded {
		t.Fatal("survival (TimeLimit 0) must not end on a timeout")
	}
}
