package main

import (
	"testing"

	gm "spekder/internal/game"
)

// TestScoreAndRecord: the points formula and cumulative/high-score recording.
func TestScoreAndRecord(t *testing.T) {
	// Normal tier (multiplier 1.0) exercises the raw formula.
	r := matchResult{Mode: 0, ModeName: "DEATHMATCH", MapName: "X", Vehicle: "TANK", Player: "P",
		Won: true, Kills: 5, Deaths: 2, Pickups: 1, Difficulty: gm.DiffNormal}
	if sc := scoreMatch(r); sc != 94 { // 5*10 - 2*4 + 1*2 + 50
		t.Fatalf("score = %d, want 94", sc)
	}
	var st statLine
	r.Score = 94
	st.record(r)
	if st.Games != 1 || st.Wins != 1 || st.Kills != 5 || st.BestScore != 94 {
		t.Fatalf("cumulative wrong: %+v", st)
	}
	if st.PerMode["DEATHMATCH"].Games != 1 || st.PerVehicle["TANK"] != 1 {
		t.Fatalf("per-mode/vehicle wrong: %+v %+v", st.PerMode, st.PerVehicle)
	}
	if h := st.High["DEATHMATCH"]; len(h) != 1 || h[0].Score != 94 {
		t.Fatalf("high score not inserted: %+v", h)
	}
	r.Score = 50
	st.record(r)
	if h := st.High["DEATHMATCH"]; len(h) != 2 || h[0].Score != 94 || h[1].Score != 50 {
		t.Fatalf("high score order wrong: %+v", h)
	}
	if favoriteMode(&st) != "DEATHMATCH" || favoriteVehicle(&st) != "TANK" {
		t.Fatal("favorites wrong")
	}
}

// TestDifficultyWeighting: a solo score scales with the bot tier; online never does.
func TestDifficultyWeighting(t *testing.T) {
	base := matchResult{Mode: 0, ModeName: "DEATHMATCH", Won: true, Kills: 5, Deaths: 2, Pickups: 1} // raw 94
	cases := []struct {
		diff gm.Difficulty
		want int
	}{
		{gm.DiffEasy, 65},      // 94 * 0.70
		{gm.DiffNormal, 94},    // 94 * 1.00
		{gm.DiffHard, 117},     // 94 * 1.25
		{gm.DiffUltimate, 141}, // 94 * 1.50
	}
	for _, c := range cases {
		r := base
		r.Difficulty = c.diff
		if sc := scoreMatch(r); sc != c.want {
			t.Errorf("%s: score = %d, want %d", c.diff, sc, c.want)
		}
	}
	// Online ignores the tier even if one is set.
	r := base
	r.Online, r.Difficulty = true, gm.DiffEasy
	if sc := scoreMatch(r); sc != 94 {
		t.Errorf("online score = %d, want 94 (no weighting)", sc)
	}
}
