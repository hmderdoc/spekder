package game

import "testing"

// TestCustomVehicleOverride: a custom build sims with its tuned stats while still
// rendering as its chassis (Vehicle index), and inherits chassis-only fields.
func TestCustomVehicleOverride(t *testing.T) {
	w := NewWorld(2, ModeDeathmatch) // 2 bots: they must NOT inherit the custom build
	cs := CustomStats{MaxHP: 130, Speed: 7.7, HullTurn: 2.0, FireDelay: 0.45, AmmoMax: 12, AmmoRegen: 2.0}
	cv := MakeCustom(0 /* SCOUT chassis */, cs)
	id := w.AddPlayerCustom([3]float64{0.3, 0.3, 0.3}, 0, "X", &cv, BodyHumanoid)
	tk := &w.Tanks[id]

	if tk.body != BodyHumanoid {
		t.Fatalf("body style not applied: %d", tk.body)
	}

	if tk.Vehicle != 0 {
		t.Fatalf("chassis index should be 0 for rendering, got %d", tk.Vehicle)
	}
	if tk.veh().MaxHP != 130 || tk.veh().Speed != 7.7 || tk.veh().FireDelay != 0.45 {
		t.Fatalf("custom stats not applied: %+v", tk.veh())
	}
	if tk.HP != 130 {
		t.Fatalf("spawn HP should be the custom max, got %d", tk.HP)
	}
	if tk.veh().Scale != Veh(0).Scale { // Scale comes from the chassis, not tuned
		t.Fatalf("custom should inherit chassis scale: %v vs %v", tk.veh().Scale, Veh(0).Scale)
	}
	for i := range w.Tanks {
		if w.Tanks[i].Bot && w.Tanks[i].custom != nil {
			t.Fatal("bots must not receive the player's custom build")
		}
	}
}
