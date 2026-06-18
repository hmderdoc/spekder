package game

import "testing"

// TestSecondaryWeaponAlignment guards the Weapons table index alignment against
// the wep* iota consts and the per-character secondary assignments wired in the
// Phase 1 secondary-weapons overhaul. If a const or table entry is added out of
// order, these name lookups drift and this test catches it.
func TestSecondaryWeaponAlignment(t *testing.T) {
	cases := []struct {
		idx  int
		name string
	}{
		{wepSand, "SAND"},
		{wepRoar, "ROAR"},
		{wepClaw, "CLAW"},
	}
	for _, c := range cases {
		if got := Weapons[c.idx].Name; got != c.name {
			t.Errorf("Weapons[%d].Name = %q, want %q", c.idx, got, c.name)
		}
	}

	if got := bodyDefFor(BodyCrab).weapon; got != wepSand {
		t.Errorf("BodyCrab primary weapon = %d, want wepSand(%d)", got, wepSand)
	}
	if got := bodyDefFor(BodyCrab).secondary; got != -1 {
		t.Errorf("BodyCrab secondary = %d, want -1 (B deploys a turret, not a palette weapon)", got)
	}
	if got := bodyDefFor(BodyTrex).secondary; got != wepRoar {
		t.Errorf("BodyTrex secondary = %d, want wepRoar(%d)", got, wepRoar)
	}
}
