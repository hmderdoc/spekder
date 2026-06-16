package game

import "testing"

// TestClawChargeStock exercises the crab claw's 2-charge stock: two snaps consume
// both charges, a third is blocked, and one charge regenerates after ChargeRegen.
func TestClawChargeStock(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 30}}

	w := &World{Mode: ModeDeathmatch}
	w.Tanks = []Tank{
		{HP: 200, Team: -1, Carrying: -1, body: BodyCrab, weapon2: wepClaw, ammo: 100},
	}
	def := &Weapons[wepClaw]
	if def.Charges != 2 {
		t.Fatalf("CLAW Charges = %d, want 2", def.Charges)
	}

	// First snap consumes a charge.
	w.fireSecondary(0)
	if w.Tanks[0].wp2Used != 1 {
		t.Fatalf("after 1st fire wp2Used = %d, want 1", w.Tanks[0].wp2Used)
	}
	// Cooldown gates back-to-back snaps; clear it so the second charge is reachable.
	w.Tanks[0].cooldown2 = 0
	w.fireSecondary(0)
	if w.Tanks[0].wp2Used != 2 {
		t.Fatalf("after 2nd fire wp2Used = %d, want 2", w.Tanks[0].wp2Used)
	}
	// Third snap is blocked: stock is empty even with cooldown cleared.
	w.Tanks[0].cooldown2 = 0
	w.fireSecondary(0)
	if w.Tanks[0].wp2Used != 2 {
		t.Fatalf("3rd fire should be blocked, wp2Used = %d, want 2", w.Tanks[0].wp2Used)
	}

	// Advance time past one ChargeRegen; a single charge should return.
	steps := int(def.ChargeRegen/0.05) + 2
	for i := 0; i < steps; i++ {
		w.simulate(0.05, nil)
	}
	if w.Tanks[0].wp2Used != 1 {
		t.Fatalf("after %vs regen wp2Used = %d, want 1", def.ChargeRegen, w.Tanks[0].wp2Used)
	}
}

// TestPounceKillResetRefundsDash verifies a kill during the POUNCE window zeroes
// the secondary cooldown (the Genji-style chain refund) via the hurt credit path.
func TestPounceKillResetRefundsDash(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 30}}

	w := &World{Mode: ModeDeathmatch}
	w.Tanks = []Tank{
		{HP: 200, Team: -1, Carrying: -1, body: BodyQuad, weapon2: wepPounce}, // BodyQuad = tiger
		{HP: 1, Team: -1, Carrying: -1, Pos: V3{X: 0, Z: 2}},
	}
	w.Tanks[0].pounceT = pounceWindow
	w.Tanks[0].cooldown2 = 1.5 // mid-recharge

	// Tank 0 lands a lethal blow on tank 1.
	w.hurt(1, 50, 0, CauseMelee)

	if !w.Tanks[1].Dead {
		t.Fatalf("victim should be dead")
	}
	if w.Tanks[0].Kills != 1 {
		t.Fatalf("killer Kills = %d, want 1", w.Tanks[0].Kills)
	}
	if w.Tanks[0].cooldown2 != 0 {
		t.Fatalf("POUNCE kill should refund the dash, cooldown2 = %v, want 0", w.Tanks[0].cooldown2)
	}
}
