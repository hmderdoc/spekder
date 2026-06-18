package game

import (
	"strings"
	"testing"
)

// ccWorld builds a small deathmatch world with n free-for-all tanks at the origin.
// It swaps in a one-map test set and restores the real (embedded) Maps on cleanup
// so it can't strand the maplint/maps tests that run after it.
func ccWorld(t *testing.T, n int) *World {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 30}}
	w := &World{Mode: ModeDeathmatch}
	w.Tanks = make([]Tank, n)
	for i := range w.Tanks {
		w.Tanks[i] = Tank{HP: 100, Team: -1, Carrying: -1, body: BodyTank, ammo: 20, strangledBy: -1}
	}
	return w
}

// TestPounceStun: the tiger's pounce both damages and freezes - a stunned tank
// can't move on its own input.
func TestPounceStun(t *testing.T) {
	w := ccWorld(t, 2)
	w.Tanks[1].body = BodyHumanoid // resistance-neutral victim (BodyTank is now melee-weak)
	w.applyShotHit(&Projectile{owner: 0, eff: EffStun, dur: 1.0, dmg: 18, affects: TargetFoes, cause: CauseMelee}, 1)
	if w.Tanks[1].stunT <= 0 {
		t.Fatal("pounce didn't stun the target")
	}
	if w.Tanks[1].HP != 82 {
		t.Fatalf("pounce should also deal its 18 damage, HP=%d", w.Tanks[1].HP)
	}
	x0, z0 := w.Tanks[1].Pos.X, w.Tanks[1].Pos.Z
	w.applyInput(1, Input{Throttle: true}, 0.5)
	if w.Tanks[1].Pos.X != x0 || w.Tanks[1].Pos.Z != z0 {
		t.Fatal("a stunned tank moved")
	}
}

// TestResistances covers the per-body immunity & weakness system.
func TestResistances(t *testing.T) {
	// poison-immune: a serpent takes no venom damage and gets no DoT
	w := ccWorld(t, 2)
	w.Tanks[1].body = BodySerpent
	hp := w.Tanks[1].HP
	w.applyShotHit(&Projectile{owner: 0, eff: EffPoison, dmg: 10, mag: 5, dur: 4, affects: TargetFoes, cause: CausePoison}, 1)
	if w.Tanks[1].HP != hp || w.Tanks[1].dotT != 0 {
		t.Errorf("poison-immune serpent took venom: HP %d->%d dotT=%v", hp, w.Tanks[1].HP, w.Tanks[1].dotT)
	}

	// slow-immune: a tiger isn't slowed
	w = ccWorld(t, 2)
	w.Tanks[1].body = BodyQuad
	w.applyShotHit(&Projectile{owner: 0, eff: EffSlow, mag: 0.5, dur: 3, affects: TargetFoes}, 1)
	if w.Tanks[1].slowT != 0 {
		t.Errorf("slow-immune tiger got slowed: %v", w.Tanks[1].slowT)
	}

	// fire-weak: a gorilla takes 1.5x fire
	w = ccWorld(t, 2)
	w.Tanks[1].body = BodyGorilla
	hp = w.Tanks[1].HP
	w.hurt(1, 20, 0, CauseFire)
	if got := hp - w.Tanks[1].HP; got != 30 {
		t.Errorf("fire-weak gorilla took %d, want 30 (20x1.5)", got)
	}

	// fire-immune: an elephant hit by a flame DoT takes no damage AND no burn
	// applied (no Burning visual) - the dot must not be set.
	w = ccWorld(t, 2)
	w.Tanks[1].body = BodyElephant
	hp = w.Tanks[1].HP
	w.applyShotHit(&Projectile{owner: 0, eff: EffDrain, dmg: 12, mag: 4, dur: 3, affects: TargetFoes, cause: CauseFire}, 1)
	if w.Tanks[1].HP != hp || w.Tanks[1].dotT != 0 {
		t.Errorf("fire-immune elephant burned: HP %d->%d dotT=%v (want no damage, no burn)", hp, w.Tanks[1].HP, w.Tanks[1].dotT)
	}

	// melee-weak: a tank takes 1.5x melee
	w = ccWorld(t, 2) // both BodyTank
	hp = w.Tanks[1].HP
	w.hurt(1, 20, 0, CauseMelee)
	if got := hp - w.Tanks[1].HP; got != 30 {
		t.Errorf("melee-weak tank took %d, want 30", got)
	}

	// knock-weak: a tank is shoved further (mag 2 -> 3)
	w = ccWorld(t, 2)
	w.Tanks[1].Pos = V3{}
	w.areaHit(&Projectile{owner: 0, eff: EffKnockback, mag: 2, affects: TargetFoes}, 1, 1, 0, 1)
	if dx := w.Tanks[1].Pos.X; dx < 2.9 {
		t.Errorf("knock-weak tank shoved only %v, want ~3", dx)
	}
}

// TestOctopodSpy covers the spy kit: fire immunity, the rear-arc backstab, the
// uncloaked damage/speed bonus, and the blink (teleport-jump that keeps cloak).
func TestOctopodSpy(t *testing.T) {
	// fire immunity
	w := ccWorld(t, 2)
	w.Tanks[1].body = BodyOctopod
	hp := w.Tanks[1].HP
	w.hurt(1, 30, 0, CauseFire)
	if w.Tanks[1].HP != hp {
		t.Errorf("octopod should be fire-immune, took %d", hp-w.Tanks[1].HP)
	}

	// backstab: octopod behind a +Z-facing target lands the 3x stab (+ uncloaked 1.25x)
	w = ccWorld(t, 2)
	w.Tanks[0].body = BodyOctopod
	w.Tanks[0].Pos = V3{Z: -1} // behind the target
	w.Tanks[1].Pos = V3{}
	w.Tanks[1].HullYaw = 0 // faces +Z; the octopod at -Z is in its rear arc
	back := w.Tanks[1].HP
	w.fireWeapon(0, &Weapons[wepStab], false)
	bd := back - w.Tanks[1].HP

	// front stab: no backstab bonus
	w = ccWorld(t, 2)
	w.Tanks[0].body = BodyOctopod
	w.Tanks[0].Pos = V3{Z: 1} // in front
	w.Tanks[1].Pos = V3{}
	w.Tanks[1].HullYaw = 0
	front := w.Tanks[1].HP
	w.fireWeapon(0, &Weapons[wepStab], false)
	fd := front - w.Tanks[1].HP
	if bd <= fd*2 {
		t.Errorf("backstab (%d) should dwarf a front stab (%d)", bd, fd)
	}

	// uncloaked move-speed bonus
	if MoveSpeedMul(BodyOctopod, false) <= MoveSpeedMul(BodyOctopod, true) {
		t.Error("an uncloaked octopod should move faster than a cloaked one")
	}

	// blink: JUMP teleports forward, keeps cloak, sets a cooldown
	w = ccWorld(t, 1)
	w.Tanks[0].body = BodyOctopod
	w.Tanks[0].cloakT = inkCloakDur
	x0 := w.Tanks[0].Pos
	w.applyInput(0, Input{Jump: true}, 1.0/30)
	if w.Tanks[0].Pos == x0 {
		t.Error("blink didn't move the octopod")
	}
	if w.Tanks[0].cloakT <= 0 {
		t.Error("blink should keep cloak")
	}
	if w.Tanks[0].blinkCdT <= 0 {
		t.Error("blink should set a cooldown")
	}
}

// TestTurtleRearShield: a precise shot into a turtle's rear arc is blocked by its
// shell; the same shot from the front lands.
func TestTurtleRearShield(t *testing.T) {
	w := ccWorld(t, 2)
	w.Tanks[1].body = BodyTurtle
	w.Tanks[1].HullYaw = 0 // faces +Z

	w.Tanks[0].Pos = V3{Z: -5} // behind the turtle
	hp := w.Tanks[1].HP
	nfx := len(w.spawnQ)
	w.shotImpact(&Projectile{owner: 0, dmg: 20}, w.Tanks[1].Pos, 1)
	if w.Tanks[1].HP != hp {
		t.Errorf("rear shot should be shielded, turtle took %d", hp-w.Tanks[1].HP)
	}
	if len(w.spawnQ) <= nfx {
		t.Error("a blocked rear shot should spawn a deflect puff (feedback)")
	}

	w.Tanks[0].Pos = V3{Z: 5} // in front
	w.shotImpact(&Projectile{owner: 0, dmg: 20}, w.Tanks[1].Pos, 1)
	if w.Tanks[1].HP == hp {
		t.Error("a front shot should hit the turtle")
	}
}

// teamWorld builds a team-mode world (Teams==2) with n tanks all on team 0 at full
// HP, so healer target-selection (mostHurtAlly) can be exercised directly.
func teamWorld(t *testing.T, n int) *World {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 60}}
	w := &World{Mode: ModeTeamKotH}
	if w.rules().Teams != 2 {
		t.Fatalf("expected a 2-team ruleset, got %d", w.rules().Teams)
	}
	w.Tanks = make([]Tank, n)
	for i := range w.Tanks {
		w.Tanks[i] = Tank{Team: 0, Carrying: -1, body: BodyTank, ammo: 20, strangledBy: -1, healTgt: -1}
		w.Tanks[i].HP = w.Tanks[i].veh().MaxHP // full health by default
	}
	return w
}

func TestHealerTargeting(t *testing.T) {
	full := bodyVeh[BodyTank].MaxHP // 150

	// proximity: between two equally-wounded allies, pick the closer one.
	w := teamWorld(t, 3)
	w.Tanks[0].body = BodyButterfly // point healer
	w.Tanks[1].HP, w.Tanks[1].Pos = full*60/100, V3{X: 5}   // 60%, near
	w.Tanks[2].HP, w.Tanks[2].Pos = full*60/100, V3{Z: 25}  // 60%, far
	if got := w.mostHurtAlly(0, -1); got != 1 {
		t.Errorf("proximity: want near ally 1, got %d", got)
	}

	// urgency still wins: a critically hurt far ally beats a barely-scratched near one.
	w = teamWorld(t, 3)
	w.Tanks[0].body = BodyButterfly
	w.Tanks[1].HP, w.Tanks[1].Pos = full*90/100, V3{X: 3}  // 90%, point-blank
	w.Tanks[2].HP, w.Tanks[2].Pos = full*30/100, V3{Z: 25} // 30%, far
	if got := w.mostHurtAlly(0, -1); got != 2 {
		t.Errorf("urgency: want critical far ally 2, got %d", got)
	}

	// hysteresis: a committed target is kept against a marginally-worse rival, but
	// dropped when another ally is clearly more hurt.
	w = teamWorld(t, 3)
	w.Tanks[0].body = BodyButterfly
	w.Tanks[1].HP, w.Tanks[1].Pos = full*60/100, V3{X: 5}
	w.Tanks[2].HP, w.Tanks[2].Pos = full*53/100, V3{X: -5} // marginally worse, same dist
	if got := w.mostHurtAlly(0, -1); got != 2 {
		t.Errorf("no commitment: should pick the worse ally 2, got %d", got)
	}
	if got := w.mostHurtAlly(0, 1); got != 1 {
		t.Errorf("hysteresis: should stick with committed ally 1, got %d", got)
	}
	w.Tanks[2].HP = full * 25 / 100 // now clearly worse
	if got := w.mostHurtAlly(0, 1); got != 2 {
		t.Errorf("hysteresis should yield to a clearly-worse ally 2, got %d", got)
	}

	// cluster (radial healer): a stag favours the wounded ally surrounded by other
	// wounded allies (so the aura pulse heals 2+), not an equally-hurt loner.
	w = teamWorld(t, 5)
	w.Tanks[0].body = BodyStag
	w.Tanks[1].HP, w.Tanks[1].Pos = full*60/100, V3{X: 6}  // clustered with 3 & 4
	w.Tanks[3].HP, w.Tanks[3].Pos = full*60/100, V3{X: 8}
	w.Tanks[4].HP, w.Tanks[4].Pos = full*60/100, V3{X: 4}
	w.Tanks[2].HP, w.Tanks[2].Pos = full*60/100, V3{Z: -22} // isolated loner
	got := w.mostHurtAlly(0, -1)
	if got == 2 || got < 1 {
		t.Errorf("cluster: should heal a clustered ally (1/3/4), not the loner, got %d", got)
	}

	// healthy squad -> nobody to heal; a wounded ENEMY is never a heal target.
	w = teamWorld(t, 3)
	w.Tanks[0].body = BodyButterfly
	w.Tanks[2].Team, w.Tanks[2].HP = 1, full*10/100 // enemy, near death
	if got := w.mostHurtAlly(0, -1); got != -1 {
		t.Errorf("healthy allies + wounded enemy: want -1, got %d", got)
	}
}

// TestCritFX: a crit-flagged hit pops a spark burst the player can read; a plain
// direct bolt hit does not (only the crit gets the extra juice).
func TestCritFX(t *testing.T) {
	w := ccWorld(t, 2)
	w.Tanks[1].body = BodyHumanoid

	base := len(w.spawnQ)
	w.applyShotHit(&Projectile{owner: 0, eff: EffDamage, dmg: 10, affects: TargetFoes}, 1)
	if len(w.spawnQ) != base {
		t.Fatalf("a plain bolt hit shouldn't spawn a burst, queued %d", len(w.spawnQ)-base)
	}
	w.applyShotHit(&Projectile{owner: 0, eff: EffDamage, dmg: 10, affects: TargetFoes, crit: true}, 1)
	if len(w.spawnQ) <= base {
		t.Error("a crit hit should pop a crit FX burst")
	}
}

// TestStagRally: the battle-medic's RALLY burst heals allies and shoves foes (the
// single area weapon does both - heal kin, knock enemies off the point).
func TestStagRally(t *testing.T) {
	w := teamWorld(t, 3)
	w.Tanks[0].body = BodyStag
	w.Tanks[1].HP = bodyVeh[BodyTank].MaxHP * 60 / 100 // wounded ally (team 0)
	w.Tanks[1].Pos = V3{X: 3}
	w.Tanks[2].Team, w.Tanks[2].body = 1, BodyHumanoid // enemy (knock-neutral)
	w.Tanks[2].Pos = V3{X: 5}

	rally := &Projectile{owner: 0, eff: EffHeal, mag: 30, dmg: 8, affects: TargetAllies, foeKnock: 6, cause: CauseMelee}

	// ally: healed, not shoved.
	hp, x := w.Tanks[1].HP, w.Tanks[1].Pos.X
	w.areaHit(rally, 1, 3, 0, 3)
	if w.Tanks[1].HP <= hp {
		t.Errorf("RALLY should heal the ally: %d -> %d", hp, w.Tanks[1].HP)
	}
	if w.Tanks[1].Pos.X != x {
		t.Errorf("RALLY should not shove a teammate, moved %v", w.Tanks[1].Pos.X-x)
	}

	// foe: stung + shoved outward (away from the stag at origin).
	fhp, fx := w.Tanks[2].HP, w.Tanks[2].Pos.X
	w.areaHit(rally, 2, 5, 0, 5)
	if w.Tanks[2].HP != fhp-8 {
		t.Errorf("RALLY should sting the foe for 8: %d -> %d", fhp, w.Tanks[2].HP)
	}
	if w.Tanks[2].Pos.X <= fx {
		t.Errorf("RALLY should knock the foe outward: x %v -> %v", fx, w.Tanks[2].Pos.X)
	}
}

// TestStagGore: the antler charge deals contact damage to a foe while leap-charging,
// once per charge (cooldown), and not while standing still.
func TestStagGore(t *testing.T) {
	w := ccWorld(t, 2)
	w.Tanks[0].body = BodyStag
	w.Tanks[1].body = BodyHumanoid // knock/melee-neutral victim
	w.Tanks[1].Pos = V3{X: 2}      // inside goreRange

	// not charging (no lunge velocity) -> no gore.
	hp := w.Tanks[1].HP
	w.goreCharge(0)
	if w.Tanks[1].HP != hp {
		t.Fatalf("a standing stag should not gore: %d -> %d", hp, w.Tanks[1].HP)
	}

	// mid-charge -> gores once for goreDmg (24), with hit feedback.
	w.Tanks[0].lungeVX = lungeSpeed
	w.goreCharge(0)
	if w.Tanks[1].HP != hp-24 {
		t.Fatalf("gore should deal 24: %d -> %d", hp, w.Tanks[1].HP)
	}
	if w.Tanks[0].goreT <= 0 {
		t.Fatal("gore should start its cooldown")
	}
	if w.Tanks[1].hitFlash <= 0 {
		t.Error("gore should flash the victim (the hit was visually mute before)")
	}
	if len(w.spawnQ) == 0 {
		t.Error("gore should spawn an impact FX")
	}
	// still charging but on cooldown -> no second hit.
	hp2 := w.Tanks[1].HP
	w.goreCharge(0)
	if w.Tanks[1].HP != hp2 {
		t.Errorf("gore should not re-hit during cooldown: %d -> %d", hp2, w.Tanks[1].HP)
	}
}

// TestTurtleShellShieldStatus: the SHELL SHIELD makes the turtle immune to every
// debilitating status - poison, slow, bleed, drain - while still taking the direct
// hit a bleed/drain rides on, and still taking knockback (NOT a shell immunity).
func TestTurtleShellShieldStatus(t *testing.T) {
	mk := func() *World { w := ccWorld(t, 2); w.Tanks[1].body = BodyTurtle; return w }

	// poison: a venom bolt does nothing (full cause immunity) - no damage, no DoT.
	w := mk()
	hp := w.Tanks[1].HP
	w.applyShotHit(&Projectile{owner: 0, eff: EffPoison, dmg: 10, mag: 5, dur: 4, affects: TargetFoes, cause: CausePoison}, 1)
	if w.Tanks[1].HP != hp || w.Tanks[1].dotT != 0 {
		t.Errorf("turtle took poison: HP %d->%d dotT=%v", hp, w.Tanks[1].HP, w.Tanks[1].dotT)
	}

	// slow: not slowed.
	w = mk()
	w.applyShotHit(&Projectile{owner: 0, eff: EffSlow, mag: 0.5, dur: 3, affects: TargetFoes}, 1)
	if w.Tanks[1].slowT != 0 {
		t.Errorf("turtle got slowed: %v", w.Tanks[1].slowT)
	}

	// bleed: takes the direct claw, but the bleed DoT never sticks.
	w = mk()
	hp = w.Tanks[1].HP
	w.applyShotHit(&Projectile{owner: 0, eff: EffBleed, dmg: 22, mag: 6, dur: 4, affects: TargetFoes, cause: CauseBleed}, 1)
	if w.Tanks[1].HP != hp-22 {
		t.Errorf("turtle should take the direct hit (want -22), HP %d->%d", hp, w.Tanks[1].HP)
	}
	if w.Tanks[1].dotT != 0 {
		t.Errorf("turtle bled: dotT=%v", w.Tanks[1].dotT)
	}

	// drain (fire cause - NOT fire-immune): takes the chip, but no leech DoT sticks.
	w = mk()
	hp = w.Tanks[1].HP
	w.applyShotHit(&Projectile{owner: 0, eff: EffDrain, dmg: 12, mag: 4, dur: 3, affects: TargetFoes, cause: CauseFire}, 1)
	if w.Tanks[1].HP != hp-12 {
		t.Errorf("turtle should take the flame chip (want -12), HP %d->%d", hp, w.Tanks[1].HP)
	}
	if w.Tanks[1].dotT != 0 {
		t.Errorf("turtle drained: dotT=%v", w.Tanks[1].dotT)
	}

	// knockback is NOT a shell immunity - the turtle still gets shoved.
	if resistMul(BodyTurtle, ResKnock) != 1 {
		t.Errorf("turtle should take normal knockback, got mul %v", resistMul(BodyTurtle, ResKnock))
	}

	// the loadout panel advertises all four status immunities.
	imm := BodyImmune(BodyTurtle)
	for _, want := range []string{"POISON", "SLOW", "BLEED", "DRAIN"} {
		if !strings.Contains(imm, want) {
			t.Errorf("BodyImmune(turtle)=%q missing %s", imm, want)
		}
	}
	if strings.Contains(imm, "KNOCK") {
		t.Errorf("BodyImmune(turtle)=%q should not list KNOCK", imm)
	}
}

// TestEvasionAndCrit covers the serpent/falcon move-scaled dodge + crit tables.
func TestEvasionAndCrit(t *testing.T) {
	if bodyDodge(BodySerpent) <= 0 || bodyDodge(BodyFalcon) <= 0 || bodyDodge(BodyTank) != 0 {
		t.Fatal("dodge table wrong")
	}
	if c, m := bodyCrit(BodySerpent); c <= 0 || m <= 1 {
		t.Fatalf("serpent crit table: %v x%v", c, m)
	}

	w := ccWorld(t, 2)
	w.Tanks[0].Pos = V3{Z: -20} // far shooter
	w.Tanks[1].body = BodySerpent

	// STILL serpent (moveFrac 0) never dodges.
	w.Tanks[1].moveFrac = 0
	for k := 0; k < 50; k++ {
		if w.dodged(&Projectile{owner: 0}, 1) {
			t.Fatal("a stationary serpent dodged")
		}
	}
	// MOVING serpent at range dodges at least sometimes.
	w.Tanks[1].moveFrac = 1
	hit := 0
	for k := 0; k < 300; k++ {
		if w.dodged(&Projectile{owner: 0}, 1) {
			hit++
		}
	}
	if hit == 0 {
		t.Error("a fast-moving serpent at range never dodged")
	}
	// A non-dodge body never dodges, even moving.
	w.Tanks[1].body = BodyTank
	w.Tanks[1].moveFrac = 1
	if w.dodged(&Projectile{owner: 0}, 1) {
		t.Error("a tank dodged")
	}
}

// TestStrangleRoots: the serpent's strangle roots the victim (can't move) but
// leaves it able to act.
func TestStrangleRoots(t *testing.T) {
	w := ccWorld(t, 2)
	w.applyShotHit(&Projectile{owner: 0, eff: EffStrangle, dur: 2.5, affects: TargetFoes}, 1)
	if w.Tanks[1].strangleT <= 0 || w.Tanks[1].strangledBy != 0 {
		t.Fatalf("strangle didn't root: T=%v by=%d", w.Tanks[1].strangleT, w.Tanks[1].strangledBy)
	}
	x0 := w.Tanks[1].Pos.X
	w.applyInput(1, Input{Throttle: true}, 0.5)
	if w.Tanks[1].Pos.X != x0 {
		t.Fatal("a strangled tank drove free")
	}
}

// TestStrangleEscapes covers all three break conditions plus the rule that the
// victim's OWN hit on the strangler does not free it.
func TestStrangleEscapes(t *testing.T) {
	reStrangle := func(w *World) {
		w.applyShotHit(&Projectile{owner: 0, eff: EffStrangle, dur: 2.5, affects: TargetFoes}, 1)
	}

	// (a) a melee swing tears free
	w := ccWorld(t, 2)
	reStrangle(w)
	w.fireWeapon(1, &Weapons[wepSlash], false)
	if w.Tanks[1].strangleT != 0 {
		t.Error("a melee swing didn't break the strangle")
	}

	// (b) a knockback move shoves the victim free
	w = ccWorld(t, 2)
	reStrangle(w)
	w.areaHit(&Projectile{owner: 0, eff: EffKnockback, mag: 2, affects: TargetFoes}, 1, 1, 0, 1)
	if w.Tanks[1].strangleT != 0 {
		t.Error("a knockback didn't break the strangle")
	}

	// (c) a THIRD party hitting the strangler frees the victim...
	w = ccWorld(t, 3)
	reStrangle(w)
	w.hurt(0, 5, 2, CauseCannon) // tank 2 shoots the strangler (tank 0)
	if w.Tanks[1].strangleT != 0 {
		t.Error("a 3rd-party hit on the strangler didn't free the victim")
	}

	// ...but the victim's OWN hit on the strangler does not
	w = ccWorld(t, 3)
	reStrangle(w)
	w.hurt(0, 5, 1, CauseCannon) // the victim (tank 1) hits the strangler
	if w.Tanks[1].strangleT == 0 {
		t.Error("the victim's own hit wrongly freed it")
	}
}
