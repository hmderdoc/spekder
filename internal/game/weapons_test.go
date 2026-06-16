package game

import (
	"math"
	"testing"
)

// twoTanks builds a tiny world with a shooter (0) and a target (1) on an open map.
func twoTanks(t *testing.T) *World {
	t.Helper()
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 30}}
	w := &World{Mode: ModeDeathmatch}
	w.Tanks = []Tank{
		{HP: 100, Team: -1, Carrying: -1},
		{HP: 60, Team: -1, Carrying: -1, Pos: V3{X: 0, Z: 5}},
	}
	return w
}

func TestEffectHeal(t *testing.T) {
	w := twoTanks(t)
	w.applyShotHit(&Projectile{owner: 0, eff: EffHeal, mag: 25, affects: TargetAllies}, 1)
	if w.Tanks[1].HP != 85 {
		t.Fatalf("heal: HP=%d want 85", w.Tanks[1].HP)
	}
	// never overheals past MaxHP
	w.Tanks[1].HP = veh(1).MaxHP - 5
	w.applyShotHit(&Projectile{owner: 0, eff: EffHeal, mag: 50, affects: TargetAllies}, 1)
	if w.Tanks[1].HP != veh(1).MaxHP {
		t.Fatalf("overheal: HP=%d want %d", w.Tanks[1].HP, veh(1).MaxHP)
	}
}

func TestEffectSlow(t *testing.T) {
	w := twoTanks(t)
	w.applyShotHit(&Projectile{owner: 0, eff: EffSlow, mag: 0.5, dur: 2, affects: TargetFoes}, 1)
	if w.Tanks[1].slowT != 2 || w.Tanks[1].slowMag != 0.5 {
		t.Fatalf("slow not applied: t=%v mag=%v", w.Tanks[1].slowT, w.Tanks[1].slowMag)
	}
}

func TestEffectKnockback(t *testing.T) {
	w := twoTanks(t)
	z0 := w.Tanks[1].Pos.Z
	w.applyShotHit(&Projectile{owner: 0, eff: EffKnockback, mag: 3, vel: V3{Z: 24}, affects: TargetFoes}, 1)
	if d := w.Tanks[1].Pos.Z - z0; math.Abs(d-3) > 0.01 {
		t.Fatalf("knockback moved %v, want ~3 along +Z", d)
	}
}

func TestEffectShieldBust(t *testing.T) {
	w := twoTanks(t)
	w.Tanks[1].shieldT = 5
	w.applyShotHit(&Projectile{owner: 0, eff: EffShieldBust, affects: TargetFoes}, 1)
	if w.Tanks[1].shieldT != 0 {
		t.Fatalf("shield not stripped: %v", w.Tanks[1].shieldT)
	}
}

func TestEffectDamageStillWorks(t *testing.T) {
	w := twoTanks(t)
	w.applyShotHit(&Projectile{owner: 0, eff: EffDamage, dmg: 20, affects: TargetFoes}, 1)
	if w.Tanks[1].HP != 40 {
		t.Fatalf("damage: HP=%d want 40", w.Tanks[1].HP)
	}
}

// A blast weapon splashes every foe inside its radius, not just one.
func TestBlastAoE(t *testing.T) {
	w := twoTanks(t)
	w.Tanks = append(w.Tanks, Tank{HP: 100, Team: -1, Carrying: -1, Pos: V3{X: 1, Z: 5}})
	s := &Projectile{owner: 0, eff: EffDamage, dmg: 30, blast: 3, affects: TargetFoes}
	w.detonate(s, V3{X: 0, Z: 5}) // both tanks 1 and 2 are within 3 units
	if w.Tanks[1].HP != 30 || w.Tanks[2].HP != 70 {
		t.Fatalf("blast: HP1=%d HP2=%d want 30/70", w.Tanks[1].HP, w.Tanks[2].HP)
	}
}

// Firing draws from the regenerating ammo pool; an empty pool throttles fire.
func TestAmmoGate(t *testing.T) {
	w := twoTanks(t)
	w.Tanks[0].ammo = 3 // exactly one grenade (cost 3)
	n := len(w.Shots)
	w.fireWeapon(0, &Weapons[wepGrenade], true)
	if len(w.Shots) != n+1 || w.Tanks[0].ammo != 0 {
		t.Fatalf("grenade should fire and drain ammo: shots+%d ammo=%v", len(w.Shots)-n, w.Tanks[0].ammo)
	}
	w.fireWeapon(0, &Weapons[wepGrenade], true) // now empty
	if len(w.Shots) != n+1 {
		t.Fatal("should not fire with insufficient ammo")
	}
}

// A damage blast also splashes destructible map entities (turrets / walls).
func TestBlastHitsEntity(t *testing.T) {
	w := twoTanks(t)
	w.entities = []Entity{{Kind: "turret", Pos: V3{X: 0, Z: 5}, Half: V3{X: 0.7, Y: 0.7, Z: 0.7}, HP: 60, Destruct: &DestructTrait{MaxHP: 60}}}
	w.detonate(&Projectile{owner: 0, eff: EffDamage, dmg: 32, blast: 4, affects: TargetFoes}, V3{X: 0, Z: 5})
	if w.entities[0].HP != 28 {
		t.Fatalf("entity blast: HP=%d want 28", w.entities[0].HP)
	}
}

// shotCanAffect enforces friend/foe: no friendly fire, support hits only allies.
func TestShotCanAffectTargeting(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "CTF", Size: 30}}
	w := &World{Mode: ModeCTF} // a team mode so teammates exist
	w.Tanks = []Tank{
		{HP: 100, Team: 0, Carrying: -1},                // shooter
		{HP: 100, Team: 0, Carrying: -1, Pos: V3{X: 2}}, // teammate
		{HP: 100, Team: 1, Carrying: -1, Pos: V3{X: 4}}, // enemy
	}
	foe := &Projectile{owner: 0, affects: TargetFoes}
	if w.shotCanAffect(foe, 1) {
		t.Fatal("foe weapon must not hit a teammate")
	}
	if !w.shotCanAffect(foe, 2) {
		t.Fatal("foe weapon should hit an enemy")
	}
	heal := &Projectile{owner: 0, eff: EffHeal, affects: TargetAllies}
	if !w.shotCanAffect(heal, 1) {
		t.Fatal("support weapon should hit a teammate")
	}
	if w.shotCanAffect(heal, 2) {
		t.Fatal("support weapon must not hit an enemy")
	}
}
