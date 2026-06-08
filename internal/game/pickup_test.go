package game

import "testing"

func startDM(t *testing.T, bots int) (*World, int) {
	t.Helper()
	w := NewWorld(bots, ModeDeathmatch)
	me := w.AddPlayer([3]float64{}, 1)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	if w.Phase != PhaseActive {
		t.Fatalf("expected active phase, got %v", w.Phase)
	}
	return w, me
}

func TestPickupRepairHeals(t *testing.T) {
	w, me := startDM(t, 1)
	w.Tanks[me].HP = 10
	w.Tanks[me].Pos = V3{X: 5, Z: 5}
	w.pickups = []Pickup{{Pos: V3{X: 5, Z: 5}, Kind: PickRepair}}
	w.stepPickups(1.0 / 30)
	if w.Tanks[me].HP != veh(w.Tanks[me].Vehicle).MaxHP {
		t.Fatalf("repair should heal to full, got %d", w.Tanks[me].HP)
	}
	if len(w.pickups) != 0 {
		t.Fatalf("pickup should be consumed, %d left", len(w.pickups))
	}
}

func TestPickupShieldBlocksDamage(t *testing.T) {
	w, me := startDM(t, 1)
	w.Tanks[me].Pos = V3{X: 0, Z: 0}
	w.Tanks[me].guard = 0
	w.pickups = []Pickup{{Pos: V3{X: 0, Z: 0}, Kind: PickShield}}
	w.stepPickups(1.0 / 30)
	if w.Tanks[me].shieldT <= 0 {
		t.Fatalf("shield should be active after pickup")
	}
	hp := w.Tanks[me].HP
	w.Shots = append(w.Shots, Projectile{Pos: w.Tanks[me].Pos, owner: -1, life: 1})
	w.stepProjectiles(1.0 / 30)
	if w.Tanks[me].HP != hp {
		t.Fatalf("shield should block damage (%d -> %d)", hp, w.Tanks[me].HP)
	}
}

func TestPickupRapidShortensReload(t *testing.T) {
	w, me := startDM(t, 1)
	base := veh(w.Tanks[me].Vehicle).FireDelay
	w.Tanks[me].rapidT = buffRapidTime
	w.Tanks[me].cooldown = 0
	w.fire(me)
	if w.Tanks[me].cooldown >= base {
		t.Fatalf("rapid fire should shorten cooldown (%v vs base %v)", w.Tanks[me].cooldown, base)
	}
}

func TestPickupCloakHidesFromBots(t *testing.T) {
	w, me := startDM(t, 1)
	// Find the bot.
	bot := -1
	for i := range w.Tanks {
		if w.Tanks[i].Bot && !w.Tanks[i].gone {
			bot = i
			break
		}
	}
	if bot < 0 {
		t.Skip("no bot present")
	}
	// Put them within sight (bots now have a difficulty-tier sight range, so a
	// far spawn wouldn't be acquired regardless of cloak).
	w.Tanks[bot].Pos = V3{}
	w.Tanks[me].Pos = V3{X: 5}
	// With no cloak, the bot's nearest enemy is the human.
	if got := w.nearestEnemy(bot); got != me {
		t.Fatalf("bot should target the human, got %d", got)
	}
	w.Tanks[me].cloakT = buffCloakTime
	if got := w.nearestEnemy(bot); got == me {
		t.Fatalf("cloaked human should not be targetable")
	}
}

func TestPickupBuffsClearOnDeath(t *testing.T) {
	w, me := startDM(t, 1)
	w.Tanks[me].rapidT = buffRapidTime
	w.Tanks[me].cloakT = buffCloakTime
	w.Tanks[me].shieldT = 0 // not shielded, so the hit lands
	w.Tanks[me].guard = 0
	w.Tanks[me].HP = 1
	w.Shots = append(w.Shots, Projectile{Pos: w.Tanks[me].Pos, owner: -1, life: 1})
	w.stepProjectiles(1.0 / 30)
	if !w.Tanks[me].Dead {
		t.Fatalf("tank should be dead")
	}
	if w.Tanks[me].rapidT != 0 || w.Tanks[me].cloakT != 0 {
		t.Fatalf("buffs should clear on death: rapid=%v cloak=%v", w.Tanks[me].rapidT, w.Tanks[me].cloakT)
	}
}

func TestPickupSpawnCapAndConsume(t *testing.T) {
	w, me := startDM(t, 2)
	in := map[int]Input{me: {}}
	for i := 0; i < 60*30; i++ {
		w.Update(1.0/30, in)
		if len(w.pickups) > pickupMax {
			t.Fatalf("pickups exceeded cap: %d", len(w.pickups))
		}
	}
}
