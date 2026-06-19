package main

import (
	"bufio"
	"fmt"
	"math"
	"time"

	gm "spekder/internal/game"
)

// FLAG RUN tiers. TIER 0 is forgiving onboarding - one forced character + one
// tooltip per level, retry on a miss with no run reset; TIER 1+ are the
// competitive life-pool gauntlets (runCampaign). CampaignMaps are grouped by
// Map.Tier; within a tier they play in load (lexical filename) order. The player
// defaults into the highest tier they've unlocked (settings.flagTier), which is
// bumped to 1 the first time they clear TIER 0.

// tierMapIdxs returns the CampaignMaps indices belonging to a tier, in order.
func tierMapIdxs(tier int) []int {
	var out []int
	for i := range gm.CampaignMaps {
		if gm.CampaignMaps[i].Tier == tier {
			out = append(out, i)
		}
	}
	return out
}

// maxTier is the highest tier with any maps (0 if only onboarding exists).
func maxTier() int {
	m := 0
	for i := range gm.CampaignMaps {
		if t := gm.CampaignMaps[i].Tier; t > m {
			m = t
		}
	}
	return m
}

// pilotName is the player-facing roster label for a character body (matches the
// character picker), e.g. TIGER/ELEPHANT - unlike the editor's lowercase vocab.
func pilotName(body int) string {
	for _, e := range playerBodies {
		if e.body == body {
			return e.name
		}
	}
	if body == gm.BodyTank {
		return "TANK"
	}
	return "UNIT"
}

// tierLabel describes a tier for the picker.
func tierLabel(t int) string {
	switch t {
	case 0:
		return "TIER 0   Basic Training  (forgiving)"
	case 1:
		return "TIER 1   Competition"
	default:
		return fmt.Sprintf("TIER %d   Mastery", t)
	}
}

// runTierSelect lets a player who has cleared TIER 0 choose which unlocked tier to
// play (so they can drop back into onboarding). Highlight starts on the highest
// unlocked tier - the default. Returns the chosen tier, or back/quit.
func runTierSelect(w *bufio.Writer, cols, rows int, ip *input, unlocked int) (tier int, back, quit bool) {
	sel := unlocked // default: the highest unlocked tier
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		title := "FLAG RUN  -  CHOOSE A TIER"
		fmt.Fprintf(w, "\x1b[2;%dH\x1b[1;96m%s\x1b[0m", (cols-len(title))/2+1, title)
		for t := 0; t <= unlocked; t++ {
			l, pre, sgr := tierLabel(t), "   ", "\x1b[0;37m"
			if t == sel {
				pre, sgr = " > ", "\x1b[1;30;47m"
			}
			line := pre + l + " "
			fmt.Fprintf(w, "\x1b[%d;%dH%s%s\x1b[0m", 5+t*2, (cols-len(line))/2+1, sgr, line)
		}
		foot := "up/dn choose    ENTER play    Bksp back"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows, (cols-len(foot))/2+1, foot)
		w.Flush()
	}
	draw()
	for {
		select {
		case <-ip.quitCh:
			return 0, false, true
		case k := <-ip.events:
			switch k {
			case mkUp:
				sel = (sel - 1 + unlocked + 1) % (unlocked + 1)
				draw()
			case mkDown:
				sel = (sel + 1) % (unlocked + 1)
				draw()
			case mkEnter:
				return sel, false, false
			case mkBack, mkLeft:
				return 0, true, false
			}
		}
	}
}

// runFlagRun is the FLAG RUN entry. If the player has unlocked past TIER 0 it
// offers a tier picker (so onboarding can be replayed); TIER 0 forces a character
// per level (no picker), while a competitive tier picks a character then plays the
// life-pool gauntlet. Returns true only if the player quit the program.
func runFlagRun(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, dropfile string, s *userSettings, vbody int, vcolor [3]float64, playerName string) bool {
	tier := s.flagTier
	if hi := maxTier(); tier > hi {
		tier = hi
	}
	if tier < 0 {
		tier = 0
	}
	if tier > 0 { // unlocked past onboarding: choose a tier (incl. replaying TIER 0)
		sel, back, quit := runTierSelect(w, cols, rows, ip, tier)
		if quit {
			return true
		}
		if back {
			return false
		}
		tier = sel
	}
	if tier <= 0 {
		if len(tierMapIdxs(0)) > 0 {
			return runTier0(w, cols, rows, rows3d, rnd, ip, dropfile, s, vbody, vcolor, playerName)
		}
		tier = 1 // no onboarding maps: fall through to competitive
	}
	// Competitive tier: pick a character, then run the gauntlet.
	b, c, vback, vquit := runVehicleMenu(w, cols, rows, ip, s, dropfile)
	if vquit {
		return true
	}
	if vback {
		return false
	}
	idxs := tierMapIdxs(tier)
	if len(idxs) == 0 { // safety: nothing tagged for this tier -> every competitive map
		for i := range gm.CampaignMaps {
			if gm.CampaignMaps[i].Tier >= 1 {
				idxs = append(idxs, i)
			}
		}
	}
	return runCampaign(w, cols, rows, rows3d, rnd, ip, dropfile, s, b, c, playerName, idxs)
}

// runResumeChoice asks whether to resume TIER 0 at the next uncompleted level or
// start over from the top. Returns the chosen 0-based start index, or back/quit.
func runResumeChoice(w *bufio.Writer, cols, rows int, ip *input, resumeLevel int) (start int, back, quit bool) {
	opts := []string{fmt.Sprintf("RESUME at level %d", resumeLevel), "START OVER from level 1"}
	sel := 0
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		title := "TIER 0  -  BASIC TRAINING"
		fmt.Fprintf(w, "\x1b[2;%dH\x1b[1;96m%s\x1b[0m", (cols-len(title))/2+1, title)
		for i, o := range opts {
			pre, sgr := "   ", "\x1b[0;37m"
			if i == sel {
				pre, sgr = " > ", "\x1b[1;30;47m"
			}
			line := pre + o + " "
			fmt.Fprintf(w, "\x1b[%d;%dH%s%s\x1b[0m", 6+i*2, (cols-len(line))/2+1, sgr, line)
		}
		foot := "up/dn choose    ENTER play    Bksp back"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows, (cols-len(foot))/2+1, foot)
		w.Flush()
	}
	draw()
	for {
		select {
		case <-ip.quitCh:
			return 0, false, true
		case k := <-ip.events:
			switch k {
			case mkUp, mkDown:
				sel ^= 1
				draw()
			case mkEnter:
				if sel == 0 {
					return resumeLevel - 1, false, false // resume at the next level (0-based)
				}
				return 0, false, false // start over
			case mkBack, mkLeft:
				return 0, true, false
			}
		}
	}
}

// runTier0 plays the onboarding tier: each level forces its taught character and
// opens on a splash. It's purely forgiving - a death or a timeout just replays the
// SAME level (no life pool, no reset), and each cleared level is banked so a later
// visit resumes there (the player can also choose to start over). Clearing every
// level unlocks TIER 1.
func runTier0(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, dropfile string, s *userSettings, vbody int, vcolor [3]float64, playerName string) bool {
	idxs := tierMapIdxs(0)
	start := 0
	if s.flagT0Done > 0 && s.flagT0Done < len(idxs) { // partial progress: resume or restart
		sel, back, quit := runResumeChoice(w, cols, rows, ip, s.flagT0Done+1)
		if quit {
			return true
		}
		if back {
			return false
		}
		start = sel
	}
	for li := start; li < len(idxs); li++ {
		m := gm.CampaignMaps[idxs[li]]
		body := vbody
		if m.Intro != nil {
			body = m.Intro.Body // the level teaches one character
		}
		updatePresence(dropfile, "flagrun", fmt.Sprintf("TIER 0 - level %d", li+1))
		mi := len(gm.Maps) // append-play-truncate (same pattern as the editor/campaign)
		gm.Maps = append(gm.Maps, m)
		// TIER 0 is onboarding: force gentle (Easy) enemies regardless of the player's
		// difficulty setting, and the player even gets aim-lock at this tier.
		sess := newOfflineOnMap(mi, 0, gm.EffectiveMode(m), gm.DiffEasy, s.aimAssist, playerName, vcolor, body)
		act := runLevelSplash(w, cols, rows, rows3d, rnd, ip, sess, s, m, li+1, len(idxs))
		if act != splashGo {
			sess.close()
			gm.Maps = gm.Maps[:mi]
			return act == splashQuit
		}
		quit, _ := playMatch(w, cols, rows, rows3d, rnd, ip, sess, dropfile, "flagrun", m.Name, true, true, nil)
		snap := sess.w.Match()
		tanks, _, _, _ := sess.w.Snapshot()
		var me gm.TankSnap
		for i := range tanks {
			if tanks[i].ID == sess.me {
				me = tanks[i]
				break
			}
		}
		sess.close()
		gm.Maps = gm.Maps[:mi]
		if quit {
			return true
		}
		if snap.FlagsTotal > 0 && snap.FlagsLeft == 0 { // cleared: bank progress, advance
			if li+1 > s.flagT0Done {
				s.flagT0Done = li + 1
				saveUserSettings(dropfile, *s)
			}
			continue
		}
		// Not cleared. A death or a timeout just retries the level (forgiving). If the
		// player is alive with flags remaining they Esc'd out -> back to the menu;
		// progress is banked, so the next visit resumes here.
		if (me.Dead && me.Lives <= 0) || snap.Phase == gm.PhaseEnded {
			runInfoScreen(w, cols, rows, ip, "TRY AGAIN", []string{
				fmt.Sprintf("TIER 0 - Level %d: %s", li+1, m.Name),
				"No penalty in training - give it another go.",
			})
			li-- // replay the same level
			continue
		}
		return false // backed out to the menu
	}
	if s.flagTier < 1 {
		s.flagTier = 1
		saveUserSettings(dropfile, *s)
	}
	runInfoScreen(w, cols, rows, ip, "BASIC TRAINING COMPLETE", []string{
		"You've cleared every TIER 0 level.",
		"FLAG RUN now drops you into TIER 1 - real competition,",
		"and you pick your own character.",
	})
	return false
}

type splashAction int

const (
	splashGo splashAction = iota // ENTER: roll out
	splashBack                   // Backspace: back to the menu
	splashQuit                   // program quit
)

// runLevelSplash shows the pre-level briefing: the map with the forced character
// posed on it under a slowly orbiting camera, plus the level title and the one
// teaching tooltip (its {action} placeholders already resolved to live keys).
// ENTER rolls out, Backspace backs out.
func runLevelSplash(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, sess *offlineSession, s *userSettings, m gm.Map, level, total int) splashAction {
	tanks, shots, flags, pickups := sess.w.Snapshot()
	ents, zones := sess.w.Entities(), sess.w.Zones()
	var pc gm.V3
	body := gm.BodyTank
	for i := range tanks {
		if tanks[i].ID == sess.me {
			pc, body = tanks[i].Pos, tanks[i].Body
			break
		}
	}
	// Drop the border walls for the briefing: the orbiting camera circles the
	// outside, and the walls would just block the view. playMatch rebuilds the
	// arena (with walls) on its first frame, so this only affects the splash.
	savedHide := hideArenaWalls
	hideArenaWalls = true
	defer func() { hideArenaWalls = savedHide }()
	buildArena(m) // renderWorld draws the static arena from package globals
	tip := ""
	if m.Intro != nil {
		tip = resolveTip(m.Intro.Tip, s)
	}
	title := fmt.Sprintf("TIER 0      LEVEL %d / %d", level, total)
	char := "PILOT:  " + pilotName(body)
	foot := "ENTER  roll out       Bksp  back"
	budget := time.Second / 20
	var prev []byte
	start := time.Now()
	ctr := func(row int, sgr, txt string) {
		fmt.Fprintf(w, "\x1b[%d;%dH%s%s\x1b[0m", row, (cols-len(txt))/2+1, sgr, txt)
	}
	for {
		select {
		case <-ip.quitCh:
			return splashQuit
		case k := <-ip.events:
			switch k {
			case mkEnter:
				return splashGo
			case mkBack:
				return splashBack
			}
		default:
		}
		now := time.Now()
		t := now.Sub(start).Seconds()
		ang := t * 0.5 // slow orbit around the posed pilot
		sy, cy := math.Sin(ang), math.Cos(ang)
		const r = 7.5
		cam := Cam{pos: V3{pc.X - sy*r, pc.Y + 3.4, pc.Z - cy*r}, yaw: ang, pitch: 0.34}
		rnd.renderWorld(cam, t, tanks, shots, flags, pickups, m.Entities, ents, zones, -1, 0, false, false, [3]byte{}, 0)
		frame, cur := encode(rnd, cols, rows3d, prev)
		prev = cur
		w.Write(frame)
		ctr(1, "\x1b[1;96m", title)
		ctr(2, "\x1b[1;97m", m.Name)
		ctr(3, "\x1b[1;93m", char)
		if tip != "" {
			ctr(rows-2, "\x1b[1;30;47m", " "+tip+" ")
		}
		ctr(rows, "\x1b[0;90m", foot)
		w.Flush()
		if d := budget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
}
