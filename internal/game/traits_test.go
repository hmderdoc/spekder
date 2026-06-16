package game

import "testing"

// Roster traits: poison/burn DoT, drain leech, passive HP regen, the turtle's
// shell mode, and the elephant's shield-spray cone.

func TestPoisonDoTAndKillCredit(t *testing.T) {
	w := twoTanks(t)
	// VENOM: an initial bite plus a 5 HP/sec drip for 4s, credited to the shooter.
	w.applyShotHit(&Projectile{owner: 0, eff: EffPoison, dmg: 6, mag: 5, dur: 4, affects: TargetFoes, cause: CausePoison}, 1)
	if w.Tanks[1].HP != 54 {
		t.Fatalf("bite: HP=%d want 54", w.Tanks[1].HP)
	}
	if w.Tanks[1].dotT != 4 || w.Tanks[1].dotPS != 5 {
		t.Fatalf("dot not set: t=%v ps=%v", w.Tanks[1].dotT, w.Tanks[1].dotPS)
	}
	for i := 0; i < 40; i++ { // 4 sim-seconds of drip
		w.stepDot(1, 0.1)
	}
	if got := w.Tanks[1].HP; got != 34 {
		t.Fatalf("after 4s of poison: HP=%d want 34", got)
	}
	// drip the victim to death: the poisoner gets the kill, labeled venom
	w.Tanks[1].HP = 2
	w.applyShotHit(&Projectile{owner: 0, eff: EffPoison, mag: 50, dur: 2, affects: TargetFoes, cause: CausePoison}, 1)
	for i := 0; i < 20 && !w.Tanks[1].Dead; i++ {
		w.stepDot(1, 0.1)
	}
	if !w.Tanks[1].Dead || w.Tanks[0].Kills != 1 {
		t.Fatalf("poison kill not credited: dead=%v kills=%d", w.Tanks[1].Dead, w.Tanks[0].Kills)
	}
	if len(w.kills) == 0 || w.kills[len(w.kills)-1].Cause != CausePoison {
		t.Fatalf("kill cause not venom: %+v", w.kills)
	}
}

func TestBleedDoT(t *testing.T) {
	w := twoTanks(t)
	// SCRATCH-style hit: small bite + a bleed that ticks for the duration.
	w.applyShotHit(&Projectile{owner: 0, eff: EffBleed, dmg: 12, mag: 6, dur: 4, affects: TargetFoes, cause: CauseBleed}, 1)
	if w.Tanks[1].HP != 48 {
		t.Fatalf("scratch bite: HP=%d want 48", w.Tanks[1].HP)
	}
	if w.Tanks[1].dotCause != CauseBleed || w.Tanks[1].dotLeech {
		t.Fatalf("bleed not set as a non-leeching CauseBleed DoT: cause=%d leech=%v", w.Tanks[1].dotCause, w.Tanks[1].dotLeech)
	}
	for i := 0; i < 40; i++ { // 4 sim-seconds of bleed at 6/s
		w.stepDot(1, 0.1)
	}
	if got := w.Tanks[1].HP; got != 24 {
		t.Fatalf("after the bleed: HP=%d want 24", got)
	}
}

func TestDrainLeechesToShooter(t *testing.T) {
	w := twoTanks(t)
	w.Tanks[0].HP = 50 // wounded breather
	w.applyShotHit(&Projectile{owner: 0, eff: EffDrain, mag: 4, dur: 3, affects: TargetFoes, cause: CauseFire}, 1)
	for i := 0; i < 30; i++ { // 3 sim-seconds of burn
		w.stepDot(1, 0.1)
	}
	if got := w.Tanks[1].HP; got != 48 {
		t.Fatalf("burned: HP=%d want 48", got)
	}
	if got := w.Tanks[0].HP; got != 62 {
		t.Fatalf("leech: shooter HP=%d want 62", got)
	}
}

func TestPassiveRegen(t *testing.T) {
	w := twoTanks(t)
	w.Tanks[0].body = BodySerpent
	w.Tanks[0].HP = 40
	for i := 0; i < 20; i++ { // 2 sim-seconds at 3 HP/sec
		w.stepRegen(&w.Tanks[0], 0.1)
	}
	if got := w.Tanks[0].HP; got != 46 {
		t.Fatalf("serpent regen: HP=%d want 46", got)
	}
	// taking damage pauses it
	w.hurt(0, 5, -1, CauseHazard)
	if w.Tanks[0].regenPause != regenHitPause {
		t.Fatalf("hit didn't pause regen: %v", w.Tanks[0].regenPause)
	}
	hp := w.Tanks[0].HP
	w.stepRegen(&w.Tanks[0], 1)
	if w.Tanks[0].HP != hp {
		t.Fatal("regen ticked while paused")
	}
	// the armored tank has no passive regen at all
	w.Tanks[1].body, w.Tanks[1].HP = BodyTank, 40
	w.stepRegen(&w.Tanks[1], 5)
	if w.Tanks[1].HP != 40 {
		t.Fatalf("tank regenerated: HP=%d", w.Tanks[1].HP)
	}
}

func TestTurtleShellMode(t *testing.T) {
	w := twoTanks(t)
	w.Tanks[0].body = BodyTurtle
	w.applyInput(0, Input{Fire2: true}, 0.05)
	if w.Tanks[0].shellT <= 0 {
		t.Fatal("B didn't shell up")
	}
	// shelled: immune to fire, and throttle doesn't move it
	if w.shotCanAffect(&Projectile{owner: 1, eff: EffDamage, affects: TargetFoes}, 0) {
		t.Fatal("shelled turtle is hittable")
	}
	x0, z0 := w.Tanks[0].Pos.X, w.Tanks[0].Pos.Z
	w.applyInput(0, Input{Throttle: true}, 0.5)
	if w.Tanks[0].Pos.X != x0 || w.Tanks[0].Pos.Z != z0 {
		t.Fatal("shelled turtle moved")
	}
	// B again (after the debounce) pops out and starts the recharge
	w.Tanks[0].cooldown2 = 0
	w.applyInput(0, Input{Fire2: true}, 0.05)
	if w.Tanks[0].shellT != 0 || w.Tanks[0].cooldown2 != shellRecharge {
		t.Fatalf("pop-out: shellT=%v cd2=%v want 0/%v", w.Tanks[0].shellT, w.Tanks[0].cooldown2, shellRecharge)
	}
}

func TestTurtleBotShellsUp(t *testing.T) {
	w := twoTanks(t)
	w.Tanks[0].body, w.Tanks[0].Bot = BodyTurtle, true
	w.Tanks[0].HP = 40 // well under half of the HEAVY chassis's 150
	w.botSpecial(0)    // enemy 5 units away (twoTanks): cornered
	if w.Tanks[0].shellT <= 0 {
		t.Fatal("hurt, cornered turtle bot didn't shell up")
	}
}

func TestElephantBuffer(t *testing.T) {
	w := twoTanks(t)
	e := &w.Tanks[0]
	e.body, e.HP = BodyElephant, 150
	e.bufferHP = elephantBufferMax

	// Damage soaks into the buffer first, from any direction (HP untouched).
	w.hurt(0, 40, 1, CauseCannon)
	if e.HP != 150 || e.bufferHP != elephantBufferMax-40 {
		t.Fatalf("buffer didn't soak: HP=%d buffer=%v", e.HP, e.bufferHP)
	}
	// Overflow past a near-empty buffer carries through to HP.
	e.bufferHP = 10
	w.hurt(0, 25, 1, CauseCannon)
	if e.bufferHP != 0 || e.HP != 135 { // 25 - 10 buffer = 15 to HP
		t.Fatalf("overflow wrong: buffer=%v HP=%d", e.bufferHP, e.HP)
	}
}

func TestHookReelsIn(t *testing.T) {
	w := twoTanks(t)
	w.Tanks[0].Pos = V3{}
	w.Tanks[1].Pos = V3{X: 0, Z: 15} // far away, dead ahead (+Z)
	w.applyShotHit(&Projectile{owner: 0, eff: EffPull, mag: hookPullDist, affects: TargetFoes}, 1)
	if z := w.Tanks[1].Pos.Z; z < hookPullDist-0.6 || z > hookPullDist+0.6 {
		t.Fatalf("hook didn't reel the target in: end Z=%v want ~%v", z, hookPullDist)
	}
}

func TestMinotaurBarrier(t *testing.T) {
	w := twoTanks(t)
	// Into the active phase first, so the countdown->active match reset doesn't
	// re-initialize the barrier state we set up below.
	drive(w, countdownTime+0.5, 1.0/30, map[int]Input{})
	m := &w.Tanks[0]
	m.body = BodyMinotaur
	m.HP, m.Pos, m.HullYaw = 150, V3{}, 0 // at origin, facing +Z
	m.shieldHP, m.shieldUp = minoShieldMax, true

	// A foe directly in front (+Z): a hit is soaked by the barrier, not the tank.
	w.Tanks[1].Pos = V3{X: 0, Z: 8}
	w.applyShotHit(&Projectile{owner: 1, eff: EffDamage, dmg: 40, affects: TargetFoes}, 0)
	if m.HP != 150 || m.shieldHP != minoShieldMax-40 {
		t.Fatalf("front hit not absorbed: HP=%d shield=%v", m.HP, m.shieldHP)
	}

	// A hit from behind ignores the barrier and wounds the tank.
	w.Tanks[1].Pos = V3{X: 0, Z: -8}
	hpBefore := m.HP
	w.applyShotHit(&Projectile{owner: 1, eff: EffDamage, dmg: 30, affects: TargetFoes}, 0)
	if m.HP != hpBefore-30 {
		t.Fatalf("back hit should bypass the barrier: HP=%d want %d", m.HP, hpBefore-30)
	}

	// Overkill from the front shatters the barrier and leaks the surplus.
	w.Tanks[1].Pos = V3{X: 0, Z: 8}
	m.shieldHP, hpBefore = 10, m.HP
	w.applyShotHit(&Projectile{owner: 1, eff: EffDamage, dmg: 25, affects: TargetFoes}, 0)
	if m.shieldUp || m.shieldHP != 0 || m.shieldBroken != minoShieldBreakCD {
		t.Fatalf("barrier should shatter: up=%v hp=%v cd=%v", m.shieldUp, m.shieldHP, m.shieldBroken)
	}
	if m.HP != hpBefore-15 { // 25 dmg - 10 absorbed = 15 leak
		t.Fatalf("shatter leak wrong: HP=%d want %d", m.HP, hpBefore-15)
	}

	// While shattered it can't redeploy, and HP regenerates only after the
	// cooldown elapses (no input drives the barrier down each tick).
	drive(w, 1, 1.0/30, map[int]Input{}) // 1s: still on cooldown, no regen yet
	if m.shieldHP != 0 {
		t.Fatalf("barrier regened during cooldown: %v", m.shieldHP)
	}
	drive(w, minoShieldBreakCD, 1.0/30, map[int]Input{}) // wait out the cooldown + regen
	if m.shieldBroken != 0 || m.shieldHP <= 0 {
		t.Fatalf("barrier didn't recover: cd=%v hp=%v", m.shieldBroken, m.shieldHP)
	}
}

func TestAegisConeShieldsAllies(t *testing.T) {
	saved := Maps
	t.Cleanup(func() { Maps = saved })
	Maps = []Map{{Name: "T", Size: 30}}
	w := &World{Mode: ModeCTF} // a team mode, so allies exist
	w.Tanks = []Tank{
		{HP: 150, Team: 0, Carrying: -1, body: BodyElephant},
		{HP: 80, Team: 0, Carrying: -1, Pos: V3{X: 0, Z: 4}}, // ally dead ahead
		{HP: 80, Team: 1, Carrying: -1, Pos: V3{X: 0, Z: 6}}, // foe behind the ally
	}
	p := Projectile{owner: 0, eff: EffShield, dur: 2.5, blast: 7, affects: TargetAllies}
	w.coneStrike(&p, V3{Z: 1})
	if w.Tanks[1].shieldT != 2.5 {
		t.Fatalf("ally not shielded: %v", w.Tanks[1].shieldT)
	}
	if w.Tanks[2].shieldT != 0 || w.Tanks[2].HP != 80 {
		t.Fatalf("foe affected by a support cone: shield=%v hp=%d", w.Tanks[2].shieldT, w.Tanks[2].HP)
	}
	if w.Tanks[0].shieldT != 1.5 { // self-coat at 60% duration
		t.Fatalf("no self-coat: %v", w.Tanks[0].shieldT)
	}
}
