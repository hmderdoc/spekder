package game

import (
	"math"
	"testing"
)

// Recenter must snap the turret back to hull-forward in the authoritative sim.
func TestRecenterZerosTurretSim(t *testing.T) {
	w, me := startDM(t, 0)
	// Aim the turret well off-center.
	w.Tanks[me].TurretYaw = 1.2
	w.Update(1.0/30, map[int]Input{me: {Recenter: true}})
	if math.Abs(w.Tanks[me].TurretYaw) > 1e-9 {
		t.Fatalf("recenter should zero the turret, got %v", w.Tanks[me].TurretYaw)
	}
}

// Predict must apply recenter identically, so the net client doesn't fight the
// server snap.
func TestRecenterZerosTurretPredict(t *testing.T) {
	m := Map{}
	_, _, turret, pitch, _ := Predict(V3{}, 0, 1.5, 0.4, 0, Input{Recenter: true}, 1.0/30, Veh(1), m, nil)
	if math.Abs(turret) > 1e-9 {
		t.Fatalf("Predict recenter should zero the turret, got %v", turret)
	}
	if math.Abs(pitch) > 1e-9 {
		t.Fatalf("Predict recenter should also level the gun, got pitch %v", pitch)
	}
}
