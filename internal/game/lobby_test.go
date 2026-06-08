package game

import "testing"

// TestNaturalMode: a map's implied mode follows its objectives.
func TestNaturalMode(t *testing.T) {
	cases := []struct {
		ents []Entity
		want Mode
	}{
		{nil, ModeDeathmatch},
		{[]Entity{{Flag: &FlagTrait{Team: -1}}}, ModeFlagRun},
		{[]Entity{{Flag: &FlagTrait{Team: 0}}}, ModeCTF},
		{[]Entity{{Zone: &ZoneTrait{Capture: 4}}}, ModeFFAKotH},
		{[]Entity{{Zone: &ZoneTrait{}}, {Flag: &FlagTrait{Team: 1}}}, ModeCTF}, // team flag wins
	}
	for i, c := range cases {
		if got := NaturalMode(Map{Entities: c.ents}); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

// TestPickNextPairingVote: the most-voted map wins, with its implied mode; a lone
// human's vote therefore decides the pairing.
func TestPickNextPairingVote(t *testing.T) {
	saved := Maps
	defer func() { Maps = saved }()
	Maps = []Map{
		{Name: "A"}, // DM
		{Name: "B", Entities: []Entity{{Zone: &ZoneTrait{Capture: 4}}}}, // KotH
		{Name: "C"},
	}
	w := &World{Lobby: true}
	w.Tanks = []Tank{
		{Bot: false, vote: 1}, // human votes map B
		{Bot: true, vote: 0},  // bots don't count
	}
	idx, mode := w.pickNextPairing()
	if idx != 1 || mode != ModeFFAKotH {
		t.Fatalf("got (idx=%d mode=%v) want (1, KotH)", idx, mode)
	}

	// No human votes: rotate from the current map, don't stall.
	w.Tanks[0].vote = -1
	w.MapIdx = 0
	if idx, _ := w.pickNextPairing(); idx != 1 {
		t.Fatalf("no-vote rotation: got %d want 1", idx)
	}
}
