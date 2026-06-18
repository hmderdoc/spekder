// balancesim runs many headless, bot-only matches and aggregates per-character
// performance, so roster balance can be judged from data instead of vibes. The
// sim engine (internal/game) is server-authoritative and deterministic, so this
// reuses it verbatim: NewWorld -> SkipCountdown -> SetBotBody (lock the roster)
// -> Update() in a tight loop with no frame sleep, then read Snapshot()/Match().
//
// Three measurements, each isolating a different signal:
//
//	duel  1v1 deathmatch round-robin over every combat-character pair. The
//	      cleanest isolation: who beats whom, head to head, map bias averaged
//	      out across seeds. Produces a win-rate ranking (and optional matrix).
//
//	ffa   one free-for-all deathmatch with every combat character present at
//	      once. Corroborates the duel ranking in a crowded fight and surfaces
//	      multiplayer-only effects (focus fire, splash, pickups).
//
//	ctf   A/B team test for the support characters (butterfly, stag), which a
//	      duel/FFA under-rates to ~zero because their whole kit needs allies.
//	      Team 0 fields 3 fighters + the healer; team 1 fields 4 fighters. If
//	      the healer is worth more than a body, team 0 should out-trade/win.
//
// Bot-AI caveat (see Step 1a audit): bots DO exercise flight, leap, melee
// charge, shells, barriers, secondary weapons and healing - but healers only
// heal in team modes, so their verdict comes from ctf, never duel/ffa.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	gm "spekder/internal/game"
)

// char is one roster entry the player can pick. role gates which measurements
// apply: combat characters fight in duel/ffa; healers are judged only in ctf.
type char struct {
	name string
	body int
	role string // "combat" or "healer"
}

// roster mirrors the door's playerBodies (main.go) plus TANK (the BodyTank
// chassis pick). CUSTOM is intentionally absent - it is being removed from the
// game, so it is not part of roster balance.
var roster = []char{
	{"TANK", gm.BodyTank, "combat"},
	{"HUMANOID", gm.BodyHumanoid, "combat"},
	{"GORILLA", gm.BodyGorilla, "combat"},
	{"MANTIS", gm.BodyMantis, "combat"},
	{"T-REX", gm.BodyTrex, "combat"},
	{"SCORPION", gm.BodyScorpion, "combat"},
	{"SERPENT", gm.BodySerpent, "combat"},
	{"INSECT", gm.BodyInsect, "combat"},
	{"CRAB", gm.BodyCrab, "combat"},
	{"OCTOPOD", gm.BodyOctopod, "combat"},
	{"TIGER", gm.BodyQuad, "combat"},
	{"TURTLE", gm.BodyTurtle, "combat"},
	{"ELEPHANT", gm.BodyElephant, "combat"},
	{"FALCON", gm.BodyFalcon, "combat"},
	{"MINOTAUR", gm.BodyMinotaur, "combat"},
	{"BUTTERFLY", gm.BodyButterfly, "healer"},
	{"STAG", gm.BodyStag, "healer"},
}

func combatChars() []char {
	var out []char
	for _, c := range roster {
		if c.role == "combat" {
			out = append(out, c)
		}
	}
	return out
}

func healerChars() []char {
	var out []char
	for _, c := range roster {
		if c.role == "healer" {
			out = append(out, c)
		}
	}
	return out
}

// --- match runner -----------------------------------------------------------

const dt = 1.0 / 30.0 // fixed sim step; no real time elapses (tight loop)

type matchOut struct {
	tanks      []gm.TankSnap
	winnerID   int
	winnerTeam int
	ended      bool // reached PhaseEnded (vs. hit the time cap)
}

// runMatch plays one headless match: bodies[i] becomes bot i's locked character.
// seed makes the whole match (map pick, spawns, AI wobble) reproducible.
func runMatch(seed int64, mode gm.Mode, bodies []int, diff gm.Difficulty, maxSec float64) matchOut {
	rand.Seed(seed)
	w := gm.NewWorld(len(bodies), mode)
	w.SetDifficulty(diff)
	w.SkipCountdown() // -> startMatch: assigns teams (by index) and rolls bot looks
	for i, b := range bodies {
		w.SetBotBody(i, b) // override the rolled look; respawns() preserves it
	}
	maxTicks := int(maxSec/dt) + 1
	for t := 0; t < maxTicks; t++ {
		if w.Match().Phase == gm.PhaseEnded {
			break
		}
		w.Update(dt, nil)
	}
	m := w.Match()
	tanks, _, _, _ := w.Snapshot()
	return matchOut{tanks: tanks, winnerID: m.WinnerID, winnerTeam: m.WinnerTeam, ended: m.Phase == gm.PhaseEnded}
}

// leaders returns the tank IDs tied for the most kills. The engine only fills
// MatchSnap.WinnerID once a frag target or the time limit is reached; a match
// that hits our shorter sim cap mid-fight reports no winner, so we rank by final
// kills directly - the same thing computeWinner does, and the metric we want.
func leaders(tanks []gm.TankSnap) []int {
	best := -1
	for _, t := range tanks {
		if t.Kills > best {
			best = t.Kills
		}
	}
	var ids []int
	for _, t := range tanks {
		if t.Kills == best {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// --- aggregation ------------------------------------------------------------

type stat struct {
	games                int
	wins                 int // top-fragger (FFA) / duel winner / KOTH king
	draws                int
	kills, deaths        int
	dmg, heal            int
	hold                 int // KOTH hold-points accrued (0 in other modes)
	shotsFired, shotsHit int
}

func (s *stat) add(t gm.TankSnap, won, draw bool) {
	s.games++
	if won {
		s.wins++
	}
	if draw {
		s.draws++
	}
	s.kills += t.Kills
	s.deaths += t.Deaths
	s.dmg += t.DmgDealt
	s.heal += t.HealDone
	s.hold += t.HoldScore
	s.shotsFired += t.ShotsFired
	s.shotsHit += t.ShotsHit
}

func (s stat) holdPerGame() float64 {
	if s.games == 0 {
		return 0
	}
	return float64(s.hold) / float64(s.games)
}

func (s stat) winRate() float64 {
	if s.games == 0 {
		return 0
	}
	return float64(s.wins) / float64(s.games)
}
func (s stat) kd() float64 {
	if s.deaths == 0 {
		return float64(s.kills)
	}
	return float64(s.kills) / float64(s.deaths)
}
func (s stat) acc() float64 {
	if s.shotsFired == 0 {
		return 0
	}
	return float64(s.shotsHit) / float64(s.shotsFired)
}

// --- duel: 1v1 round-robin --------------------------------------------------

func runDuels(seeds int, diff gm.Difficulty, maxSec float64, base int64, showMatrix bool) {
	cc := combatChars()
	n := len(cc)
	by := map[int]*stat{}
	for _, c := range cc {
		by[c.body] = &stat{}
	}
	// win[a][b] = matches body a beat body b (decisive only).
	win := map[int]map[int]int{}
	played := map[int]map[int]int{}
	for _, a := range cc {
		win[a.body] = map[int]int{}
		played[a.body] = map[int]int{}
	}
	seed := base
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			a, b := cc[i].body, cc[j].body
			for r := 0; r < seeds; r++ {
				seed++
				out := runMatch(seed, gm.ModeDeathmatch, []int{a, b}, diff, maxSec)
				idBody := map[int]int{}
				for _, t := range out.tanks {
					idBody[t.ID] = t.Body
				}
				ld := leaders(out.tanks)
				draw := len(ld) != 1
				winID, winBody := -1, -1
				if !draw {
					winID = ld[0]
					winBody = idBody[winID]
				}
				for _, t := range out.tanks {
					by[t.Body].add(t, !draw && t.ID == winID, draw)
				}
				played[a][b]++
				played[b][a]++
				if winBody == a {
					win[a][b]++
				} else if winBody == b {
					win[b][a]++
				}
			}
		}
	}
	fmt.Printf("\n=== DUEL  (1v1 deathmatch, %d combat chars, %d seeds/pair, %d matches, diff=%s) ===\n",
		n, seeds, n*(n-1)/2*seeds, diff)
	printRanking(cc, by, "duel win-rate")
	if showMatrix {
		printMatrix(cc, win, played)
	}
}

// --- ffa: one big deathmatch ------------------------------------------------

func runFFA(seeds int, diff gm.Difficulty, maxSec float64, base int64) {
	cc := combatChars()
	bodies := make([]int, len(cc))
	for i, c := range cc {
		bodies[i] = c.body
	}
	by := map[int]*stat{}
	for _, c := range cc {
		by[c.body] = &stat{}
	}
	seed := base
	for r := 0; r < seeds; r++ {
		seed++
		out := runMatch(seed, gm.ModeDeathmatch, bodies, diff, maxSec)
		ld := leaders(out.tanks)
		winID := -1
		if len(ld) == 1 {
			winID = ld[0]
		}
		for _, t := range out.tanks {
			by[t.Body].add(t, winID >= 0 && t.ID == winID, false)
		}
	}
	fmt.Printf("\n=== FFA  (free-for-all deathmatch, all %d combat chars, %d matches, diff=%s) ===\n",
		len(cc), seeds, diff)
	fmt.Printf("    (win = top fragger; even baseline = %.1f%%)\n", 100/float64(len(cc)))
	printRanking(cc, by, "ffa win-share")
}

// --- koth: who holds the hill ----------------------------------------------

// runKoth plays FFA King-of-the-Hill matches with all combat chars and ranks them
// by hold-points accrued (the objective metric duel/FFA structurally miss). The
// "king" of each match (most hold-points, decisive only) feeds a familiar win-share
// column; deaths/g reads survival-on-point. This is the arena TURTLE is built for.
func runKoth(seeds int, diff gm.Difficulty, maxSec float64, base int64) {
	cc := combatChars()
	bodies := make([]int, len(cc))
	for i, c := range cc {
		bodies[i] = c.body
	}
	by := map[int]*stat{}
	for _, c := range cc {
		by[c.body] = &stat{}
	}
	seed := base
	for r := 0; r < seeds; r++ {
		seed++
		out := runMatch(seed, gm.ModeFFAKotH, bodies, diff, maxSec)
		// king = the sole tank with the most hold-points this match
		kingID, best, tie := -1, -1, false
		for _, t := range out.tanks {
			switch {
			case t.HoldScore > best:
				kingID, best, tie = t.ID, t.HoldScore, false
			case t.HoldScore == best:
				tie = true
			}
		}
		if best <= 0 || tie {
			kingID = -1 // nobody actually held it / a tie: no decisive king
		}
		for _, t := range out.tanks {
			by[t.Body].add(t, kingID >= 0 && t.ID == kingID, false)
		}
	}
	fmt.Printf("\n=== KOTH  (FFA King of the Hill, all %d combat chars, %d matches, diff=%s) ===\n",
		len(cc), seeds, diff)
	fmt.Printf("    (ranked by hold-points/game - the objective metric; win-share = match 'king')\n")
	printKothRanking(cc, by)
}

func printKothRanking(cc []char, by map[int]*stat) {
	sort.Slice(cc, func(a, b int) bool {
		return by[cc[a].body].holdPerGame() > by[cc[b].body].holdPerGame()
	})
	var sum, sumsq float64
	for _, c := range cc {
		h := by[c.body].holdPerGame()
		sum += h
		sumsq += h * h
	}
	nF := float64(len(cc))
	mean := sum / nF
	std := math.Sqrt(math.Max(0, sumsq/nF-mean*mean))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "RANK\tCHARACTER\tHOLD/G\tWIN-SHARE\tK/D\tKILLS/G\tDEATHS/G\tACC\tFLAG\n")
	for i, c := range cc {
		s := by[c.body]
		g := float64(max(s.games, 1))
		flag := ""
		if std > 0 {
			switch {
			case s.holdPerGame() > mean+1.5*std:
				flag = "TOP"
			case s.holdPerGame() < mean-1.5*std:
				flag = "low"
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%.1f\t%.1f%%\t%.2f\t%.1f\t%.1f\t%.0f%%\t%s\n",
			i+1, c.name, s.holdPerGame(), 100*s.winRate(), s.kd(),
			float64(s.kills)/g, float64(s.deaths)/g, 100*s.acc(), flag)
	}
	w.Flush()
	fmt.Printf("    mean hold/g = %.1f, stddev = %.1f  (TOP/low = >1.5 stddev from mean)\n", mean, std)
}

// --- ctf: A/B healer value --------------------------------------------------

func runCTF(seeds int, diff gm.Difficulty, maxSec float64, base int64) {
	healers := healerChars()
	const fighter = gm.BodyTank // TANK is the neutral baseline fighter
	fmt.Printf("\n=== CTF  (A/B healer value: team0 = 3 TANK + healer  vs  team1 = 4 TANK) ===\n")
	fmt.Printf("    (team0 traded a 4th fighter for the healer; >50%% kill-win or +kill-diff = the healer earns its slot)\n")
	fmt.Printf("    (win decided by team kills/match - bot flag captures are too sparse to be decisive)\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "HEALER\tT0 WINS\tT1 WINS\tTIES\tT0 KILL-WIN%\tAVG KILL DIFF (T0-T1)\tHEAL/GAME")
	for _, h := range healers {
		// index order: even -> team0, odd -> team1 (assignTeams fills alternately)
		bodies := []int{fighter, fighter, fighter, fighter, fighter, fighter, h.body, fighter}
		var t0w, t1w, ties, t0kills, t1kills, healSum, games int
		seed := base
		for r := 0; r < seeds; r++ {
			seed++
			out := runMatch(seed, gm.ModeCTF, bodies, diff, maxSec)
			games++
			var mk0, mk1 int
			for _, t := range out.tanks {
				if t.Team == 0 {
					mk0 += t.Kills
				} else if t.Team == 1 {
					mk1 += t.Kills
				}
				if t.Body == h.body {
					healSum += t.HealDone
				}
			}
			t0kills += mk0
			t1kills += mk1
			switch {
			case mk0 > mk1:
				t0w++
			case mk1 > mk0:
				t1w++
			default:
				ties++
			}
		}
		decisive := t0w + t1w
		winPct := 0.0
		if decisive > 0 {
			winPct = 100 * float64(t0w) / float64(decisive)
		}
		killDiff := float64(t0kills-t1kills) / float64(max(games, 1))
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%.0f%%\t%+.1f\t%.0f\n",
			h.name, t0w, t1w, ties, winPct, killDiff, float64(healSum)/float64(max(games, 1)))
	}
	w.Flush()
}

// --- reporting --------------------------------------------------------------

func printRanking(cc []char, by map[int]*stat, label string) {
	sort.Slice(cc, func(a, b int) bool {
		return by[cc[a].body].winRate() > by[cc[b].body].winRate()
	})
	// mean / stddev of win-rate to flag outliers
	var sum, sumsq float64
	for _, c := range cc {
		wr := by[c.body].winRate()
		sum += wr
		sumsq += wr * wr
	}
	nF := float64(len(cc))
	mean := sum / nF
	std := math.Sqrt(math.Max(0, sumsq/nF-mean*mean))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "RANK\tCHARACTER\t%s\tK/D\tKILLS/G\tDEATHS/G\tACC\tDMG/G\tFLAG\n", strings.ToUpper(label))
	for i, c := range cc {
		s := by[c.body]
		g := float64(max(s.games, 1))
		flag := ""
		if std > 0 {
			switch {
			case s.winRate() > mean+1.5*std:
				flag = "OP?"
			case s.winRate() < mean-1.5*std:
				flag = "UP?"
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%.1f%%\t%.2f\t%.1f\t%.1f\t%.0f%%\t%.0f\t%s\n",
			i+1, c.name, 100*s.winRate(), s.kd(),
			float64(s.kills)/g, float64(s.deaths)/g, 100*s.acc(), float64(s.dmg)/g, flag)
	}
	w.Flush()
	fmt.Printf("    mean %s = %.1f%%, stddev = %.1f%%  (OP?/UP? = >1.5 stddev from mean)\n",
		label, 100*mean, 100*std)
}

func printMatrix(cc []char, win, played map[int]map[int]int) {
	fmt.Println("\n    duel win-rate matrix (row beats column, %):")
	w := tabwriter.NewWriter(os.Stdout, 0, 1, 1, ' ', 0)
	hdr := "          "
	for _, c := range cc {
		hdr += "\t" + short(c.name)
	}
	fmt.Fprintln(w, hdr)
	for _, a := range cc {
		row := fmt.Sprintf("%-10s", a.name)
		for _, b := range cc {
			if a.body == b.body {
				row += "\t-"
				continue
			}
			p := played[a.body][b.body]
			if p == 0 {
				row += "\t."
				continue
			}
			row += fmt.Sprintf("\t%.0f", 100*float64(win[a.body][b.body])/float64(p))
		}
		fmt.Fprintln(w, row)
	}
	w.Flush()
}

func short(s string) string {
	if len(s) > 4 {
		return s[:4]
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func main() {
	duelSeeds := flag.Int("duelseeds", 24, "seeds per duel pair")
	ffaSeeds := flag.Int("ffaseeds", 50, "FFA matches")
	ctfSeeds := flag.Int("ctfseeds", 50, "CTF matches per healer")
	kothSeeds := flag.Int("kothseeds", 0, "FFA KOTH matches (0 = skip; measures hold-points)")
	diffName := flag.String("diff", "HARD", "bot difficulty (EASY..ULTIMATE)")
	maxSec := flag.Float64("maxsec", 120, "max simulated seconds per match")
	baseSeed := flag.Int64("seed", 1000, "base RNG seed (reproducibility)")
	skip := flag.String("skip", "", "comma-list of measurements to skip: duel,ffa,ctf,koth")
	matrix := flag.Bool("matrix", false, "print the full duel win-rate matrix")
	flag.Parse()

	diff, ok := gm.ParseDifficulty(*diffName)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown difficulty %q\n", *diffName)
		os.Exit(1)
	}
	skipped := map[string]bool{}
	for _, s := range strings.Split(*skip, ",") {
		skipped[strings.TrimSpace(strings.ToLower(s))] = true
	}

	fmt.Printf("spekder balance sim  (diff=%s, maxsec=%.0f, base seed=%d)\n", diff, *maxSec, *baseSeed)
	if !skipped["duel"] {
		runDuels(*duelSeeds, diff, *maxSec, *baseSeed, *matrix)
	}
	if !skipped["ffa"] {
		runFFA(*ffaSeeds, diff, *maxSec, *baseSeed+500000)
	}
	if !skipped["ctf"] {
		runCTF(*ctfSeeds, diff, *maxSec, *baseSeed+900000)
	}
	if *kothSeeds > 0 && !skipped["koth"] {
		runKoth(*kothSeeds, diff, *maxSec, *baseSeed+1300000)
	}
}
