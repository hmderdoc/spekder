package game

import "testing"

// The attract demo's campaign sim: a hero bot must actually collect flags on
// a real campaign level (enemies defend, hero gathers).
func TestDemoHeroCollectsFlags(t *testing.T) {
	if len(CampaignMaps) == 0 {
		t.Skip("no campaign maps")
	}
	idx := len(Maps)
	Maps = append(Maps, CampaignMaps[0])
	defer func() { Maps = Maps[:idx] }()
	enemies := 3
	if r := CampaignMaps[0].Rules; r != nil && r.Bots >= 0 {
		enemies = r.Bots
	}
	w := NewWorld(enemies+1, ModeFlagRun)
	w.PinMap(idx)
	w.SetDemoHero(enemies)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{})
	for s := 0; s < 120; s++ {
		drive(w, 1, 1.0/30, map[int]Input{})
		if m := w.Match(); m.FlagsTotal > 0 && m.FlagsLeft < m.FlagsTotal {
			return // the stand-in is gathering: the sim teaches the mode
		}
	}
	t.Fatal("120s on FLAG-1 and the demo hero never collected a flag")
}
