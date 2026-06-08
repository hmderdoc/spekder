package game

import (
	"math"
	"testing"
)

// TestTurretAimsAndFires: a turret already aimed at an in-range tank fires a
// projectile with no kill-credit owner (-1) carrying the trait's damage.
func TestTurretAimsAndFires(t *testing.T) {
	w, me := startDM(t, 0)
	w.Tanks[me].Pos = V3{X: 0, Z: 5} // straight ahead of a yaw=0 (+Z) turret
	w.Tanks[me].guard = 0
	w.Tanks[me].cloakT = 0
	w.entities = []Entity{{
		Kind: "turret", Pos: V3{X: 0, Y: 1, Z: 0}, Half: V3{X: 0.8, Y: 1, Z: 0.8},
		Yaw: 0, Turret: &TurretTrait{Range: 20, FireDelay: 1.0, Dmg: 10, TurnRate: 4},
	}}
	w.Shots = nil
	for i := 0; i < 5; i++ {
		w.stepEntities(1.0 / 30)
	}
	if len(w.Shots) == 0 {
		t.Fatal("turret should fire at an in-range tank")
	}
	if w.Shots[0].owner != -1 {
		t.Fatalf("turret shot owner should be -1 (no kill credit), got %d", w.Shots[0].owner)
	}
	if w.Shots[0].dmg != 10 {
		t.Fatalf("turret shot should carry trait dmg 10, got %d", w.Shots[0].dmg)
	}
}

// TestTurretTracksAndRespectsCloak: the barrel rotates toward an off-angle target
// over time (not instantly), and a cloaked tank is invisible to the turret.
func TestTurretTracksAndRespectsCloak(t *testing.T) {
	w, me := startDM(t, 0)
	w.Tanks[me].Pos = V3{X: 10, Z: 0} // off to the side: want yaw ~= +pi/2
	w.Tanks[me].guard = 0
	w.entities = []Entity{{
		Kind: "turret", Pos: V3{}, Half: V3{X: 0.8, Y: 1, Z: 0.8},
		Yaw: 0, Turret: &TurretTrait{Range: 20, FireDelay: 1.0, TurnRate: 1.0},
	}}
	w.stepEntities(1.0 / 30)
	yaw := w.entities[0].Yaw
	if yaw <= 0 || yaw >= math.Pi/2 {
		t.Fatalf("turret should track partway toward +pi/2, got %v", yaw)
	}
	// Cloak the only target: the turret should stop tracking (hold its facing).
	w.Tanks[me].cloakT = buffCloakTime
	before := w.entities[0].Yaw
	w.stepEntities(1.0 / 30)
	if w.entities[0].Yaw != before {
		t.Fatalf("turret should ignore a cloaked tank, yaw moved %v -> %v", before, w.entities[0].Yaw)
	}
}

// TestDestructibleDamageAndRespawn: projectiles whittle a destructible entity's
// HP, destroy it at 0, and the Respawn trait brings it back at full HP.
func TestDestructibleDamageAndRespawn(t *testing.T) {
	w, _ := startDM(t, 0)
	w.entities = []Entity{{
		Kind: "wall", Pos: V3{X: 0, Y: 1, Z: 0}, Half: V3{X: 1, Y: 1, Z: 1}, Solid: true,
		Destruct: &DestructTrait{MaxHP: 50}, Respawn: &RespawnTrait{Delay: 2}, HP: 50,
	}}
	s := Projectile{Pos: V3{X: 0, Y: 1, Z: 0}, dmg: 30}
	if !w.shotHitsEntity(&s) {
		t.Fatal("shot should be absorbed by the destructible")
	}
	if w.entities[0].HP != 20 {
		t.Fatalf("HP should drop to 20, got %d", w.entities[0].HP)
	}
	s2 := Projectile{Pos: V3{X: 0, Y: 1, Z: 0}, dmg: 30}
	w.shotHitsEntity(&s2)
	if !w.entities[0].Dead {
		t.Fatal("entity should be destroyed at 0 HP")
	}
	for i := 0; i < 3*30; i++ { // 3s > 2s respawn delay
		w.stepEntities(1.0 / 30)
	}
	if w.entities[0].Dead || w.entities[0].HP != 50 {
		t.Fatalf("entity should respawn at full HP, got dead=%v hp=%d", w.entities[0].Dead, w.entities[0].HP)
	}
}

// TestDestructibleNoRespawnStaysDead: without a Respawn trait, a destroyed
// entity is gone for the rest of the match.
func TestDestructibleNoRespawnStaysDead(t *testing.T) {
	w, _ := startDM(t, 0)
	w.entities = []Entity{{
		Kind: "wall", Pos: V3{X: 0, Y: 1, Z: 0}, Half: V3{X: 1, Y: 1, Z: 1}, Solid: true,
		Destruct: &DestructTrait{MaxHP: 10}, HP: 10,
	}}
	s := Projectile{Pos: V3{X: 0, Y: 1, Z: 0}, dmg: 30}
	w.shotHitsEntity(&s)
	if !w.entities[0].Dead {
		t.Fatal("entity should die")
	}
	for i := 0; i < 10*30; i++ {
		w.stepEntities(1.0 / 30)
	}
	if !w.entities[0].Dead {
		t.Fatal("entity without Respawn should stay dead")
	}
}

// TestHazardBurnsTankInsideOnly: a tank standing in a hazard footprint loses HP;
// stepping out stops the damage.
func TestHazardBurnsTankInsideOnly(t *testing.T) {
	w, me := startDM(t, 0)
	w.Tanks[me].Pos = V3{X: 0, Z: 0}
	w.Tanks[me].guard, w.Tanks[me].shieldT = 0, 0
	w.entities = []Entity{{Kind: "hazard", Pos: V3{}, Half: V3{X: 2, Y: 0.2, Z: 2}, Hazard: &HazardTrait{DPS: 30}}}
	hp := w.Tanks[me].HP
	for i := 0; i < 30; i++ {
		w.stepEntities(1.0 / 30)
	}
	if w.Tanks[me].HP >= hp {
		t.Fatalf("hazard should burn a tank inside it (%d -> %d)", hp, w.Tanks[me].HP)
	}
	w.Tanks[me].Pos = V3{X: 12, Z: 12} // clear of the footprint
	cur := w.Tanks[me].HP
	for i := 0; i < 30; i++ {
		w.stepEntities(1.0 / 30)
	}
	if w.Tanks[me].HP != cur {
		t.Fatalf("tank outside the hazard should take no damage (%d -> %d)", cur, w.Tanks[me].HP)
	}
}

// TestTeleporterWarpsAndDebounces: a tank in a teleporter footprint is moved to
// the destination and gets a debounce so it doesn't immediately re-trigger.
func TestTeleporterWarpsAndDebounces(t *testing.T) {
	w, me := startDM(t, 0)
	w.Tanks[me].Pos = V3{X: 0, Z: 0}
	w.Tanks[me].guard, w.Tanks[me].teleT = 0, 0
	w.entities = []Entity{{
		Kind: "teleporter", Pos: V3{}, Half: V3{X: 1, Y: 0.2, Z: 1},
		Teleport: &TeleportTrait{Dest: V3{X: 12, Z: -8}, Cooldown: 1},
	}}
	w.stepEntities(1.0 / 30)
	if w.Tanks[me].Pos.X != 12 || w.Tanks[me].Pos.Z != -8 {
		t.Fatalf("tank should warp to dest, got %+v", w.Tanks[me].Pos)
	}
	if w.Tanks[me].teleT <= 0 {
		t.Fatal("teleport should arm a per-tank debounce")
	}
}

// TestSolidCollision: CollideBoxes pushes a point out of a box's nearest side,
// and collidables() folds in alive solid entities (and excludes dead/non-solid).
func TestSolidCollision(t *testing.T) {
	boxes := []Box{{Pos: V3{X: 5, Y: 1, Z: 0}, Half: V3{X: 1, Y: 1, Z: 1}}}
	p := V3{X: 4.2, Y: 0, Z: 0} // inside the rad-inflated side (minx = 5-1-1 = 3)
	CollideBoxes(boxes, &p)
	if math.Abs(p.X-3) > 1e-6 {
		t.Fatalf("point should be pushed out to x=3, got %v", p.X)
	}

	w, _ := startDM(t, 0)
	base := len(w.ActiveMap().Obstacles)
	w.entities = []Entity{
		{Solid: true, Pos: V3{X: 1, Y: 1, Z: 1}, Half: V3{X: 1, Y: 1, Z: 1}},             // counts
		{Solid: true, Dead: true, Pos: V3{X: 2, Y: 1, Z: 2}, Half: V3{X: 1, Y: 1, Z: 1}}, // dead: excluded
		{Solid: false, Pos: V3{X: 3, Y: 1, Z: 3}, Half: V3{X: 1, Y: 1, Z: 1}},            // non-solid: excluded
	}
	if got := len(w.collidables()) - base; got != 1 {
		t.Fatalf("collidables should add exactly 1 solid entity box, added %d", got)
	}
	if n := len(SolidBoxes(Map{Entities: w.entities}, []EntitySnap{{}, {Dead: true}, {}})); n != 1 {
		t.Fatalf("SolidBoxes should return 1 (alive+solid), got %d", n)
	}
}

// TestTrampolineLaunches: a tank resting on a bounce pad gets a fixed upward
// velocity, and is re-launched after it comes back down (not while still rising).
func TestTrampolineLaunches(t *testing.T) {
	w, me := startDMMap(t, 0, "OPEN GRID")
	w.Tanks[me].Pos = V3{X: 0, Y: 0, Z: 0}
	w.Tanks[me].vy = 0
	w.entities = []Entity{{
		Kind: "trampoline", Pos: V3{X: 0, Y: 0.1, Z: 0}, Half: V3{X: 1.5, Y: 0.1, Z: 1.5},
		Bounce: &BounceTrait{Power: 12},
	}}
	w.stepEntities(1.0 / 30)
	if w.Tanks[me].vy != 12 {
		t.Fatalf("standing on the pad should launch upward (vy=12), got %v", w.Tanks[me].vy)
	}
	// While still rising, it must NOT re-trigger (debounce).
	w.Tanks[me].vy = 5
	w.Tanks[me].Pos.Y = 2
	w.stepEntities(1.0 / 30)
	if w.Tanks[me].vy != 5 {
		t.Fatalf("rising tank above the pad should not re-launch, vy became %v", w.Tanks[me].vy)
	}
	// Off the pad: no launch.
	w.Tanks[me].Pos = V3{X: 10, Y: 0, Z: 10}
	w.Tanks[me].vy = 0
	w.stepEntities(1.0 / 30)
	if w.Tanks[me].vy != 0 {
		t.Fatalf("a tank off the pad should not be launched, vy=%v", w.Tanks[me].vy)
	}
}

// startDMMap starts a deathmatch pinned to a named map (clear arena for
// deterministic projectile tests).
func startDMMap(t *testing.T, bots int, name string) (*World, int) {
	t.Helper()
	w := NewWorld(bots, ModeDeathmatch)
	if idx := FindMap(name); idx >= 0 {
		w.PinMap(idx)
	}
	me := w.AddPlayer([3]float64{}, 1)
	drive(w, countdownTime+0.2, 1.0/30, map[int]Input{me: {}})
	return w, me
}

func firstBot(w *World) int {
	for i := range w.Tanks {
		if w.Tanks[i].Bot && !w.Tanks[i].gone {
			return i
		}
	}
	return -1
}

// TestPitchedShotHeightMatters: with height-aware hit detection, a flat shot
// passes under an elevated target, but a shot at the target's height connects.
func TestPitchedShotHeightMatters(t *testing.T) {
	w, me := startDMMap(t, 1, "OPEN GRID")
	bot := firstBot(w)
	if bot < 0 {
		t.Skip("no bot")
	}
	w.Tanks[bot].Pos = V3{X: 0, Y: 3, Z: 6} // up on a ledge, dead ahead
	w.Tanks[bot].guard, w.Tanks[bot].shieldT, w.Tanks[bot].Dead = 0, 0, false
	hp := w.Tanks[bot].HP

	w.Shots = []Projectile{{Pos: V3{X: 0, Y: EyeHeight, Z: 6}, vel: V3{Z: 1}, owner: me, life: 1}}
	w.stepProjectiles(1.0 / 30)
	if w.Tanks[bot].HP != hp {
		t.Fatalf("flat shot should pass under an elevated target (hp %d -> %d)", hp, w.Tanks[bot].HP)
	}
	w.Shots = []Projectile{{Pos: V3{X: 0, Y: 3.3, Z: 6}, vel: V3{Z: 1}, owner: me, life: 1}}
	w.stepProjectiles(1.0 / 30)
	if w.Tanks[bot].HP >= hp {
		t.Fatalf("shot at the target's height should hit (hp stayed %d)", w.Tanks[bot].HP)
	}
}

// TestTurretDepressesAtGroundTarget: an elevated turret aims its gun DOWN (pitch
// < 0) toward a ground tank, instead of firing flat over it.
func TestTurretDepressesAtGroundTarget(t *testing.T) {
	w, me := startDMMap(t, 0, "OPEN GRID")
	w.Tanks[me].Pos = V3{X: 0, Y: 0, Z: 6}
	w.Tanks[me].guard, w.Tanks[me].cloakT = 0, 0
	w.entities = []Entity{{
		Kind: "turret", Pos: V3{X: 0, Y: 4, Z: 0}, Half: V3{X: 0.7, Y: 0.3, Z: 0.7},
		Turret: &TurretTrait{Range: 30, FireDelay: 1, TurnRate: 6},
	}}
	for i := 0; i < 12; i++ {
		w.stepEntities(1.0 / 30)
	}
	if w.entities[0].Pitch >= 0 {
		t.Fatalf("elevated turret should depress (pitch<0) onto a ground target, got %v", w.entities[0].Pitch)
	}
}

// TestPlayerAimPitchClampAndRecenter: AimUp raises the gun (clamped at pitchMax),
// and Recenter levels it back to 0.
func TestPlayerAimPitchClampAndRecenter(t *testing.T) {
	w, me := startDM(t, 0)
	for i := 0; i < 120; i++ { // hold aim-up well past the clamp
		w.Update(1.0/30, map[int]Input{me: {AimUp: true}})
	}
	if w.Tanks[me].TurretPitch <= 0 || w.Tanks[me].TurretPitch > pitchMax+1e-6 {
		t.Fatalf("aim-up should raise pitch up to pitchMax, got %v", w.Tanks[me].TurretPitch)
	}
	w.Update(1.0/30, map[int]Input{me: {Recenter: true}})
	if math.Abs(w.Tanks[me].TurretPitch) > 1e-9 {
		t.Fatalf("recenter should level the gun, got %v", w.Tanks[me].TurretPitch)
	}
}
