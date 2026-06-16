package game

import "testing"

// TestBotProfileLadder locks the tier ordering: easier tiers miss more, react
// slower, track slower, and see less. HARD/ULTIMATE have unlimited sight; only
// ULTIMATE seeks pickups.
func TestBotProfileLadder(t *testing.T) {
	e, n, h, u := BotProfiles[DiffEasy], BotProfiles[DiffNormal], BotProfiles[DiffHard], BotProfiles[DiffUltimate]
	if !(e.Wobble > n.Wobble && n.Wobble > h.Wobble && h.Wobble >= u.Wobble) {
		t.Fatalf("wobble should fall Easy->Ultimate: %v %v %v %v", e.Wobble, n.Wobble, h.Wobble, u.Wobble)
	}
	if !(e.TrackRate < n.TrackRate && n.TrackRate < h.TrackRate && h.TrackRate < u.TrackRate) {
		t.Fatal("track rate should rise Easy->Ultimate")
	}
	if !(e.ReactDelay > n.ReactDelay && n.ReactDelay > h.ReactDelay && h.ReactDelay >= u.ReactDelay) {
		t.Fatal("reaction delay should fall Easy->Ultimate")
	}
	if !(e.FireDelayMul > n.FireDelayMul && n.FireDelayMul > h.FireDelayMul && h.FireDelayMul > u.FireDelayMul) {
		t.Fatal("reload multiplier should fall Easy->Ultimate")
	}
	if !(e.Sight > 0 && e.Sight < n.Sight) || h.Sight != 0 || u.Sight != 0 {
		t.Fatal("low tiers limited sight, HARD/ULTIMATE unlimited")
	}
	if u.SeekPickups != true || e.SeekPickups || h.SeekPickups {
		t.Fatal("only ULTIMATE should seek pickups")
	}
	if ProfileFor(DiffNormal).Name != "NORMAL" || ProfileFor(Difficulty(99)).Name != "NORMAL" {
		t.Fatal("ProfileFor should resolve names and clamp to NORMAL")
	}
}

// TestRollBotAIBands: per-bot jitter lands within the expected band around the
// tier center, and unlimited sight stays unlimited.
func TestRollBotAIBands(t *testing.T) {
	w := NewWorld(1, ModeDeathmatch)
	w.SetDifficulty(DiffNormal)
	w.rollBotAI(0)
	b, p := &w.Tanks[0], BotProfiles[DiffNormal]
	if b.aiTrack < p.TrackRate*0.84 || b.aiTrack > p.TrackRate*1.16 {
		t.Fatalf("track jitter out of band: %v (center %v)", b.aiTrack, p.TrackRate)
	}
	if b.aiSeek != p.SeekPickups {
		t.Fatal("aiSeek should match the profile")
	}
	w.SetDifficulty(DiffHard)
	w.rollBotAI(0)
	if w.Tanks[0].aiSight != 0 {
		t.Fatalf("HARD sight should stay unlimited (0), got %v", w.Tanks[0].aiSight)
	}
}

// TestSightLimitsAcquisition: an EASY bot can't acquire a target past its sight,
// while a HARD bot (unlimited) sees across the map.
func TestSightLimitsAcquisition(t *testing.T) {
	mk := func(d Difficulty) (*World, int, int) {
		w := NewWorld(1, ModeDeathmatch)
		w.SetDifficulty(d)
		me := w.AddPlayer([3]float64{}, 1, "P", BodyTank)
		drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
		return w, me, firstBot(w)
	}
	w, me, bot := mk(DiffEasy)
	w.Tanks[bot].Pos = V3{}
	w.Tanks[me].Pos, w.Tanks[me].cloakT = V3{X: 25}, 0 // far beyond EASY sight (~14)
	if w.nearestEnemy(bot) != -1 {
		t.Fatal("EASY bot should not acquire a far target")
	}
	w.Tanks[me].Pos = V3{X: 5} // close
	if w.nearestEnemy(bot) != me {
		t.Fatal("EASY bot should acquire a near target")
	}

	w2, me2, bot2 := mk(DiffHard)
	w2.Tanks[bot2].Pos = V3{}
	w2.Tanks[me2].Pos, w2.Tanks[me2].cloakT = V3{X: 40}, 0
	if w2.nearestEnemy(bot2) != me2 {
		t.Fatal("HARD bot has unlimited sight; should acquire across the map")
	}
}

// TestHumansHaveNoBotAI: players never get wobble/reload multipliers (only bots
// roll AI), so their fire is unaffected by difficulty.
func TestHumansHaveNoBotAI(t *testing.T) {
	w, me := startDM(t, 1)
	if w.Tanks[me].aiWobble != 0 || w.Tanks[me].aiFireMul != 0 {
		t.Fatal("human tank should have zero bot-AI params")
	}
}
