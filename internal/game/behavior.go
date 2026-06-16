package game

import (
	"math/rand"
	"strings"
)

// Event-driven behaviors (see EVENTS.md). A map author wires Signals -> Conditions
// -> Actions on entities and on a map-level "logic" director, backed by a per-match
// integer blackboard (Vars). The engine is server-authoritative and runs in the
// normal tick; effects (spawns, stat changes, messages) replicate via the snapshot.

// Behavior is one author rule: when On fires and every When passes, run Do. Once
// limits it to a single firing per match (tracked out-of-band so templates stay
// immutable - see World.logicDone / Entity.bDone).
type Behavior struct {
	On   string      `json:"on"`
	When []Condition `json:"when,omitempty"`
	Do   []Action    `json:"do"`
	Once bool        `json:"once,omitempty"`
}

// Condition gates a behavior. Kind selects which fields matter:
//
//	var:    Var Op N        (blackboard compare)
//	hp:     Sel Op N        (entity HP percent; Sel = self or #tag)
//	count:  Sel Op N        (Sel = bots|players|alive)
//	chance: N               (percent 0..100)
type Condition struct {
	Kind string  `json:"kind"`
	Sel  string  `json:"sel,omitempty"`
	Var  string  `json:"var,omitempty"`
	Op   string  `json:"op,omitempty"`
	N    float64 `json:"n,omitempty"`
	R    float64 `json:"r,omitempty"` // radius for `near` (units around the owning entity)
}

// Action is one effect. Act selects which fields matter:
//
//	emit:     Sig [After]
//	setvar:   Var N      addvar: Var N
//	message:  Text
//	setstat:  Target Stat N      (Stat = weapon|dmg|firedelay|range|hp|maxhp)
//	spawn:    What [At] [Count]   (What = creature/"tank"; At = self or #tag)
//	enable|disable: Target
//	win|lose|nextwave
type Action struct {
	Act    string  `json:"act"`
	Sig    string  `json:"sig,omitempty"`
	After  float64 `json:"after,omitempty"`
	What   string  `json:"what,omitempty"`
	At     string  `json:"at,omitempty"`
	Count  int     `json:"count,omitempty"`
	Var    string  `json:"var,omitempty"`
	Target string  `json:"target,omitempty"`
	Stat   string  `json:"stat,omitempty"`
	Text   string  `json:"text,omitempty"`
	N      float64 `json:"n,omitempty"`
}

// Signal is a queued event: Source = emitting entity (-1 = world/director), Subject =
// the primary tank involved (victim/enterer/picker), Other = a secondary tank (the
// killer on `killed`); -1 when absent.
type Signal struct {
	Name                   string
	Source, Subject, Other int
}

type delayedSig struct {
	name                   string
	source, subject, other int
	t                      float64
}

// emit queues a signal for this tick's dispatch (no secondary tank).
func (w *World) emit(name string, source, subject int) {
	w.bus = append(w.bus, Signal{Name: name, Source: source, Subject: subject, Other: -1})
}

// Signal sources / behavior refs share one int space: an entity is its index, a
// tank is its index + tankSrcBase, and -1 is the world/director (broadcast). This
// lets `self` stay a plain int while distinguishing entities from tanks (mobile
// bosses / scripted actors).
const tankSrcBase = 1 << 20

func tankSrc(i int) int    { return i + tankSrcBase }
func isTankSrc(s int) bool { return s >= tankSrcBase }

// resolveRef maps a selector to an encoded ref (entity index, tankSrc(i), or -1).
func (w *World) resolveRef(sel string, self int, sig Signal) int {
	switch {
	case sel == "" || strings.EqualFold(sel, "self"):
		return self
	case strings.EqualFold(sel, "victim"), strings.EqualFold(sel, "subject"):
		if sig.Subject >= 0 {
			return tankSrc(sig.Subject)
		}
	case strings.EqualFold(sel, "killer"):
		if sig.Other >= 0 {
			return tankSrc(sig.Other)
		}
	case strings.HasPrefix(sel, "#"):
		tag := sel[1:]
		for i := range w.entities {
			if strings.EqualFold(w.entities[i].Tag, tag) {
				return i
			}
		}
	}
	return -1
}

func (w *World) refPos(ref int) (V3, bool) {
	switch {
	case ref < 0:
	case isTankSrc(ref):
		if i := ref - tankSrcBase; i < len(w.Tanks) {
			return w.Tanks[i].Pos, true
		}
	case ref < len(w.entities):
		return w.entities[ref].Pos, true
	}
	return V3{}, false
}

func (w *World) refHPpct(ref int) (float64, bool) {
	switch {
	case ref < 0:
	case isTankSrc(ref):
		if i := ref - tankSrcBase; i < len(w.Tanks) {
			max := w.Tanks[i].veh().MaxHP
			if max <= 0 {
				max = 1
			}
			return float64(w.Tanks[i].HP) / float64(max) * 100, true
		}
	case ref < len(w.entities):
		return w.entityHPpct(ref), true
	}
	return 0, false
}

// resetBehaviors re-seeds the blackboard + director rules from the active map and
// clears the bus/timers. Called from resetEntities at match start.
func (w *World) resetBehaviors() {
	m := w.ActiveMap()
	w.vars = map[string]int{}
	for k, v := range m.Vars {
		w.vars[k] = v
	}
	w.logic = m.Logic
	w.logicDone = make([]bool, len(m.Logic))
	w.bus, w.delayed, w.events, w.started = nil, nil, nil, false
}

// runBehaviors fires `start` once, advances delayed emits, then drains the signal
// bus (actions may emit more; a guard caps cascades). Called at the end of simulate.
const tickInterval = 0.5 // seconds between periodic `tick` signals

func (w *World) runBehaviors(dt float64) {
	if !w.started {
		w.started = true
		w.emit("start", -1, -1)
	}
	if w.tickT += dt; w.tickT >= tickInterval { // periodic tick (payload gating, polling)
		w.tickT -= tickInterval
		w.emit("tick", -1, -1)
	}
	if len(w.delayed) > 0 {
		keep := w.delayed[:0]
		for _, d := range w.delayed {
			if d.t -= dt; d.t <= 0 {
				w.bus = append(w.bus, Signal{Name: d.name, Source: d.source, Subject: d.subject, Other: d.other})
			} else {
				keep = append(keep, d)
			}
		}
		w.delayed = keep
	}
	for guard := 0; guard < 256 && len(w.bus) > 0; guard++ {
		sig := w.bus[0]
		w.bus = w.bus[1:]
		w.dispatch(sig)
	}
}

// dispatch routes a signal to the director and to entity behaviors. A signal with
// Source >= 0 is "self-scoped" (destroyed/hp_below): only that entity (and the
// director) see it. Source == -1 is a broadcast (start/killed/captured/picked/
// wave_cleared and all custom emits): every entity sees it.
func (w *World) dispatch(sig Signal) {
	for i := range w.logic {
		w.tryBehavior(&w.logic[i], &w.logicDone[i], -1, sig)
	}
	for ei := range w.entities {
		if sig.Source != -1 && sig.Source != ei {
			continue
		}
		e := &w.entities[ei]
		for bi := range e.Behaviors {
			w.tryBehavior(&e.Behaviors[bi], &e.bDone[bi], ei, sig)
		}
	}
	for ti := range w.Tanks { // mobile bosses / scripted actors carry behaviors too
		t := &w.Tanks[ti]
		if len(t.Behaviors) == 0 || t.gone {
			continue
		}
		if sig.Source != -1 && sig.Source != tankSrc(ti) {
			continue
		}
		for bi := range t.Behaviors {
			if bi < len(t.bDone) {
				w.tryBehavior(&t.Behaviors[bi], &t.bDone[bi], tankSrc(ti), sig)
			}
		}
	}
}

func (w *World) tryBehavior(b *Behavior, done *bool, self int, sig Signal) {
	if b.On != sig.Name || (b.Once && *done) {
		return
	}
	for i := range b.When {
		if !w.condOK(b.When[i], self, sig) {
			return
		}
	}
	if b.Once {
		*done = true
	}
	for i := range b.Do {
		w.doAction(b.Do[i], self, sig)
	}
}

func (w *World) condOK(c Condition, self int, sig Signal) bool {
	switch strings.ToLower(c.Kind) {
	case "var":
		return cmpOp(float64(w.vars[c.Var]), c.Op, c.N)
	case "hp":
		if pct, ok := w.refHPpct(w.resolveRef(c.Sel, self, sig)); ok {
			return cmpOp(pct, c.Op, c.N)
		}
		return false
	case "count":
		return cmpOp(float64(w.countSel(c.Sel)), c.Op, c.N)
	case "near": // count Sel-tanks within radius R of the owning actor
		return cmpOp(float64(w.countNear(c.Sel, self, c.R)), c.Op, c.N)
	case "side": // is the referenced tank a player or a bot? (Var = "player"|"bot")
		ref := w.resolveRef(c.Sel, self, sig)
		if isTankSrc(ref) {
			if i := ref - tankSrcBase; i < len(w.Tanks) {
				return w.Tanks[i].Bot == strings.EqualFold(c.Var, "bot")
			}
		}
		return false
	case "chance":
		return rand.Float64()*100 < c.N
	}
	return false
}

// countNear counts live tanks matching sel within radius r of the owning actor.
func (w *World) countNear(sel string, self int, r float64) int {
	c0, ok := w.refPos(self)
	if !ok || r <= 0 {
		return 0
	}
	n := 0
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || t.Dead {
			continue
		}
		switch strings.ToLower(sel) {
		case "bots", "enemies":
			if !t.Bot {
				continue
			}
		case "players", "humans":
			if t.Bot {
				continue
			}
		}
		dx, dz := t.Pos.X-c0.X, t.Pos.Z-c0.Z
		if dx*dx+dz*dz <= r*r {
			n++
		}
	}
	return n
}

func (w *World) doAction(a Action, self int, sig Signal) {
	switch strings.ToLower(a.Act) {
	case "emit":
		// Custom signals broadcast (source -1) so any entity can subscribe; the
		// triggering subject + killer ride along so chains keep context.
		if a.After > 0 {
			w.delayed = append(w.delayed, delayedSig{a.Sig, -1, sig.Subject, sig.Other, a.After})
		} else {
			w.bus = append(w.bus, Signal{Name: a.Sig, Source: -1, Subject: sig.Subject, Other: sig.Other})
		}
	case "setvar":
		w.vars[a.Var] = int(a.N)
	case "addvar":
		w.vars[a.Var] += int(a.N)
	case "message":
		if a.Text != "" {
			w.events = append(w.events, a.Text)
		}
	case "setstat":
		w.applySetStat(a, self, sig)
	case "spawn":
		w.applySpawn(a, self, sig)
	case "enable", "disable":
		if i := w.resolveEntity(a.Target, self); i >= 0 {
			w.entities[i].Dead = strings.EqualFold(a.Act, "disable")
		}
	case "move": // start/continue an entity along a path (What = path name, N = speed)
		if i := w.resolveEntity(a.Target, self); i >= 0 {
			e := &w.entities[i]
			e.mvPath, e.mvOn = a.What, true
			if a.N > 0 {
				e.mvSpeed = a.N
			} else if e.mvSpeed == 0 {
				e.mvSpeed = 3
			}
		}
	case "stop":
		if i := w.resolveEntity(a.Target, self); i >= 0 {
			w.entities[i].mvOn = false
		}
	case "damage": // hurt a tank ref (N = amount); credit to self if self is a tank
		if ref := w.resolveRef(a.Target, self, sig); isTankSrc(ref) {
			if ti := ref - tankSrcBase; ti < len(w.Tanks) && !w.Tanks[ti].Dead && !w.Tanks[ti].gone {
				owner := -1
				if isTankSrc(self) {
					owner = self - tankSrcBase
				}
				w.hurt(ti, int(a.N), owner, CauseHazard)
			}
		}
	case "heal": // restore HP to a tank ref (N = amount), capped at its max
		if ref := w.resolveRef(a.Target, self, sig); isTankSrc(ref) {
			if ti := ref - tankSrcBase; ti < len(w.Tanks) && !w.Tanks[ti].Dead {
				t := &w.Tanks[ti]
				if max := t.veh().MaxHP; t.HP < max {
					if t.HP += int(a.N); t.HP > max {
						t.HP = max
					}
				}
			}
		}
	case "win":
		w.endByBehavior(true)
	case "lose":
		w.endByBehavior(false)
	case "nextwave":
		if w.rules().Bots == BotWaves {
			w.spawnWave()
		}
	}
}

// ---- helpers ----

func cmpOp(a float64, op string, b float64) bool {
	switch op {
	case "==", "=", "":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

// resolveEntity maps a selector to an entity index: ""/"self" = the owning entity,
// "#tag" = the first entity with that tag. -1 if unresolved.
func (w *World) resolveEntity(sel string, self int) int {
	if sel == "" || strings.EqualFold(sel, "self") {
		return self
	}
	if strings.HasPrefix(sel, "#") {
		tag := sel[1:]
		for i := range w.entities {
			if strings.EqualFold(w.entities[i].Tag, tag) {
				return i
			}
		}
	}
	return -1
}

func (w *World) entityHPpct(i int) float64 {
	e := &w.entities[i]
	max := 1
	if e.Destruct != nil && e.Destruct.MaxHP > 0 {
		max = e.Destruct.MaxHP
	}
	return float64(e.HP) / float64(max) * 100
}

func (w *World) countSel(sel string) int {
	n := 0
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || t.Dead {
			continue
		}
		switch strings.ToLower(sel) {
		case "bots", "enemies":
			if t.Bot {
				n++
			}
		case "players", "humans":
			if !t.Bot {
				n++
			}
		default: // "alive" / anything
			n++
		}
	}
	return n
}

func (w *World) applySetStat(a Action, self int, sig Signal) {
	ref := w.resolveRef(a.Target, self, sig)
	if ref < 0 {
		return
	}
	stat := strings.ToLower(a.Stat)
	if isTankSrc(ref) { // a tank (mobile boss): tune live / custom stats
		t := &w.Tanks[ref-tankSrcBase]
		switch stat {
		case "weapon":
			t.weapon2 = int(a.N)
		case "hp":
			t.HP = int(a.N)
		case "speed", "firedelay", "maxhp", "turn", "hullturn":
			if t.custom == nil {
				v := t.veh()
				t.custom = &v
			}
			switch stat {
			case "speed":
				t.custom.Speed = a.N
			case "firedelay":
				t.custom.FireDelay = a.N
			case "turn", "hullturn":
				t.custom.HullTurn = a.N
			case "maxhp":
				t.custom.MaxHP = int(a.N)
			}
		}
		return
	}
	e := &w.entities[ref] // NewEntities clones trait pointers, so this is match-local
	switch stat {
	case "weapon":
		e.Weapon = int(a.N)
	case "dmg":
		if e.Turret != nil {
			e.Turret.Dmg = int(a.N)
		}
	case "firedelay":
		if e.Turret != nil {
			e.Turret.FireDelay = a.N
		}
	case "range":
		if e.Turret != nil {
			e.Turret.Range = a.N
		}
	case "hp":
		e.HP = int(a.N)
	case "maxhp":
		if e.Destruct != nil {
			e.Destruct.MaxHP = int(a.N)
		}
	}
}

func (w *World) applySpawn(a Action, self int, sig Signal) {
	pos, _ := w.refPos(self)
	if p, ok := w.refPos(w.resolveRef(a.At, self, sig)); ok {
		pos = p
	}
	n := a.Count
	if n <= 0 {
		n = 1
	}
	if strings.HasPrefix(a.What, "@") { // "@name" = a scripted actor (mobile boss)
		for k := 0; k < n; k++ {
			off := V3{X: (rand.Float64()*2 - 1) * 2.5, Z: (rand.Float64()*2 - 1) * 2.5}
			w.spawnActor(a.What[1:], pos.Add(off))
		}
		return
	}
	veh, body := spawnArchetype(a.What)
	for k := 0; k < n; k++ {
		off := V3{X: (rand.Float64()*2 - 1) * 2.5, Z: (rand.Float64()*2 - 1) * 2.5}
		w.spawnBot(pos.Add(off), veh, body)
	}
}

// spawnActor instantiates a named actor template as a behavior-carrying bot tank.
func (w *World) spawnActor(name string, pos V3) {
	actors := w.ActiveMap().Actors
	for i := range actors {
		if !strings.EqualFold(actors[i].Name, name) {
			continue
		}
		ac := actors[i]
		slot := -1
		for j := range w.Tanks {
			if w.Tanks[j].gone {
				slot = j
				break
			}
		}
		nidx := len(w.Tanks)
		t := Tank{
			Bot: true, body: ac.Body,
			guard: spawnGuardTime, vote: -1, Color: BotPalette[nidx%len(BotPalette)], Name: botName(nidx),
			Team: -1, Carrying: -1, weapon2: wepGrenade, Pos: pos,
			Behaviors: ac.Behaviors, Watch: ac.Watch,
			bDone: make([]bool, len(ac.Behaviors)), wHit: make([]bool, len(ac.Watch)),
		}
		// A scripted actor carries an authored chassis (stats) that may intentionally
		// differ from its body, so pin it as a stat override: the 5 chassis live on
		// as an authoring stat-preset palette even though the player roster is now
		// per-character.
		v := veh(ac.Vehicle)
		if ac.MaxHP > 0 { // a tougher boss: a custom stat block sets its HP
			v.MaxHP = ac.MaxHP
		}
		t.custom, t.HP, t.ammo = &v, v.MaxHP, v.AmmoMax
		if slot >= 0 {
			w.Tanks[slot] = t
			w.rollBotAI(slot)
		} else {
			w.Tanks = append(w.Tanks, t)
			w.rollBotAI(len(w.Tanks) - 1)
		}
		return
	}
}

// stepTankWatch emits hp_below for behavior-carrying tanks (mobile bosses) as they
// cross their Watch thresholds.
func (w *World) stepTankWatch() {
	for i := range w.Tanks {
		t := &w.Tanks[i]
		if t.gone || t.Dead || len(t.Watch) == 0 {
			continue
		}
		max := t.veh().MaxHP
		if max <= 0 {
			max = 1
		}
		pct := float64(t.HP) / float64(max) * 100
		for k, thr := range t.Watch {
			if k < len(t.wHit) && !t.wHit[k] && pct <= thr {
				t.wHit[k] = true
				w.emit("hp_below", tankSrc(i), -1)
			}
		}
	}
}

// spawnArchetype maps an author name to a (chassis, body) pair for spawned bots.
func spawnArchetype(name string) (vehicle, body int) {
	switch strings.ToLower(name) {
	case "spider":
		return 0, BodySpider
	case "insect":
		return 4, BodyInsect
	case "scorpion":
		return 1, BodyScorpion
	case "serpent":
		return 0, BodySerpent
	case "quadruped", "quad":
		return 3, BodyQuad
	case "humanoid":
		return 1, BodyHumanoid
	case "tripod":
		return 2, BodyTripod
	case "drone":
		return 3, BodyDrone
	case "crab":
		return 2, BodyCrab
	case "octopod":
		return 4, BodyOctopod
	default: // "tank" / unknown: a random chassis, no creature body
		return rand.Intn(len(Vehicles)), BodyTank
	}
}

// spawnBot adds a bot tank at pos (reusing a vacated slot when possible).
func (w *World) spawnBot(pos V3, vehicle, body int) {
	slot := -1
	for j := range w.Tanks {
		if w.Tanks[j].gone {
			slot = j
			break
		}
	}
	n := len(w.Tanks)
	v := veh(vehicle) // authored archetype chassis (stats), pinned as a per-tank override
	t := Tank{
		Bot: true, body: body, custom: &v, HP: v.MaxHP, ammo: v.AmmoMax,
		guard: spawnGuardTime, vote: -1, Color: BotPalette[n%len(BotPalette)], Name: botName(n),
		Team: -1, Carrying: -1, weapon2: wepGrenade, Pos: pos,
	}
	if slot >= 0 {
		w.Tanks[slot] = t
		w.rollBotAI(slot)
	} else {
		w.Tanks = append(w.Tanks, t)
		w.rollBotAI(len(w.Tanks) - 1)
	}
}

// endByBehavior forces the match to end (a behavior's win/lose). playersWin awards
// the first human; otherwise no winner.
func (w *World) endByBehavior(playersWin bool) {
	w.Phase, w.Timer, w.Shots = PhaseEnded, endTime, nil
	w.WinnerID = -1
	if playersWin {
		for i := range w.Tanks {
			if !w.Tanks[i].Bot && !w.Tanks[i].gone {
				w.WinnerID = i
				break
			}
		}
	}
}
