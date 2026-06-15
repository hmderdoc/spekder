package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gm "spekder/internal/game"
)

// matchResultFrom builds a result for the local player from a finished viewState.
func matchResultFrom(v viewState, player string, dur float64) matchResult {
	won := v.winnerID == v.me || (v.myTeam >= 0 && v.winnerTeam == v.myTeam)
	obj := 0
	if int(v.mode) >= 0 && int(v.mode) < len(gm.Rulesets) {
		switch gm.Rulesets[int(v.mode)].Objective {
		case gm.ObjZone:
			obj = v.self.HoldScore
		case gm.ObjTeamFlags:
			if v.myTeam >= 0 && v.myTeam < 2 {
				obj = v.teamScore[v.myTeam]
			}
		case gm.ObjNeutralFlags:
			obj = v.flagsTotal - v.flagsLeft
		}
	}
	return matchResult{
		Mode: v.mode, ModeName: v.mode.String(), MapName: v.gmap.Name,
		Vehicle: gm.Veh(v.self.Vehicle).Name, Player: player,
		Won: won, Duration: dur,
		Kills: v.self.Kills, Deaths: v.self.Deaths,
		ShotsFired: v.self.ShotsFired, ShotsHit: v.self.ShotsHit,
		Pickups: v.self.Pickups, Wave: v.wave, Objective: obj,
		DmgDealt: v.self.DmgDealt, HealDone: v.self.HealDone,
		When: time.Now().Unix(),
	}
}

// Per-BBS-user stats: cumulative totals (the player card) + per-mode high scores,
// stored next to the binary under data/spekder-<handle>.stats (JSON). See SCORING.md.

type modeStat struct {
	Games, Wins, Best int
}

type scoreEntry struct {
	Score int    `json:"score"`
	Map   string `json:"map"`
	Name  string `json:"name"`
	When  int64  `json:"when"`
	Diff  int    `json:"diff"` // difficulty tier the run was played on (gm.Difficulty)
}

type statLine struct {
	Games, Wins, Losses             int
	Kills, Deaths                   int
	ShotsFired, ShotsHit, Pickups   int
	DmgDealt, HealDone              int
	TimePlayed                      float64
	BestWave, TotalScore, BestScore int
	PerMode                         map[string]*modeStat
	PerVehicle                      map[string]int
	High                            map[string][]scoreEntry // per mode, best single runs (top N)
}

// matchResult is one finished match, the input to record().
type matchResult struct {
	Mode                       gm.Mode
	ModeName, MapName, Vehicle string
	Player                     string
	Won                        bool
	Duration                   float64
	Kills, Deaths              int
	ShotsFired, ShotsHit       int
	Pickups, Wave, Objective   int
	DmgDealt, HealDone         int
	Score                      int
	When                       int64
	Difficulty                 gm.Difficulty // solo bot tier (drives the score multiplier)
	Online                     bool          // true = live arena match (no difficulty weighting)
}

const highScoreKeep = 10

// difficultyMul weights a solo score by bot tier: an Ultimate clear is worth far
// more than the same run on Easy. Online matches have no tier (always 1.0).
func difficultyMul(d gm.Difficulty) float64 {
	switch d {
	case gm.DiffEasy:
		return 0.70
	case gm.DiffBeginner:
		return 0.85
	case gm.DiffHard:
		return 1.25
	case gm.DiffUltimate:
		return 1.50
	}
	return 1.0 // Normal
}

// scoreMatch is the points formula (transparent, mode-aware). See SCORING.md.
func scoreMatch(r matchResult) int {
	s := r.Kills*10 - r.Deaths*4 + r.Pickups*2
	s += r.DmgDealt / 25 // chip damage / assists
	s += r.HealDone / 15 // support: HP restored to allies
	if r.Won {
		s += 50
	}
	if int(r.Mode) >= 0 && int(r.Mode) < len(gm.Rulesets) {
		rs := gm.Rulesets[int(r.Mode)]
		if rs.Bots == gm.BotWaves {
			s += r.Wave * 40
		}
		switch rs.Objective {
		case gm.ObjZone:
			s += r.Objective * 2
		case gm.ObjNeutralFlags, gm.ObjTeamFlags:
			s += r.Objective * 30
		}
		// Time bonus: finishing an objective match fast (a win that isn't plain DM).
		if r.Won && rs.Objective != gm.ObjNone && r.Duration > 0 {
			if b := int((180.0 - r.Duration) * 2); b > 0 {
				s += b
			}
		}
	}
	if !r.Online { // weight solo runs by difficulty; the arena is its own board
		s = int(float64(s) * difficultyMul(r.Difficulty))
	}
	if s < 0 {
		s = 0
	}
	return s
}

// scoreBreakdown is a compact, human-readable accounting of where a match's
// points came from (only the non-zero contributors), ending with the solo
// difficulty multiplier when it isn't 1.0. Mirrors scoreMatch exactly.
func scoreBreakdown(r matchResult) string {
	var parts []string
	add := func(label string, v int) {
		if v != 0 {
			parts = append(parts, fmt.Sprintf("%s %+d", label, v))
		}
	}
	add("kills", r.Kills*10)
	add("deaths", -r.Deaths*4)
	add("pickups", r.Pickups*2)
	add("dmg", r.DmgDealt/25)
	add("heal", r.HealDone/15)
	if r.Won {
		add("win", 50)
	}
	if int(r.Mode) >= 0 && int(r.Mode) < len(gm.Rulesets) {
		rs := gm.Rulesets[int(r.Mode)]
		if rs.Bots == gm.BotWaves {
			add("wave", r.Wave*40)
		}
		switch rs.Objective {
		case gm.ObjZone:
			add("hold", r.Objective*2)
		case gm.ObjNeutralFlags, gm.ObjTeamFlags:
			add("flags", r.Objective*30)
		}
		if r.Won && rs.Objective != gm.ObjNone && r.Duration > 0 {
			if b := int((180.0 - r.Duration) * 2); b > 0 {
				add("time", b)
			}
		}
	}
	out := strings.Join(parts, "  ")
	if !r.Online {
		if m := difficultyMul(r.Difficulty); m != 1.0 {
			out += fmt.Sprintf("   x%.2f %s", m, r.Difficulty.String())
		}
	}
	return out
}

// record folds a finished match into the cumulative line + per-mode high scores.
func (st *statLine) record(r matchResult) {
	st.Games++
	if r.Won {
		st.Wins++
	} else {
		st.Losses++
	}
	st.Kills += r.Kills
	st.Deaths += r.Deaths
	st.ShotsFired += r.ShotsFired
	st.ShotsHit += r.ShotsHit
	st.Pickups += r.Pickups
	st.DmgDealt += r.DmgDealt
	st.HealDone += r.HealDone
	st.TimePlayed += r.Duration
	st.TotalScore += r.Score
	if r.Score > st.BestScore {
		st.BestScore = r.Score
	}
	if r.Wave > st.BestWave {
		st.BestWave = r.Wave
	}
	if st.PerMode == nil {
		st.PerMode = map[string]*modeStat{}
	}
	ms := st.PerMode[r.ModeName]
	if ms == nil {
		ms = &modeStat{}
		st.PerMode[r.ModeName] = ms
	}
	ms.Games++
	if r.Won {
		ms.Wins++
	}
	if r.Score > ms.Best {
		ms.Best = r.Score
	}
	if st.PerVehicle == nil {
		st.PerVehicle = map[string]int{}
	}
	if r.Vehicle != "" {
		st.PerVehicle[r.Vehicle]++
	}
	if st.High == nil {
		st.High = map[string][]scoreEntry{}
	}
	list := append(st.High[r.ModeName], scoreEntry{Score: r.Score, Map: r.MapName, Name: r.Player, When: r.When, Diff: int(r.Difficulty)})
	sort.SliceStable(list, func(i, j int) bool { return list[i].Score > list[j].Score })
	if len(list) > highScoreKeep {
		list = list[:highScoreKeep]
	}
	st.High[r.ModeName] = list
}

// favorite returns the highest-count key in a count map (ties: first seen-stable).
func favoriteMode(st *statLine) string {
	best, bn := "", -1
	for k, ms := range st.PerMode {
		if ms.Games > bn {
			bn, best = ms.Games, k
		}
	}
	return best
}
func favoriteVehicle(st *statLine) string {
	best, bn := "", -1
	for k, n := range st.PerVehicle {
		if n > bn {
			bn, best = n, k
		}
	}
	return best
}

func statsPath(dropfile string) string {
	_, handle := door32Identity(dropfile)
	dir := "data"
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Join(filepath.Dir(exe), "data")
	}
	return filepath.Join(dir, "spekder-"+sanitizeKey(handle)+".stats")
}

func loadStats(dropfile string) statLine {
	var st statLine
	if data, err := os.ReadFile(statsPath(dropfile)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func saveStats(dropfile string, st statLine) {
	p := statsPath(dropfile)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if data, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = os.WriteFile(p, data, 0o644)
	}
}

// recordResult records an already-scored finished match to the caller's stats file.
func recordResult(dropfile string, r matchResult) {
	st := loadStats(dropfile)
	st.record(r)
	saveStats(dropfile, st)
}

// scoreRank describes where a just-finished match landed in the caller's local
// record, so the end-of-match scoreboard can call out an achievement.
type scoreRank struct {
	rank         int  // 1-based place in this mode's local top list (0 = didn't make it)
	personalBest bool // beat the caller's previous best single-match score
	bestWave     bool // Survival: reached a deeper wave than ever before
}

// recordResultRanked records a finished match and reports how it placed, reading
// the caller's prior bests before folding the new result in.
func recordResultRanked(dropfile string, r matchResult) scoreRank {
	st := loadStats(dropfile)
	prevBest, prevWave := st.BestScore, st.BestWave
	st.record(r)
	saveStats(dropfile, st)
	rk := scoreRank{personalBest: r.Score > prevBest && r.Score > 0, bestWave: r.Wave > prevWave && r.Wave > 0}
	for i, e := range st.High[r.ModeName] {
		if e.When == r.When && e.Score == r.Score {
			rk.rank = i + 1
			break
		}
	}
	return rk
}
