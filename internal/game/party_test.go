package game

import "testing"

// assignTeams keeps the friendly default - with no parties, all humans play
// together (co-op vs bots) - and only splits humans once parties exist, keeping
// each party (and the shared solo pool) whole on one side.
func TestAssignTeamsByParty(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 30}}

	w := &World{Mode: ModeCTF}
	w.Tanks = []Tank{
		{Team: -1, Carrying: -1, Party: "RED"},
		{Team: -1, Carrying: -1, Party: "RED"},
		{Team: -1, Carrying: -1, Party: "BLUE"},
		{Team: -1, Carrying: -1, Party: "BLUE"},
	}
	w.assignTeams()
	if w.Tanks[0].Team != w.Tanks[1].Team {
		t.Fatal("party RED was split across teams")
	}
	if w.Tanks[2].Team != w.Tanks[3].Team {
		t.Fatal("party BLUE was split across teams")
	}
	if w.Tanks[0].Team == w.Tanks[2].Team {
		t.Fatal("the two parties landed on the same team")
	}

	// No parties: two solo callers stay together (co-op vs bots), NOT split.
	w2 := &World{Mode: ModeCTF}
	w2.Tanks = []Tank{{Team: -1, Carrying: -1}, {Team: -1, Carrying: -1}}
	w2.assignTeams()
	if w2.Tanks[0].Team != w2.Tanks[1].Team {
		t.Fatal("with no parties, solo callers should stay on the same team")
	}

	// One party + a solo: the party opts into the split, so it lands opposite the
	// solo pool (the solo isn't carved up - there's just one of them here).
	w4 := &World{Mode: ModeCTF}
	w4.Tanks = []Tank{
		{Team: -1, Carrying: -1, Party: "DUO"},
		{Team: -1, Carrying: -1, Party: "DUO"},
		{Team: -1, Carrying: -1},
	}
	w4.assignTeams()
	if w4.Tanks[0].Team != w4.Tanks[1].Team {
		t.Fatal("party DUO was split")
	}
	if w4.Tanks[2].Team == w4.Tanks[0].Team {
		t.Fatal("the solo should be opposite the party once a party exists")
	}

	// A party is kept whole even against the balance: 3 in a party + 1 solo means
	// the party stays together (3 v 1, bots even it out), not split 2-2.
	w3 := &World{Mode: ModeCTF}
	w3.Tanks = []Tank{
		{Team: -1, Carrying: -1, Party: "PACK"},
		{Team: -1, Carrying: -1, Party: "PACK"},
		{Team: -1, Carrying: -1, Party: "PACK"},
		{Team: -1, Carrying: -1},
	}
	w3.assignTeams()
	if !(w3.Tanks[0].Team == w3.Tanks[1].Team && w3.Tanks[1].Team == w3.Tanks[2].Team) {
		t.Fatal("a 3-player party was split")
	}
	if w3.Tanks[3].Team == w3.Tanks[0].Team {
		t.Fatal("the solo player should be opposite the 3-player party")
	}
}
