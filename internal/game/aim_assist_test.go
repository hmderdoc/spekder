package game

import (
	"math"
	"testing"
)

// twoPlayerOpen sets up a deathmatch on the clear OPEN map with two human tanks
// (so the second is an enemy for FFA aim-assist tests), at the origin.
func twoPlayerOpen(t *testing.T) (*World, int, int) {
	t.Helper()
	w := NewWorld(0, ModeDeathmatch)
	if idx := FindMap("OPEN GRID"); idx >= 0 {
		w.PinMap(idx)
	}
	me := w.AddPlayer([3]float64{}, 1)
	tgt := w.AddPlayer([3]float64{}, 1)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}, tgt: {}})
	w.Tanks[me].Pos = V3{}
	w.Tanks[me].HullYaw, w.Tanks[me].TurretYaw, w.Tanks[me].TurretPitch = 0, 0, 0
	w.Tanks[me].guard = 0
	return w, me, tgt
}

// TestAimAssistEasesOntoTarget: a target just inside the assist window gets the
// reticle eased onto it over a few ticks (not instantly).
func TestAimAssistEasesOntoTarget(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0 // ~0.10 rad off the +Z aim
	want := math.Atan2(1, 10)

	w.assistAimStep(me, 1.0/30) // one tick: should move toward, not snap
	one := w.Tanks[me].TurretYaw
	if one <= 0 || one >= want {
		t.Fatalf("one tick should ease partway (0 < %v < %v)", one, want)
	}
	for i := 0; i < 30; i++ {
		w.assistAimStep(me, 1.0/30)
	}
	if aim := w.Tanks[me].HullYaw + w.Tanks[me].TurretYaw; math.Abs(aim-want) > 0.01 {
		t.Fatalf("assist should settle on the target (~%v), got %v", want, aim)
	}
}

// TestAimAssistOnlyWithinWindow: a target well off the reticle is not assisted.
func TestAimAssistOnlyWithinWindow(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 9, Z: 10}, 0 // ~0.73 rad off, outside the window
	w.assistAimStep(me, 1.0/30)
	if w.Tanks[me].TurretYaw != 0 {
		t.Fatalf("no assist outside the window, turret moved to %v", w.Tanks[me].TurretYaw)
	}
}

// TestAimAssistDisabled: with assist off, the turret never auto-adjusts.
func TestAimAssistDisabled(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(false)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0 // inside the window
	w.assistAimStep(me, 1.0/30)
	if w.Tanks[me].TurretYaw != 0 {
		t.Fatalf("assist disabled but turret moved to %v", w.Tanks[me].TurretYaw)
	}
}

// TestAimAssistSkipsCloaked: a cloaked enemy in the window isn't assisted onto.
func TestAimAssistSkipsCloaked(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, buffCloakTime
	w.assistAimStep(me, 1.0/30)
	if w.Tanks[me].TurretYaw != 0 {
		t.Fatalf("should not assist onto a cloaked enemy, turret moved to %v", w.Tanks[me].TurretYaw)
	}
}
