package game

import "testing"

// TestBodyDefOverlayIsIdentity guards the switch->table refactor: with no push
// applied, the resolved bodyDef must equal the builtin def for every body, byte
// for byte. If this fails, the overlay seeding drifted from the switch.
func TestBodyDefOverlayIsIdentity(t *testing.T) {
	for b := 0; b < BodyKinds; b++ {
		if got, want := bodyDefFor(b), bodyDefBuiltin(b); got != want {
			t.Errorf("body %d: bodyDefFor=%+v != bodyDefBuiltin=%+v", b, got, want)
		}
	}
}

// TestApplyBalanceRetunes confirms a pushed tuning reaches the resolved stats
// (chassis via veh, body scalars via the accessors) and that CurrentBalance
// round-trips it. Mutates package globals, so it snapshots and restores them.
func TestApplyBalanceRetunes(t *testing.T) {
	saved := CurrentBalance()
	t.Cleanup(func() { ApplyBalance(saved) })

	bal := CurrentBalance()
	// nerf the tiger's (BodyQuad) HP and buff its speed - per-character now.
	bal.Rows[BodyQuad].MaxHP = 42
	bal.Bodies[BodyQuad].SpeedMul = 2.5
	ApplyBalance(bal)

	if VehBody(BodyQuad).MaxHP != 42 {
		t.Errorf("per-body MaxHP not applied: got %d, want 42", VehBody(BodyQuad).MaxHP)
	}
	if got := BodySpeedMul(BodyQuad); got != 2.5 {
		t.Errorf("body speedMul not applied: got %v, want 2.5", got)
	}
	// structural fields must be untouched by a numeric push.
	if PrimaryWeapon(BodyQuad) != wepScratch {
		t.Errorf("push altered kit identity: tiger primary changed")
	}

	// CurrentBalance reflects the live tables.
	if cur := CurrentBalance(); cur.Rows[BodyQuad].MaxHP != 42 || cur.Bodies[BodyQuad].SpeedMul != 2.5 {
		t.Errorf("CurrentBalance did not reflect applied tuning: %+v", cur.Rows[BodyQuad])
	}
}
