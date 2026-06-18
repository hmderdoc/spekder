package game

import "testing"

// TestCrabDeploysTurret covers the crab's deployable turret: B plants a turret
// owned by the crab, it engages the crab's enemy (and the shot credits the crab),
// and a second B near it repairs it.
func TestCrabDeploysTurret(t *testing.T) {
	Maps = []Map{{Name: "T", Size: 30}}
	w := &World{Mode: ModeDeathmatch}
	w.Tanks = []Tank{
		{HP: 150, Team: -1, Carrying: -1, body: BodyCrab, Pos: V3{}},     // crab at origin, facing +Z
		{HP: 100, Team: -1, Carrying: -1, body: BodyTank, Pos: V3{Z: 6}}, // enemy in range
	}

	w.crabDeploy(0)
	if len(w.entities) != 1 {
		t.Fatalf("deploy added %d entities, want 1", len(w.entities))
	}
	e := &w.entities[0]
	if e.Turret == nil {
		t.Fatal("deployed entity has no Turret trait")
	}
	if e.Owner != 0 {
		t.Fatalf("turret Owner = %d, want 0 (the crab)", e.Owner)
	}
	if e.HP != crabTurretHP {
		t.Fatalf("turret HP = %d, want %d", e.HP, crabTurretHP)
	}

	// It should engage the enemy and the shot must credit the crab (owner 0).
	for i := 0; i < 10 && len(w.Shots) == 0; i++ {
		w.stepEntities(1.0 / 30)
	}
	if len(w.Shots) == 0 {
		t.Fatal("turret never fired at the in-range enemy")
	}
	if w.Shots[0].owner != 0 {
		t.Errorf("turret shot owner = %d, want 0 (kills should credit the crab)", w.Shots[0].owner)
	}

	// Repair: damage it, B again while standing close, it heals.
	e.HP = 40
	w.Tanks[0].cooldown2 = 0
	w.crabDeploy(0)
	if e.HP <= 40 {
		t.Errorf("repair didn't heal the turret (HP %d)", e.HP)
	}
	if len(w.entities) != 1 {
		t.Errorf("repair should not deploy a second turret (have %d)", len(w.entities))
	}
}

// TestCrabRelocatesTurret: away from the turret, B does nothing until the redeploy
// timer elapses; after it, B tears the old turret down and re-plants it at the crab.
func TestCrabRelocatesTurret(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 30}}
	w := &World{Mode: ModeDeathmatch}
	w.Tanks = []Tank{{HP: 150, Team: -1, Carrying: -1, body: BodyCrab, Pos: V3{}}}

	w.crabDeploy(0)
	if len(w.entities) != 1 {
		t.Fatalf("deploy added %d entities, want 1", len(w.entities))
	}
	pos1 := w.entities[0].Pos

	// Walk far away and tap B before the timer: no relocation.
	w.Tanks[0].Pos = V3{X: 12, Z: 12}
	w.Tanks[0].cooldown2 = 0
	w.crabDeploy(0)
	if w.entities[0].Pos != pos1 {
		t.Fatal("turret relocated before the redeploy timer elapsed")
	}

	// Timer elapsed: B re-places it (same slot, new position near the crab).
	w.Tanks[0].redeployT = 0
	w.Tanks[0].cooldown2 = 0
	w.crabDeploy(0)
	if len(w.entities) != 1 {
		t.Fatalf("relocate should reuse the slot, have %d turrets", len(w.entities))
	}
	if w.entities[0].Pos == pos1 {
		t.Fatal("turret did not relocate after the timer elapsed")
	}
}
