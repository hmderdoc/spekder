package game

import "testing"

// escortWorld builds a minimal ESCORT world: a straight "main" path with a payload
// at the start, one escort (team 0) and one defender (team 1).
func escortWorld(t *testing.T) (*World, int) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{
		Name:     "E",
		Size:     40,
		Paths:    []Path{{Name: "main", Points: []V3{{X: -12}, {X: 12}}}},
		Entities: []Entity{{Kind: "payload", Pos: V3{X: -12}, Payload: &PayloadTrait{Path: "main", Radius: 4, Speed: 8}}},
	}}
	w := &World{Mode: ModeEscort}
	w.entities = w.ActiveMap().NewEntities()
	w.Tanks = []Tank{
		{Team: 0, body: BodyTank, HP: 100, Carrying: -1, strangledBy: -1}, // escort / attacker
		{Team: 1, body: BodyTank, HP: 100, Carrying: -1, strangledBy: -1}, // defender
	}
	w.setupPayload()
	return w, w.payloadEntity()
}

func step(w *World, pi int) {
	const dt = 1.0 / 30
	w.stepEscort(dt)
	if w.entities[pi].mvOn {
		w.stepMove(&w.entities[pi], pi, dt)
	}
}

func TestEscortDelivery(t *testing.T) {
	w, pi := escortWorld(t)
	w.Tanks[1].Pos = V3{X: 500, Z: 500} // defender nowhere near
	for i := 0; i < 3000 && !w.payloadDelivered(); i++ {
		w.Tanks[0].Pos = w.entities[pi].Pos // escort rides along with the payload
		step(w, pi)
	}
	if !w.payloadDelivered() {
		t.Fatal("uncontested payload should reach the goal")
	}
	w.Timer = 5 // not timed out; the delivery itself ends it
	w.checkEnd()
	if w.winnerTeam != 0 {
		t.Errorf("delivery -> attackers (team 0) win, got winnerTeam %d", w.winnerTeam)
	}
}

// TestEscortAdvancesInSimulate drives the real per-tick path (simulate, which holds
// the objective switch) rather than stepEscort directly - so if ObjEscort is ever
// mis-routed in that switch (the payload silently never advancing in real play),
// this fails where the direct-call tests would not.
func TestEscortAdvancesInSimulate(t *testing.T) {
	w, pi := escortWorld(t)
	w.Tanks[1].Pos = V3{X: 500, Z: 500} // defender nowhere near
	for i := 0; i < 600 && w.payloadProgress() == 0; i++ {
		w.Tanks[0].Pos = w.entities[pi].Pos // attacker rides along with the payload
		w.simulate(1.0/30, nil)
	}
	if w.payloadProgress() == 0 {
		t.Fatal("ObjEscort must route to stepEscort in simulate(); payload never advanced")
	}
}

func TestEscortContestedTimeout(t *testing.T) {
	w, pi := escortWorld(t)
	w.Tanks[0].Pos = w.entities[pi].Pos // escort present...
	w.Tanks[1].Pos = w.entities[pi].Pos // ...but a defender contests it
	for i := 0; i < 300; i++ {
		step(w, pi)
	}
	if w.payloadDelivered() {
		t.Fatal("a contested payload must not advance")
	}
	w.Timer = 0 // clock ran out with the payload undelivered
	w.checkEnd()
	if w.winnerTeam != 1 {
		t.Errorf("timeout undelivered -> defenders (team 1) win, got winnerTeam %d", w.winnerTeam)
	}
}

func TestEscortBotSeeksPayload(t *testing.T) {
	w, pi := escortWorld(t)
	w.Tanks[0].Bot = true
	// away from the payload -> seek it (and it's the payload's current position)
	w.Tanks[0].Pos = V3{X: -35, Z: -35}
	dest, seek, hold := w.botObjectiveDest(0)
	if !seek || hold {
		t.Fatalf("a bot off the payload should seek it (seek=%v hold=%v)", seek, hold)
	}
	if dest != w.entities[pi].Pos {
		t.Errorf("objective should be the payload at %v, got %v", w.entities[pi].Pos, dest)
	}
	// on the payload -> hold and fight (presence is what advances/contests it)
	w.Tanks[0].Pos = w.entities[pi].Pos
	if _, seek, hold = w.botObjectiveDest(0); seek || !hold {
		t.Errorf("a bot on the payload should hold (seek=%v hold=%v)", seek, hold)
	}
}

func TestEscortNaturalMode(t *testing.T) {
	m := Map{Entities: []Entity{{Payload: &PayloadTrait{}}}}
	if got := NaturalMode(m); got != ModeEscort {
		t.Errorf("a map with a payload should be ModeEscort, got %v", got)
	}
}
