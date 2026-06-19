package main

import (
	"bufio"
	"fmt"

	gm "spekder/internal/game"
)

// The FLAG RUN campaign: gm.CampaignMaps played in order with one life pool
// across the run - start with 3, every cleared level banks one more. Each
// level's map rules pin its enemy roster (bots: N, lives: 1, so they stay
// dead) while SetCampaignLives carries the player's pool over the map's
// lives setting. A level is cleared by collecting every flag before the
// clock; the run ends when the pool runs dry.

// campaignLives is the starting life pool for a fresh run.
const campaignLives = 3

// livesOf reads the player's remaining lives out of the session's world.
func livesOf(sess *offlineSession) int {
	tanks, _, _, _ := sess.w.Snapshot()
	for i := range tanks {
		if tanks[i].ID == sess.me {
			return tanks[i].Lives
		}
	}
	return 0
}

// runCampaign plays a competitive tier's maps (idxs into gm.CampaignMaps) from
// level 1 as one life-pool gauntlet. Returns true only if the player quit the
// program entirely.
func runCampaign(w *bufio.Writer, cols, rows, rows3d int, rnd *Renderer, ip *input, dropfile string, s *userSettings, vbody int, vcolor [3]float64, playerName string, idxs []int) bool {
	if len(idxs) == 0 {
		return false
	}
	tier := gm.CampaignMaps[idxs[0]].Tier // every map in idxs shares a tier
	tierScore := 0                        // summed across the levels of this session
	lives := campaignLives
	for li := 0; li < len(idxs); li++ {
		m := gm.CampaignMaps[idxs[li]]
		updatePresence(dropfile, "campaign", fmt.Sprintf("level %d", li+1))
		idx := len(gm.Maps) // the editor's append-play-truncate pattern
		gm.Maps = append(gm.Maps, m)
		sess := newOfflineOnMap(idx, 0, gm.EffectiveMode(m), s.difficulty, s.aimAssist, playerName, vcolor, vbody)
		sess.w.SetCampaignLives(lives)
		quit, _ := playMatch(w, cols, rows, rows3d, rnd, ip, sess, dropfile, "campaign", m.Name, true, false, nil)
		tierScore += lastMatchScore
		snap := sess.w.Match()
		left := livesOf(sess)
		sess.close()
		gm.Maps = gm.Maps[:idx]
		if quit {
			return true
		}
		if snap.Phase != gm.PhaseEnded && left > 0 {
			// Backed out mid-level (Esc / Backspace): the run is abandoned.
			runInfoScreen(w, cols, rows, ip, "RUN ABANDONED", []string{
				fmt.Sprintf("You left the campaign at level %d.", li+1),
				"A fresh run starts at level 1.",
			})
			return false
		}
		if cleared := snap.FlagsTotal > 0 && snap.FlagsLeft == 0; !cleared {
			recordTierRun(dropfile, tier, tierScore, int(s.difficulty), playerName)
			runInfoScreen(w, cols, rows, ip, "GAME OVER", []string{
				fmt.Sprintf("The run ends at level %d: %s.", li+1, m.Name),
				fmt.Sprintf("Best run so far: level %d.   Run score: %d.", s.campaignBest, tierScore),
				"",
				"Grab every flag before the clock - and keep a life in reserve.",
			})
			return false
		}
		if li+1 > s.campaignBest {
			s.campaignBest = li + 1
			saveUserSettings(dropfile, *s)
		}
		lives = left + 1 // the cleared-level bonus life
		if li == len(idxs)-1 {
			recordTierRun(dropfile, tier, tierScore, int(s.difficulty), playerName)
			runInfoScreen(w, cols, rows, ip, "CAMPAIGN COMPLETE", []string{
				fmt.Sprintf("All %d levels cleared, %d lives still in hand.", len(idxs), lives),
				fmt.Sprintf("Run score: %d.", tierScore),
				"The flags are safe. For now.",
			})
			return false
		}
		runInfoScreen(w, cols, rows, ip, fmt.Sprintf("LEVEL %d CLEARED", li+1), []string{
			fmt.Sprintf("+1 life banked. Lives for level %d: %d.", li+2, lives),
			"",
			"ENTER to roll out.",
		})
	}
	return false
}
