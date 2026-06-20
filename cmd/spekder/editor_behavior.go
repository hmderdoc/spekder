package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	gm "spekder/internal/game"
)

// In-editor authoring for the event system (docs/EVENTS.md Phase 2). A set of small
// modal screens that build Behaviors (signal -> conditions -> actions) on an entity
// or the map's director, so authors never touch JSON. Most fields are chosen from a
// fixed vocabulary; only names/messages are typed.

var (
	sigVocab    = []string{"start", "tick", "killed", "destroyed", "captured", "picked", "wave_cleared", "hp_below", "entered", "exited", "arrived", "(custom...)"}
	condKindVoc = []string{"var", "hp", "count", "near", "side", "chance"}
	opVocab     = []string{"==", "!=", "<", "<=", ">", ">="}
	actVocab    = []string{"message", "setvar", "addvar", "setstat", "spawn", "move", "stop", "damage", "heal", "emit", "enable", "disable", "win", "lose", "nextwave"}
	sideVocab   = []string{"player", "bot"}
	statVocab   = []string{"weapon", "dmg", "firedelay", "range", "hp", "maxhp"}
	archVocab   = []string{"tank", "spider", "insect", "scorpion", "serpent", "quadruped", "humanoid", "tripod", "drone", "crab", "octopod"}
	countSelVoc = []string{"bots", "players", "alive"}
)

func indexOf(v []string, s string) int {
	for i, x := range v {
		if x == s {
			return i
		}
	}
	return 0
}
func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
func orElse(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
func numLabel(name string, v float64) string { return fmt.Sprintf("%s: %g", name, v) }

func watchStr(ws []float64) string {
	parts := make([]string, len(ws))
	for i, v := range ws {
		parts[i] = strconv.FormatFloat(v, 'g', -1, 64)
	}
	return strings.Join(parts, ",")
}
func parseFloats(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if v, err := strconv.ParseFloat(p, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// pickFromList is a modal vertical menu. Returns the chosen index, or ok=false if
// the user backed out (Backspace). Nav keys (incl. w/s from letter keys) move; the
// runes those letters also push are harmless here and drained when a field opens.
func pickFromList(w *bufio.Writer, cols, rows int, ip *input, title string, opts []string, cur int) (int, bool) {
	if cur < 0 || cur >= len(opts) {
		cur = 0
	}
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		fmt.Fprintf(w, "\x1b[2;3H\x1b[1;96m%s\x1b[0m", clip(title, cols-4))
		for i, o := range opts {
			style, mark := "\x1b[0;36m", "  "
			if i == cur {
				style, mark = "\x1b[1;30;46m", "> "
			}
			fmt.Fprintf(w, "\x1b[%d;3H%s %s%-28s \x1b[0m", 4+i, style, mark, clip(o, cols-8))
		}
		fmt.Fprintf(w, "\x1b[%d;3H\x1b[0;90mup/dn  ENTER select  Bksp back\x1b[0m", rows-1)
		w.Flush()
	}
	draw()
	for {
		select {
		case <-ip.quitCh:
			return cur, false
		case k := <-ip.events:
			switch k {
			case mkUp:
				cur = (cur - 1 + len(opts)) % len(opts)
				draw()
			case mkDown:
				cur = (cur + 1) % len(opts)
				draw()
			case mkEnter:
				return cur, true
			case mkBack:
				return cur, false
			}
		}
	}
}

// textEntry is a modal text field. Letter keys push BOTH a rune and a nav event, so
// we consume runes for the text and ignore nav events (only ENTER=ok, Bksp=delete).
func textEntry(w *bufio.Writer, cols, rows int, ip *input, prompt, cur string) (string, bool) {
	ip.drainRunes()
	s := cur
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		fmt.Fprintf(w, "\x1b[2;3H\x1b[1;96m%s\x1b[0m", clip(prompt, cols-4))
		fmt.Fprintf(w, "\x1b[4;3H\x1b[1;37m%s\x1b[0m\x1b[7m \x1b[0m\x1b[K", clip(s, cols-6))
		fmt.Fprintf(w, "\x1b[%d;3H\x1b[0;90mtype  ENTER ok  Bksp delete/back\x1b[0m", rows-1)
		w.Flush()
	}
	draw()
	for {
		select {
		case <-ip.quitCh:
			return cur, false
		case r := <-ip.runes:
			if r >= 0x20 && r < 0x7f && len(s) < 48 {
				s += string(r)
				draw()
			}
		case k := <-ip.events:
			switch k {
			case mkEnter:
				return s, true
			case mkBack:
				if len(s) > 0 {
					s = s[:len(s)-1]
					draw()
				} else {
					return cur, false // empty + Bksp = cancel
				}
			}
		}
	}
}

func numEntry(w *bufio.Writer, cols, rows int, ip *input, prompt string, cur float64) float64 {
	s, ok := textEntry(w, cols, rows, ip, prompt, strconv.FormatFloat(cur, 'g', -1, 64))
	if !ok {
		return cur
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return v
	}
	return cur
}

// --- summaries (one-line previews in the lists) ---

func behaviorSummary(b gm.Behavior) string {
	once := ""
	if b.Once {
		once = " once"
	}
	return fmt.Sprintf("on %-12s %dw %dd%s", b.On, len(b.When), len(b.Do), once)
}
func condSummary(c gm.Condition) string {
	switch c.Kind {
	case "var":
		return fmt.Sprintf("var %s %s %g", c.Var, c.Op, c.N)
	case "hp":
		return fmt.Sprintf("hp %s %s %g", orElse(c.Sel, "self"), c.Op, c.N)
	case "count":
		return fmt.Sprintf("count %s %s %g", c.Sel, c.Op, c.N)
	case "near":
		return fmt.Sprintf("near %s r%g %s %g", c.Sel, c.R, c.Op, c.N)
	case "side":
		return fmt.Sprintf("side %s is %s", orElse(c.Sel, "self"), orElse(c.Var, "player"))
	case "chance":
		return fmt.Sprintf("chance %g%%", c.N)
	}
	return c.Kind
}
func actSummary(a gm.Action) string {
	switch a.Act {
	case "message":
		return fmt.Sprintf("message %q", clip(a.Text, 20))
	case "setvar", "addvar":
		return fmt.Sprintf("%s %s=%g", a.Act, a.Var, a.N)
	case "setstat":
		return fmt.Sprintf("setstat %s %s=%g", orElse(a.Target, "self"), a.Stat, a.N)
	case "spawn":
		return fmt.Sprintf("spawn %s x%d @%s", a.What, max1(a.Count), orElse(a.At, "self"))
	case "move":
		return fmt.Sprintf("move %s along %s @%g", orElse(a.Target, "self"), orElse(a.What, "main"), a.N)
	case "stop":
		return "stop " + orElse(a.Target, "self")
	case "damage", "heal":
		return fmt.Sprintf("%s %s %g", a.Act, orElse(a.Target, "self"), a.N)
	case "emit":
		if a.After > 0 {
			return fmt.Sprintf("emit %s +%gs", a.Sig, a.After)
		}
		return "emit " + a.Sig
	case "enable", "disable":
		return a.Act + " " + a.Target
	}
	return a.Act
}
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// runBehaviorEditor edits a behavior list on an entity (ent != nil; also exposes
// its Tag + Watch) or the map's director (ent == nil, list = logic).
func runBehaviorEditor(w *bufio.Writer, cols, rows int, ip *input, ent *gm.Entity, logic *[]gm.Behavior, title string) {
	list := logic
	if ent != nil {
		list = &ent.Behaviors
	}
	text := func(p, cur string) string {
		if s, ok := textEntry(w, cols, rows, ip, p, cur); ok {
			return s
		}
		return cur
	}
	pickSig := func(cur string) (string, bool) {
		i, ok := pickFromList(w, cols, rows, ip, "SIGNAL (on)", sigVocab, indexOf(sigVocab, cur))
		if !ok {
			return cur, false
		}
		if sigVocab[i] == "(custom...)" {
			s, ok := textEntry(w, cols, rows, ip, "CUSTOM SIGNAL NAME", "")
			return s, ok && s != ""
		}
		return sigVocab[i], true
	}
	sel := 0
	for {
		var items []string
		base := 0
		if ent != nil {
			items = append(items, "TAG: "+orElse(ent.Tag, "(none)"), "WATCH%: "+orElse(watchStr(ent.Watch), "(none)"))
			base = 2
		}
		for _, b := range *list {
			items = append(items, behaviorSummary(b))
		}
		items = append(items, "[+ add behavior]")
		idx, ok := pickFromList(w, cols, rows, ip, title, items, sel)
		if !ok {
			return
		}
		sel = idx
		switch {
		case ent != nil && idx == 0:
			ent.Tag = sanitizeTag(text("TAG (name; referenced as #tag)", ent.Tag))
		case ent != nil && idx == 1:
			ent.Watch = parseFloats(text("WATCH (HP% thresholds, comma-separated)", watchStr(ent.Watch)))
		case idx == base+len(*list): // add behavior
			if on, ok := pickSig("hp_below"); ok {
				*list = append(*list, gm.Behavior{On: on})
			}
		default: // an existing behavior
			bi := idx - base
			act, ok := pickFromList(w, cols, rows, ip, behaviorSummary((*list)[bi]), []string{"Edit", "Delete"}, 0)
			if !ok {
				continue
			}
			if act == 1 {
				*list = append((*list)[:bi], (*list)[bi+1:]...)
				if sel > base {
					sel--
				}
				continue
			}
			runOneBehavior(w, cols, rows, ip, &(*list)[bi], pickSig)
		}
	}
}

// runOneBehavior edits a single behavior: its trigger, once flag, conditions, actions.
func runOneBehavior(w *bufio.Writer, cols, rows int, ip *input, b *gm.Behavior, pickSig func(string) (string, bool)) {
	sel := 0
	for {
		items := []string{"ON: " + b.On, "ONCE: " + yesno(b.Once)}
		for _, c := range b.When {
			items = append(items, "  when "+condSummary(c))
		}
		items = append(items, "[+ add condition]")
		for _, a := range b.Do {
			items = append(items, "  do "+actSummary(a))
		}
		items = append(items, "[+ add action]")
		idx, ok := pickFromList(w, cols, rows, ip, "EDIT BEHAVIOR", items, sel)
		if !ok {
			return
		}
		sel = idx
		nw := len(b.When)
		addCond := 2 + nw
		actStart := addCond + 1
		addAct := actStart + len(b.Do)
		switch {
		case idx == 0:
			if on, ok := pickSig(b.On); ok {
				b.On = on
			}
		case idx == 1:
			b.Once = !b.Once
		case idx >= 2 && idx < addCond:
			ci := idx - 2
			a, ok := pickFromList(w, cols, rows, ip, condSummary(b.When[ci]), []string{"Edit", "Delete"}, 0)
			if ok && a == 0 {
				runConditionEditor(w, cols, rows, ip, &b.When[ci])
			} else if ok && a == 1 {
				b.When = append(b.When[:ci], b.When[ci+1:]...)
			}
		case idx == addCond:
			c := gm.Condition{Kind: "var", Op: "=="}
			runConditionEditor(w, cols, rows, ip, &c)
			b.When = append(b.When, c)
		case idx >= actStart && idx < addAct:
			ai := idx - actStart
			a, ok := pickFromList(w, cols, rows, ip, actSummary(b.Do[ai]), []string{"Edit", "Delete"}, 0)
			if ok && a == 0 {
				runActionEditor(w, cols, rows, ip, &b.Do[ai])
			} else if ok && a == 1 {
				b.Do = append(b.Do[:ai], b.Do[ai+1:]...)
			}
		case idx == addAct:
			ac := gm.Action{Act: "message"}
			runActionEditor(w, cols, rows, ip, &ac)
			b.Do = append(b.Do, ac)
		}
	}
}

func runConditionEditor(w *bufio.Writer, cols, rows int, ip *input, c *gm.Condition) {
	text := func(p, cur string) string {
		if s, ok := textEntry(w, cols, rows, ip, p, cur); ok {
			return s
		}
		return cur
	}
	pick := func(p string, v []string, cur string) string {
		if i, ok := pickFromList(w, cols, rows, ip, p, v, indexOf(v, cur)); ok {
			return v[i]
		}
		return cur
	}
	sel := 0
	for {
		items := []string{"KIND: " + c.Kind}
		switch c.Kind {
		case "var":
			items = append(items, "VAR: "+c.Var, "OP: "+c.Op, numLabel("N", c.N))
		case "hp":
			items = append(items, "SEL: "+orElse(c.Sel, "self"), "OP: "+c.Op, numLabel("N", c.N))
		case "count":
			items = append(items, "SEL: "+c.Sel, "OP: "+c.Op, numLabel("N", c.N))
		case "near":
			items = append(items, "SEL: "+c.Sel, numLabel("RADIUS", c.R), "OP: "+c.Op, numLabel("N", c.N))
		case "side":
			items = append(items, "SEL: "+orElse(c.Sel, "self"), "IS: "+orElse(c.Var, "player"))
		case "chance":
			items = append(items, numLabel("PERCENT", c.N))
		}
		idx, ok := pickFromList(w, cols, rows, ip, "CONDITION", items, sel)
		if !ok {
			return
		}
		sel = idx
		if idx == 0 {
			c.Kind = pick("KIND", condKindVoc, c.Kind)
			continue
		}
		switch c.Kind {
		case "var":
			switch idx {
			case 1:
				c.Var = text("VAR (blackboard name)", c.Var)
			case 2:
				c.Op = pick("OP", opVocab, c.Op)
			case 3:
				c.N = numEntry(w, cols, rows, ip, "N (value)", c.N)
			}
		case "hp":
			switch idx {
			case 1:
				c.Sel = text("SEL (self or #tag)", c.Sel)
			case 2:
				c.Op = pick("OP", opVocab, c.Op)
			case 3:
				c.N = numEntry(w, cols, rows, ip, "N (HP percent)", c.N)
			}
		case "count":
			switch idx {
			case 1:
				c.Sel = pick("SEL", countSelVoc, c.Sel)
			case 2:
				c.Op = pick("OP", opVocab, c.Op)
			case 3:
				c.N = numEntry(w, cols, rows, ip, "N (count)", c.N)
			}
		case "near":
			switch idx {
			case 1:
				c.Sel = pick("SEL", countSelVoc, c.Sel)
			case 2:
				c.R = numEntry(w, cols, rows, ip, "RADIUS (units around this entity)", c.R)
			case 3:
				c.Op = pick("OP", opVocab, c.Op)
			case 4:
				c.N = numEntry(w, cols, rows, ip, "N (count)", c.N)
			}
		case "side":
			switch idx {
			case 1:
				c.Sel = text("SEL (self/#tag/victim/killer)", c.Sel)
			case 2:
				c.Var = pick("IS", sideVocab, c.Var)
			}
		case "chance":
			if idx == 1 {
				c.N = numEntry(w, cols, rows, ip, "PERCENT (0-100)", c.N)
			}
		}
	}
}

func runActionEditor(w *bufio.Writer, cols, rows int, ip *input, a *gm.Action) {
	text := func(p, cur string) string {
		if s, ok := textEntry(w, cols, rows, ip, p, cur); ok {
			return s
		}
		return cur
	}
	pick := func(p string, v []string, cur string) string {
		if i, ok := pickFromList(w, cols, rows, ip, p, v, indexOf(v, cur)); ok {
			return v[i]
		}
		return cur
	}
	sel := 0
	for {
		items := []string{"ACT: " + a.Act}
		switch a.Act {
		case "message":
			items = append(items, "TEXT: "+clip(a.Text, cols-12))
		case "setvar", "addvar":
			items = append(items, "VAR: "+a.Var, numLabel("N", a.N))
		case "setstat":
			items = append(items, "TARGET: "+orElse(a.Target, "self"), "STAT: "+orElse(a.Stat, "weapon"), numLabel("N", a.N))
		case "spawn":
			items = append(items, "WHAT: "+orElse(a.What, "tank"), "AT: "+orElse(a.At, "self"), numLabel("COUNT", float64(a.Count)))
		case "move":
			items = append(items, "TARGET: "+orElse(a.Target, "self"), "PATH: "+orElse(a.What, "main"), numLabel("SPEED", a.N))
		case "stop":
			items = append(items, "TARGET: "+orElse(a.Target, "self"))
		case "damage", "heal":
			items = append(items, "TARGET: "+orElse(a.Target, "self"), numLabel("AMOUNT", a.N))
		case "emit":
			items = append(items, "SIG: "+a.Sig, numLabel("AFTER (sec)", a.After))
		case "enable", "disable":
			items = append(items, "TARGET: "+a.Target)
		}
		idx, ok := pickFromList(w, cols, rows, ip, "ACTION", items, sel)
		if !ok {
			return
		}
		sel = idx
		if idx == 0 {
			a.Act = pick("ACT", actVocab, a.Act)
			continue
		}
		switch a.Act {
		case "message":
			if idx == 1 {
				a.Text = text("MESSAGE TEXT", a.Text)
			}
		case "setvar", "addvar":
			switch idx {
			case 1:
				a.Var = text("VAR (blackboard name)", a.Var)
			case 2:
				a.N = numEntry(w, cols, rows, ip, "N (value)", a.N)
			}
		case "setstat":
			switch idx {
			case 1:
				a.Target = text("TARGET (self or #tag)", a.Target)
			case 2:
				a.Stat = pick("STAT", statVocab, a.Stat)
			case 3:
				a.N = numEntry(w, cols, rows, ip, "N (value)", a.N)
			}
		case "spawn":
			switch idx {
			case 1:
				a.What = pick("WHAT (archetype)", archVocab, a.What)
			case 2:
				a.At = text("AT (self or #tag)", a.At)
			case 3:
				a.Count = int(numEntry(w, cols, rows, ip, "COUNT", float64(a.Count)))
			}
		case "emit":
			switch idx {
			case 1:
				a.Sig = text("SIGNAL NAME", a.Sig)
			case 2:
				a.After = numEntry(w, cols, rows, ip, "AFTER (sec; 0 = now)", a.After)
			}
		case "move":
			switch idx {
			case 1:
				a.Target = text("TARGET (self or #tag)", a.Target)
			case 2:
				a.What = text("PATH name (default main)", a.What)
			case 3:
				a.N = numEntry(w, cols, rows, ip, "SPEED (units/sec)", a.N)
			}
		case "stop":
			if idx == 1 {
				a.Target = text("TARGET (self or #tag)", a.Target)
			}
		case "damage", "heal":
			switch idx {
			case 1:
				a.Target = text("TARGET (self/#tag/victim/killer)", a.Target)
			case 2:
				a.N = numEntry(w, cols, rows, ip, "AMOUNT (HP)", a.N)
			}
		case "enable", "disable":
			if idx == 1 {
				a.Target = text("TARGET (#tag)", a.Target)
			}
		}
	}
}

// bodyVocab maps body-style index -> name (order matches gm.Body* iota).
var bodyVocab = []string{"tank", "spider", "quadruped", "insect", "humanoid", "scorpion", "serpent", "tripod", "drone", "crab", "octopod", "butterfly", "mantis", "turtle", "trex", "gorilla"}

func bodyName(i int) string {
	if i >= 0 && i < len(bodyVocab) {
		return bodyVocab[i]
	}
	return "?"
}

func vehicleNames() []string {
	out := make([]string, len(gm.Vehicles))
	for i := range gm.Vehicles {
		out[i] = gm.Vehicles[i].Name
	}
	return out
}

func pickIndex(w *bufio.Writer, cols, rows int, ip *input, title string, opts []string, cur int) int {
	if i, ok := pickFromList(w, cols, rows, ip, title, opts, cur); ok {
		return i
	}
	return cur
}

// runActorEditor edits a map's actor templates - named mobile-boss tanks (chassis +
// body + HP + watch + behaviors) that a director spawns with `spawn @name`. This is
// the in-editor authoring for mobile bosses (everything else was already no-code).
func runActorEditor(w *bufio.Writer, cols, rows int, ip *input, m *gm.Map) {
	sel := 0
	for {
		var items []string
		for _, a := range m.Actors {
			items = append(items, fmt.Sprintf("%-12s %s/%s  hp%d  %db", a.Name, gm.Veh(a.Vehicle).Name, bodyName(a.Body), a.MaxHP, len(a.Behaviors)))
		}
		items = append(items, "[+ add actor]")
		idx, ok := pickFromList(w, cols, rows, ip, "ACTORS  (mobile bosses; spawn @name)", items, sel)
		if !ok {
			return
		}
		sel = idx
		if idx == len(m.Actors) { // add
			m.Actors = append(m.Actors, gm.Actor{Name: "boss", Vehicle: 1, Body: gm.BodyTank, MaxHP: 200})
			continue
		}
		act, ok := pickFromList(w, cols, rows, ip, m.Actors[idx].Name, []string{"Edit", "Delete"}, 0)
		if !ok {
			continue
		}
		if act == 1 {
			m.Actors = append(m.Actors[:idx], m.Actors[idx+1:]...)
			if sel > 0 {
				sel--
			}
			continue
		}
		runOneActor(w, cols, rows, ip, &m.Actors[idx])
	}
}

func runOneActor(w *bufio.Writer, cols, rows int, ip *input, a *gm.Actor) {
	text := func(p, cur string) string {
		if s, ok := textEntry(w, cols, rows, ip, p, cur); ok {
			return s
		}
		return cur
	}
	sel := 0
	for {
		items := []string{
			"NAME: " + a.Name,
			"CHASSIS: " + gm.Veh(a.Vehicle).Name,
			"BODY: " + bodyName(a.Body),
			numLabel("MAXHP", float64(a.MaxHP)),
			"WATCH%: " + orElse(watchStr(a.Watch), "(none)"),
			fmt.Sprintf("BEHAVIORS (%d)...", len(a.Behaviors)),
		}
		idx, ok := pickFromList(w, cols, rows, ip, "EDIT ACTOR", items, sel)
		if !ok {
			return
		}
		sel = idx
		switch idx {
		case 0:
			a.Name = sanitizeTag(text("ACTOR NAME (spawn @name)", a.Name))
		case 1:
			a.Vehicle = pickIndex(w, cols, rows, ip, "CHASSIS (stats/frame)", vehicleNames(), a.Vehicle)
		case 2:
			a.Body = pickIndex(w, cols, rows, ip, "BODY (silhouette)", bodyVocab, a.Body)
		case 3:
			a.MaxHP = int(numEntry(w, cols, rows, ip, "MAX HP (0 = chassis default)", float64(a.MaxHP)))
		case 4:
			a.Watch = parseFloats(text("WATCH (HP% thresholds, comma-separated)", watchStr(a.Watch)))
		case 5:
			runBehaviorEditor(w, cols, rows, ip, nil, &a.Behaviors, "ACTOR BEHAVIORS")
		}
	}
}

// sanitizeTag keeps a tag to safe identifier characters.
func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}
