package game

import (
	"math"
	"testing"
)

// TestAimLockSkipsOwnTurret: a crab's aim-assist must not lock onto its OWN
// deployed turret, even when the turret sits closer to the reticle than the foe.
func TestAimLockSkipsOwnTurret(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[me].body = BodyCrab
	// own turret dead on the aim line (would win on yoff if it were eligible)...
	w.entities = []Entity{{
		Kind: "turret", Pos: V3{X: 0, Y: 0, Z: 5}, Half: V3{X: 0.45, Y: 0.5, Z: 0.45},
		Destruct: &DestructTrait{MaxHP: 120}, HP: 120, Owner: me, Turret: &TurretTrait{Range: 16},
	}}
	w.Tanks[me].turretIdx = 1
	// ...and an enemy slightly off it
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0
	w.aimAssistStep(me, true, false, 1.0/30)
	if w.Tanks[me].lockKind == 2 {
		t.Fatal("aim-assist locked the crab's OWN turret")
	}
}

// twoPlayerOpen sets up a deathmatch on the clear OPEN map with two human tanks
// (so the second is an enemy for FFA aim-assist tests), at the origin.
func twoPlayerOpen(t *testing.T) (*World, int, int) {
	t.Helper()
	w := NewWorld(0, ModeDeathmatch)
	w.SetDifficulty(DiffEasy) // sticky lock-on is an easy-tier aid; these tests exercise that path
	if idx := FindMap("OPEN GRID"); idx >= 0 {
		w.PinMap(idx)
	}
	me := w.AddPlayer([3]float64{}, "P", BodyTank)
	tgt := w.AddPlayer([3]float64{}, "P", BodyTank)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}, tgt: {}})
	w.Tanks[me].Pos = V3{}
	w.Tanks[me].HullYaw, w.Tanks[me].TurretYaw, w.Tanks[me].TurretPitch = 0, 0, 0
	w.Tanks[me].guard = 0
	return w, me, tgt
}

// TestAimLockCatchesTarget: turning the turret while a target is within the
// capture radius locks the aim onto it (snap), not a gradual nudge.
func TestAimLockCatchesTarget(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0 // ~0.10 rad off the +Z aim
	want := math.Atan2(1, 10)
	w.aimAssistStep(me, true, false, 1.0/30) // sweeping the turret
	if w.Tanks[me].lockKind == 0 {
		t.Fatal("turning past a near target should lock on")
	}
	if aim := w.Tanks[me].HullYaw + w.Tanks[me].TurretYaw; math.Abs(aim-want) > 1e-6 {
		t.Fatalf("lock should snap aim onto the target (%v), got %v", want, aim)
	}
}

// TestAimLockOnlyWithinRadius: a target outside the capture radius isn't locked.
func TestAimLockOnlyWithinRadius(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 9, Z: 10}, 0 // ~0.73 rad off
	w.aimAssistStep(me, true, false, 1.0/30)
	if w.Tanks[me].lockKind != 0 || w.Tanks[me].TurretYaw != 0 {
		t.Fatalf("no lock outside the capture radius (kind=%d yaw=%v)", w.Tanks[me].lockKind, w.Tanks[me].TurretYaw)
	}
}

// TestAimLockNeedsSweep: assist only acquires while the player is turning/elevating.
func TestAimLockNeedsSweep(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0
	w.aimAssistStep(me, false, false, 1.0/30) // idle: no turn/elevate
	if w.Tanks[me].lockKind != 0 {
		t.Fatal("should not acquire a lock while idle")
	}
}

// TestAimLockDisabled: with assist off, no lock ever forms.
func TestAimLockDisabled(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(false)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0
	w.aimAssistStep(me, true, false, 1.0/30)
	if w.Tanks[me].lockKind != 0 || w.Tanks[me].TurretYaw != 0 {
		t.Fatal("assist disabled: no lock, no movement")
	}
}

// TestAimLockHoldsThenBreaksOnSustainedTurn: the lock must SURVIVE the continued
// sweep that acquired it (otherwise it self-cancels and never holds a target), hold
// while aiming still, and release only on a sustained turn-away.
func TestAimLockHoldsThenBreaksOnSustainedTurn(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0
	const dt = 1.0 / 30
	w.aimAssistStep(me, true, false, dt) // acquires while sweeping
	if w.Tanks[me].lockKind == 0 {
		t.Fatal("should lock first")
	}
	w.aimAssistStep(me, true, false, dt) // a single continued turn tick
	if w.Tanks[me].lockKind == 0 {
		t.Fatal("a single turn tick should not break the lock (it must hold)")
	}
	for i := 0; i < 20; i++ { // holding still keeps the lock
		w.aimAssistStep(me, false, false, dt)
	}
	if w.Tanks[me].lockKind == 0 {
		t.Fatal("holding still should keep the lock")
	}
	for i := 0; i < int(assistLockBreak/dt)+2; i++ { // sustained turn-away releases
		w.aimAssistStep(me, true, false, dt)
	}
	if w.Tanks[me].lockKind != 0 {
		t.Fatal("a sustained turn should release the lock")
	}
}

// TestStandardAutoAimNoStickyLock: at the harder tiers the assist snaps onto a
// target you're aiming near but does NOT form a tracking lock (you stay free).
func TestStandardAutoAimNoStickyLock(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetDifficulty(DiffHard)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0
	want := math.Atan2(1, 10)
	w.aimAssistStep(me, true, false, 1.0/30)
	if w.Tanks[me].lockKind != 0 {
		t.Fatal("harder tiers should not form a sticky lock")
	}
	if aim := w.Tanks[me].HullYaw + w.Tanks[me].TurretYaw; math.Abs(aim-want) > 1e-6 {
		t.Fatalf("standard auto-aim should still snap onto the target (%v), got %v", want, aim)
	}
}

// TestAimLockHoldsWhileStill: once locked, releasing the turn keeps the lock so
// you can fire (it doesn't break just because you stop adjusting).
func TestAimLockHoldsWhileStill(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, 0
	w.aimAssistStep(me, true, false, 1.0/30) // catch
	if w.Tanks[me].lockKind == 0 {
		t.Fatal("should lock")
	}
	for i := 0; i < 30; i++ { // ~1s idle
		w.aimAssistStep(me, false, false, 1.0/30)
	}
	if w.Tanks[me].lockKind == 0 {
		t.Fatal("lock should hold while the player is still")
	}
}

// TestAimLockTurretEntity: assist locks onto shootable map entities (turrets /
// destructibles) too - the small, elevated, hard-to-hit things it's most for.
func TestAimLockTurretEntity(t *testing.T) {
	w := NewWorld(0, ModeDeathmatch)
	w.SetDifficulty(DiffEasy) // lock-on tier
	if idx := FindMap("OPEN GRID"); idx >= 0 {
		w.PinMap(idx)
	}
	me := w.AddPlayer([3]float64{}, "P", BodyTank)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	w.Tanks[me].Pos = V3{}
	w.Tanks[me].HullYaw, w.Tanks[me].TurretYaw, w.Tanks[me].TurretPitch = 0, 0, 0
	w.SetAimAssist(true)
	w.entities = []Entity{{
		Kind: "turret", Pos: V3{X: 1, Y: 2, Z: 10}, Half: V3{X: 0.7, Y: 0.3, Z: 0.7}, Owner: -1,
		Destruct: &DestructTrait{MaxHP: 50}, HP: 50, Turret: &TurretTrait{Range: 20},
	}}
	w.aimAssistStep(me, true, false, 1.0/30)
	if w.Tanks[me].lockKind != 2 {
		t.Fatalf("should lock onto the turret entity, kind=%d", w.Tanks[me].lockKind)
	}
	if w.Tanks[me].TurretPitch <= 0 {
		t.Fatalf("lock should elevate onto the raised turret, pitch=%v", w.Tanks[me].TurretPitch)
	}
	w.entities[0].Dead = true // destroyed: lock should drop
	w.aimAssistStep(me, false, false, 1.0/30)
	if w.Tanks[me].lockKind != 0 {
		t.Fatal("lock should clear when the target is destroyed")
	}
}

// TestAimLockSkipsCloaked: a cloaked enemy in the radius isn't locked.
func TestAimLockSkipsCloaked(t *testing.T) {
	w, me, tgt := twoPlayerOpen(t)
	w.SetAimAssist(true)
	w.Tanks[tgt].Pos, w.Tanks[tgt].cloakT = V3{X: 1, Z: 10}, buffCloakTime
	w.aimAssistStep(me, true, false, 1.0/30)
	if w.Tanks[me].lockKind != 0 {
		t.Fatal("should not lock onto a cloaked enemy")
	}
}
