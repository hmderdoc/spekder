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

// TestEndlessModeNoTimeout: a ruleset with TimeLimit 0 (survival) does not end on
// the clock — only its win condition can end it.
func TestEndlessModeNoTimeout(t *testing.T) {
	w := NewWorld(2, ModeSurvival)
	me := w.AddPlayer([3]float64{}, 1)
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
