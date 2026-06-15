package game

import "testing"

// drain runs the queued bus to completion (what runBehaviors does, minus start/dt).
func (w *World) drain() {
	for guard := 0; guard < 256 && len(w.bus) > 0; guard++ {
		s := w.bus[0]
		w.bus = w.bus[1:]
		w.dispatch(s)
	}
}

// TestBehaviorBossPhases: each hp_below crossing advances one phase. Rules gate on
// the boss's actual HP percent + Once (HP doesn't mutate mid-dispatch, so they
// don't cascade) - the recommended boss pattern.
func TestBehaviorBossPhases(t *testing.T) {
	w := &World{vars: map[string]int{}}
	boss := Entity{
		Tag: "boss", Destruct: &DestructTrait{MaxHP: 100}, HP: 60, // 60% -> below the 66 mark
		Behaviors: []Behavior{
			{On: "hp_below", When: []Condition{{Kind: "hp", Sel: "self", Op: "<=", N: 66}},
				Do: []Action{{Act: "message", Text: "PHASE 2"}, {Act: "spawn", What: "spider", Count: 2}}, Once: true},
			{On: "hp_below", When: []Condition{{Kind: "hp", Sel: "self", Op: "<=", N: 33}},
				Do: []Action{{Act: "message", Text: "PHASE 3"}}, Once: true},
		},
		bDone: make([]bool, 2),
	}
	w.entities = []Entity{boss}

	w.emit("hp_below", 0, -1) // first threshold (66): only the phase-2 rule
	w.drain()
	if len(w.events) != 1 || w.events[0] != "PHASE 2" {
		t.Fatalf("first threshold should announce only PHASE 2: %v", w.events)
	}
	spiders := 0
	for i := range w.Tanks {
		if w.Tanks[i].Bot && w.Tanks[i].body == BodySpider {
			spiders++
		}
	}
	if spiders != 2 {
		t.Fatalf("expected 2 spawned spiders, got %d", spiders)
	}

	w.events = nil
	w.entities[0].HP = 20 // drop below the 33 mark
	w.emit("hp_below", 0, -1)
	w.drain()
	if len(w.events) != 1 || w.events[0] != "PHASE 3" {
		t.Fatalf("second threshold should announce only PHASE 3 (no re-fire): %v", w.events)
	}
}

// TestBehaviorEmitChain: a custom emit chains across director rules (state machine).
func TestBehaviorEmitChain(t *testing.T) {
	w := &World{vars: map[string]int{}}
	w.logic = []Behavior{
		{On: "start", Do: []Action{{Act: "emit", Sig: "phase2"}}},
		{On: "phase2", Do: []Action{{Act: "setvar", Var: "x", N: 5}}},
	}
	w.logicDone = make([]bool, 2)
	w.emit("start", -1, -1)
	w.drain()
	if w.vars["x"] != 5 {
		t.Fatalf("emit chain failed; x = %d", w.vars["x"])
	}
}

// TestBehaviorSelfScope: a self-scoped signal (Source = entity) only reaches that
// entity, not its neighbors; a broadcast (Source -1) reaches all.
func TestBehaviorSelfScope(t *testing.T) {
	w := &World{vars: map[string]int{}}
	mk := func() Entity {
		return Entity{Behaviors: []Behavior{{On: "destroyed", Do: []Action{{Act: "addvar", Var: "hits", N: 1}}}}, bDone: make([]bool, 1)}
	}
	w.entities = []Entity{mk(), mk()}
	w.emit("destroyed", 1, -1) // only entity 1
	w.drain()
	if w.vars["hits"] != 1 {
		t.Fatalf("self-scoped signal should hit one entity, got %d", w.vars["hits"])
	}
	w.emit("killed", -1, -1) // broadcast: no entity listens, but must not panic
	w.drain()
}

// TestBehaviorJSONRoundTrip: vars/logic + entity tag/watch/behaviors survive the
// schema-v4 JSON encode/decode.
func TestBehaviorJSONRoundTrip(t *testing.T) {
	m := Map{
		Name: "T", Version: 4, Vars: map[string]int{"phase": 1},
		Logic: []Behavior{{On: "start", Do: []Action{{Act: "message", Text: "go"}}}},
		Entities: []Entity{{
			Kind: "turret", Tag: "boss", Watch: []float64{50, 25},
			Destruct: &DestructTrait{MaxHP: 100}, Turret: &TurretTrait{Range: 10},
			Behaviors: []Behavior{{On: "hp_below",
				When: []Condition{{Kind: "var", Var: "phase", Op: "==", N: 1}},
				Do:   []Action{{Act: "setvar", Var: "phase", N: 2}}, Once: true}},
		}},
	}
	data, err := MapJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMapJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Vars["phase"] != 1 || len(got.Logic) != 1 || got.Logic[0].On != "start" {
		t.Fatalf("map vars/logic lost: %+v / %+v", got.Vars, got.Logic)
	}
	e := got.Entities[0]
	if e.Tag != "boss" || len(e.Watch) != 2 || len(e.Behaviors) != 1 {
		t.Fatalf("entity behavior fields lost: %+v", e)
	}
	b := e.Behaviors[0]
	if b.On != "hp_below" || !b.Once || len(b.When) != 1 || b.Do[0].Act != "setvar" {
		t.Fatalf("behavior detail lost: %+v", b)
	}
}

// TestWardenMapLoads: the hand-authored boss map embeds, parses to v4, and has a
// tagged boss with watch thresholds + behaviors, with no fatal validation issues.
func TestWardenMapLoads(t *testing.T) {
	idx := FindMap("WARDEN")
	if idx < 0 {
		t.Fatal("WARDEN map not embedded")
	}
	m := Maps[idx]
	if m.Version != 4 || len(m.Logic) == 0 {
		t.Fatalf("version=%d logic=%d", m.Version, len(m.Logic))
	}
	var boss *Entity
	for i := range m.Entities {
		if m.Entities[i].Tag == "boss" {
			boss = &m.Entities[i]
		}
	}
	if boss == nil {
		t.Fatal("no boss entity (tag=boss)")
	}
	if len(boss.Behaviors) != 3 || len(boss.Watch) != 2 {
		t.Fatalf("boss behaviors=%d watch=%d", len(boss.Behaviors), len(boss.Watch))
	}
	for _, is := range ValidateMap(m) {
		if is.Fatal {
			t.Fatalf("fatal map issue: %+v", is)
		}
	}
}

// TestPathAt: walking a polyline by cumulative distance, and end detection.
func TestPathAt(t *testing.T) {
	pts := []V3{{X: 0}, {X: 10}, {X: 10, Z: 10}}
	if p, done := pathAt(pts, 5); done || p.X != 5 {
		t.Fatalf("mid first segment: %+v done=%v", p, done)
	}
	if p, done := pathAt(pts, 15); done || p.X != 10 || p.Z != 5 {
		t.Fatalf("mid second segment: %+v done=%v", p, done)
	}
	if _, done := pathAt(pts, 100); !done {
		t.Fatal("past end should be done")
	}
}

// TestTriggerEnterExit: a tank crossing a trigger footprint emits entered then
// exited (self-scoped), firing the trigger's behaviors.
func TestTriggerEnterExit(t *testing.T) {
	w := &World{vars: map[string]int{}}
	w.entities = []Entity{{
		Kind: "trigger", Pos: V3{}, Half: V3{X: 2, Y: 1, Z: 2}, Trigger: &TriggerTrait{},
		Behaviors: []Behavior{
			{On: "entered", Do: []Action{{Act: "addvar", Var: "in", N: 1}}},
			{On: "exited", Do: []Action{{Act: "addvar", Var: "out", N: 1}}},
		},
		bDone: make([]bool, 2),
	}}
	w.Tanks = []Tank{{Pos: V3{X: 0, Z: 0}}} // inside
	w.stepTrigger(&w.entities[0], 0)
	w.drain()
	if w.vars["in"] != 1 || w.vars["out"] != 0 {
		t.Fatalf("enter: in=%d out=%d", w.vars["in"], w.vars["out"])
	}
	w.stepTrigger(&w.entities[0], 0) // still inside: no repeat
	w.drain()
	if w.vars["in"] != 1 {
		t.Fatalf("no re-enter expected: in=%d", w.vars["in"])
	}
	w.Tanks[0].Pos = V3{X: 10} // leave
	w.stepTrigger(&w.entities[0], 0)
	w.drain()
	if w.vars["out"] != 1 {
		t.Fatalf("exit not fired: out=%d", w.vars["out"])
	}
}

// TestNearCondition: counts tanks within radius of the owning entity.
func TestNearCondition(t *testing.T) {
	w := &World{}
	w.entities = []Entity{{Pos: V3{X: 0, Z: 0}}}
	w.Tanks = []Tank{
		{Pos: V3{X: 2, Z: 0}},            // player, within 5
		{Pos: V3{X: 20, Z: 0}},           // player, far
		{Pos: V3{X: 1, Z: 1}, Bot: true}, // bot, within 5
	}
	if !w.condOK(Condition{Kind: "near", Sel: "players", R: 5, Op: ">=", N: 1}, 0, Signal{}) {
		t.Fatal("expected a player within 5")
	}
	if w.condOK(Condition{Kind: "near", Sel: "players", R: 5, Op: ">=", N: 2}, 0, Signal{}) {
		t.Fatal("only one player is within 5")
	}
	if !w.condOK(Condition{Kind: "near", Sel: "bots", R: 5, Op: "==", N: 1}, 0, Signal{}) {
		t.Fatal("expected one bot within 5")
	}
}

// TestEscortMapLoads: the payload demo embeds, parses to v4 with a path + a cart.
func TestEscortMapLoads(t *testing.T) {
	idx := FindMap("ESCORT")
	if idx < 0 {
		t.Fatal("ESCORT map not embedded")
	}
	m := Maps[idx]
	if len(m.Paths) != 1 || len(m.Paths[0].Points) < 2 {
		t.Fatalf("escort path missing: %+v", m.Paths)
	}
	var cart *Entity
	for i := range m.Entities {
		if m.Entities[i].Tag == "cart" {
			cart = &m.Entities[i]
		}
	}
	if cart == nil || len(cart.Behaviors) == 0 {
		t.Fatal("cart entity with behaviors missing")
	}
	for _, is := range ValidateMap(m) {
		if is.Fatal {
			t.Fatalf("fatal map issue: %+v", is)
		}
	}
}

// TestActorBehaviorsDispatch: a behavior-carrying tank (mobile boss) fires on its
// own self-scoped hp_below, and entity-scoped signals don't leak to it.
func TestActorBehaviorsDispatch(t *testing.T) {
	w := &World{vars: map[string]int{}}
	w.Tanks = []Tank{{
		Bot: true, HP: 20, // 20/70 = ~28% of a SCOUT (vehicle 0)
		Behaviors: []Behavior{{On: "hp_below",
			When: []Condition{{Kind: "hp", Sel: "self", Op: "<=", N: 50}},
			Do:   []Action{{Act: "addvar", Var: "raged", N: 1}}, Once: true}},
		bDone: make([]bool, 1),
	}}
	w.emit("hp_below", tankSrc(0), -1)
	w.drain()
	if w.vars["raged"] != 1 {
		t.Fatalf("tank behavior should fire on its hp_below: raged=%d", w.vars["raged"])
	}
	// An entity-scoped signal (small source) must not reach the tank.
	w.vars["raged"] = 0
	w.Tanks[0].bDone[0] = false
	w.emit("hp_below", 0, -1) // entity index 0 (none exist) - not the tank
	w.drain()
	if w.vars["raged"] != 0 {
		t.Fatalf("entity-scoped signal leaked to a tank")
	}
}

// TestStalkerMapLoads: the mobile-boss demo embeds with an actor template + director.
func TestStalkerMapLoads(t *testing.T) {
	idx := FindMap("STALKER")
	if idx < 0 {
		t.Fatal("STALKER map not embedded")
	}
	m := Maps[idx]
	if len(m.Actors) != 1 || m.Actors[0].Name != "stalker" || len(m.Actors[0].Behaviors) == 0 {
		t.Fatalf("stalker actor template missing: %+v", m.Actors)
	}
	if len(m.Logic) == 0 {
		t.Fatal("director should spawn the stalker on start")
	}
	for _, is := range ValidateMap(m) {
		if is.Fatal {
			t.Fatalf("fatal map issue: %+v", is)
		}
	}

	// Spawning the actor produces a behavior-carrying bot tank.
	w := &World{MapIdx: idx}
	w.spawnActor("stalker", V3{})
	if len(w.Tanks) != 1 || len(w.Tanks[0].Behaviors) == 0 || w.Tanks[0].HP != 420 {
		t.Fatalf("spawned actor wrong: tanks=%d hp=%d", len(w.Tanks), func() int {
			if len(w.Tanks) > 0 {
				return w.Tanks[0].HP
			}
			return -1
		}())
	}
}

// TestKillerSideDamageHeal: the killer selector, the side condition, and the
// damage/heal actions resolving tank refs.
func TestKillerSideDamageHeal(t *testing.T) {
	w := &World{vars: map[string]int{}}
	w.Tanks = []Tank{{Bot: true, HP: 40}, {Bot: false, HP: 0}} // 0 killer(bot,wounded), 1 victim(player,dead)
	w.logic = []Behavior{
		{On: "killed", When: []Condition{{Kind: "side", Sel: "victim", Var: "player"}},
			Do: []Action{{Act: "addvar", Var: "pk", N: 1}, {Act: "heal", Target: "killer", N: 30}}},
		{On: "hurtkiller", Do: []Action{{Act: "damage", Target: "killer", N: 10}}},
	}
	w.logicDone = make([]bool, 2)
	w.bus = append(w.bus, Signal{Name: "killed", Source: -1, Subject: 1, Other: 0})
	w.drain()
	if w.vars["pk"] != 1 {
		t.Fatalf("side(victim,player) should pass: pk=%d", w.vars["pk"])
	}
	if w.Tanks[0].HP != 70 { // 40 + 30, capped at SCOUT max 70
		t.Fatalf("heal killer: HP=%d want 70", w.Tanks[0].HP)
	}
	w.bus = append(w.bus, Signal{Name: "hurtkiller", Source: -1, Subject: -1, Other: 0})
	w.drain()
	if w.Tanks[0].HP != 60 {
		t.Fatalf("damage killer: HP=%d want 60", w.Tanks[0].HP)
	}
	// side mismatch: a bot victim should NOT pass side(victim,player)
	w.vars["pk"] = 0
	w.Tanks[1].Bot = true
	w.bus = append(w.bus, Signal{Name: "killed", Source: -1, Subject: 1, Other: 0})
	w.drain()
	if w.vars["pk"] != 0 {
		t.Fatalf("side(victim,player) must fail for a bot victim: pk=%d", w.vars["pk"])
	}
}
