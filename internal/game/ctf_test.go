package game

import "testing"

// startCTF makes a CTF world with one human and `bots` bots, then counts in past
// the countdown so the match is active with teams/flags set up.
func startCTF(t *testing.T, bots int) (*World, int) {
	t.Helper()
	w := NewWorld(bots, ModeCTF)
	me := w.AddPlayer([3]float64{}, 1, "P", BodyTank)
	in := map[int]Input{me: {}}
	drive(w, countdownTime+0.2, 1.0/30, in)
	if w.Phase != PhaseActive {
		t.Fatalf("expected active phase, got %v", w.Phase)
	}
	return w, me
}

func TestCTFSetup(t *testing.T) {
	w, me := startCTF(t, 4)
	if w.Tanks[me].Team != 0 {
		t.Fatalf("human should be team 0, got %d", w.Tanks[me].Team)
	}
	// Two team flags exist, one per team, each home and uncarried.
	if len(w.flags) != 2 {
		t.Fatalf("expected 2 team flags, got %d", len(w.flags))
	}
	for _, f := range w.flags {
		if f.Carrier != -1 || !f.atHome {
			t.Fatalf("flag should start home and uncarried: %+v", f)
		}
	}
	if w.ownFlag(0) == nil || w.enemyFlag(0) == nil {
		t.Fatalf("team 0 should have an own flag and an enemy flag")
	}
	// Bots are split across both teams.
	var c [2]int
	for i := range w.Tanks {
		if !w.Tanks[i].gone && w.Tanks[i].Team >= 0 {
			c[w.Tanks[i].Team]++
		}
	}
	if c[0] == 0 || c[1] == 0 {
		t.Fatalf("teams should both be populated, got %v", c)
	}
}

func TestCTFPickupDropReturn(t *testing.T) {
	w, me := startCTF(t, 2)
	t.Logf("teams: %d vs others", w.Tanks[me].Team)
	enemy := w.enemyFlag(0)
	// Teleport the human onto the enemy flag and run a CTF step to grab it.
	w.Tanks[me].Pos = enemy.Pos
	w.Tanks[me].guard = 0
	w.stepCTF(1.0 / 30)
	if w.Tanks[me].Carrying < 0 {
		t.Fatalf("human should be carrying the enemy flag after touching it")
	}
	ef := w.enemyFlag(0)
	if ef.Carrier != me || ef.atHome {
		t.Fatalf("enemy flag should be carried and not home: %+v", ef)
	}

	// Killing the carrier drops the flag with a return timer.
	w.Tanks[me].HP = 1
	w.Shots = append(w.Shots, Projectile{Pos: w.Tanks[me].Pos, owner: -1, life: 1})
	// Force a hit by stepping projectiles with a shot right on top (owner -1 = neutral).
	w.stepProjectiles(1.0 / 30)
	if !w.Tanks[me].Dead {
		t.Fatalf("carrier should be dead after the lethal hit")
	}
	if w.Tanks[me].Carrying != -1 {
		t.Fatalf("dead carrier should no longer hold a flag")
	}
	ef = w.enemyFlag(0)
	if ef.Carrier != -1 || ef.atHome || ef.dropTimer <= 0 {
		t.Fatalf("flag should be dropped with a pending return timer: %+v", ef)
	}

	// Let the drop timer expire; the flag returns home. Remove the bots first so
	// none picks up the dropped flag mid-return - this test isolates the
	// auto-return mechanic, not contested play.
	for i := range w.Tanks {
		if w.Tanks[i].Bot {
			w.Tanks[i].gone, w.Tanks[i].Dead = true, true
		}
	}
	drive(w, flagReturnTime+1, 1.0/30, map[int]Input{me: {}})
	ef = w.enemyFlag(0)
	if !ef.atHome && w.Phase == PhaseActive {
		t.Fatalf("dropped flag should have returned home, got %+v", ef)
	}
}

func TestCTFCaptureScores(t *testing.T) {
	w, me := startCTF(t, 2)
	team := w.Tanks[me].Team
	enemy := w.enemyFlag(team)
	own := w.ownFlag(team)

	// Grab the enemy flag.
	w.Tanks[me].Pos = enemy.Pos
	w.Tanks[me].guard = 0
	w.stepCTF(1.0 / 30)
	if w.Tanks[me].Carrying < 0 {
		t.Fatalf("should be carrying before capture")
	}
	// Carry it to our own base (own flag is home) and capture.
	w.Tanks[me].Pos = own.Home
	w.stepCTF(1.0 / 30)
	if w.Tanks[me].Carrying != -1 {
		t.Fatalf("flag should be captured (no longer carried)")
	}
	if w.teamScore[team] != 1 {
		t.Fatalf("team %d should have 1 capture, got %v", team, w.teamScore)
	}
	if !w.enemyFlag(team).atHome {
		t.Fatalf("captured flag should reset to its home base")
	}
}

func TestCTFWinByCaptureLimit(t *testing.T) {
	w, me := startCTF(t, 2)
	team := w.Tanks[me].Team
	w.teamScore[team] = ctfCaptureLimit
	w.checkEnd()
	if w.Phase != PhaseEnded {
		t.Fatalf("reaching the capture limit should end the match")
	}
	if w.winnerTeam != team {
		t.Fatalf("winner team should be %d, got %d", team, w.winnerTeam)
	}
	if w.WinnerID != -1 {
		t.Fatalf("CTF has no individual winner; WinnerID should be -1, got %d", w.WinnerID)
	}
}

// TestCTFSoak runs a populated CTF match for a simulated minute, exercising bot
// objective AI, pickups, drops, captures and phase transitions, asserting only
// that nothing panics and snapshots stay well-formed.
func TestCTFSoak(t *testing.T) {
	w, me := startCTF(t, 6)
	in := map[int]Input{me: {Throttle: true, Fire: true}}
	for tick := 0; tick < 60*30; tick++ {
		w.Update(1.0/30, in)
		tanks, _, flags, _ := w.Snapshot()
		for _, ts := range tanks {
			if ts.Team < -1 || ts.Team > 1 {
				t.Fatalf("tank %d has bogus team %d", ts.ID, ts.Team)
			}
		}
		if w.Mode == ModeCTF && w.Phase == PhaseActive && len(flags) != 2 {
			t.Fatalf("active CTF should always show 2 flags, got %d", len(flags))
		}
	}
}

func TestCTFNoFriendlyFire(t *testing.T) {
	w, _ := startCTF(t, 4)
	// Find two live teammates.
	a, b := -1, -1
	for i := range w.Tanks {
		if w.Tanks[i].gone || w.Tanks[i].Dead {
			continue
		}
		if a < 0 {
			a = i
		} else if w.Tanks[i].Team == w.Tanks[a].Team {
			b = i
			break
		}
	}
	if a < 0 || b < 0 {
		t.Skip("could not find two live teammates")
	}
	w.Tanks[b].guard = 0
	hpBefore := w.Tanks[b].HP
	// A shot owned by `a` sitting on teammate `b` must not deal damage.
	w.Shots = append(w.Shots, Projectile{Pos: w.Tanks[b].Pos, owner: a, life: 1})
	w.stepProjectiles(1.0 / 30)
	if w.Tanks[b].HP != hpBefore {
		t.Fatalf("friendly fire should not damage a teammate (%d -> %d)", hpBefore, w.Tanks[b].HP)
	}
}
