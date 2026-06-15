package main

import (
	"bufio"
	"fmt"
)

// Rebindable in-game controls. The reader maps an incoming byte -> game action via
// the effective bind map; OPTIONS -> CONTROLS lets a caller override the defaults
// (select a control, press a key). Menu navigation (arrows / wasd / ENTER) stays
// fixed; only the in-game action keys are remappable.

type bindRow struct {
	act   int
	name  string
	def   byte // default key (0 = arrow-only / none)
	slug  string
	arrow string // fixed arrow-key alias (display only), if any
}

var bindable = []bindRow{
	{aThrottle, "FORWARD", 'w', "forward", ""},
	{aReverse, "BACK", 's', "back", ""},
	{aHullL, "TURN LEFT", 'a', "turnleft", ""},
	{aHullR, "TURN RIGHT", 'd', "turnright", ""},
	{aStrafeL, "STRAFE LEFT", 'q', "strafeleft", ""},
	{aStrafeR, "STRAFE RIGHT", 'e', "straferight", ""},
	{aTurretL, "AIM LEFT", ',', "aimleft", "ARROW LEFT"},
	{aTurretR, "AIM RIGHT", '.', "aimright", "ARROW RIGHT"},
	{aAimUp, "AIM UP", 0, "aimup", "ARROW UP"},
	{aAimDown, "AIM DOWN", 0, "aimdown", "ARROW DOWN"},
	{aFire, "FIRE", ' ', "fire", ""},
	{aFire2, "SECONDARY", 'b', "secondary", ""},
	{aJump, "JUMP", '\r', "jump", ""},
	{aRecenter, "RECENTER AIM", 'c', "recenter", ""},
	{aCruiseF, "CRUISE FWD", 'W', "cruisefwd", ""},
	{aCruiseB, "CRUISE BACK", 'S', "cruiseback", ""},
	{aCruiseL, "CRUISE LEFT", 'A', "cruiseleft", ""},
	{aCruiseR, "CRUISE RIGHT", 'D', "cruiseright", ""},
	{aCruiseSL, "CRUISE STRAFE L", 'Q', "cruisestrafeleft", ""},
	{aCruiseSR, "CRUISE STRAFE R", 'E', "cruisestraferight", ""},
}

// cruiseEvent maps a cruise action to the latch event the reader emits for it.
var cruiseEvent = map[int]menuKey{
	aCruiseF: mkCruiseF, aCruiseB: mkCruiseB, aCruiseL: mkCruiseL, aCruiseR: mkCruiseR,
	aCruiseSL: mkCruiseSL, aCruiseSR: mkCruiseSR,
}

// bindLabel is the displayed key(s) for a control: its fixed arrow alias (if any)
// and/or its remappable key.
func bindLabel(s *userSettings, r bindRow) string {
	k := effectiveKey(s, r.act)
	switch {
	case r.arrow != "" && k != 0:
		return r.arrow + " / " + keyLabel(k)
	case r.arrow != "":
		return r.arrow
	default:
		return keyLabel(k)
	}
}

func slugToAct(slug string) (int, bool) {
	for _, r := range bindable {
		if r.slug == slug {
			return r.act, true
		}
	}
	return 0, false
}

func actSlug(act int) string {
	for _, r := range bindable {
		if r.act == act {
			return r.slug
		}
	}
	return ""
}

// keyToken / tokenKey serialize a bound key for the settings file. Comma is named
// so it survives the comma-separated bind list; 0 = explicitly unbound.
func keyToken(b byte) string {
	switch b {
	case 0:
		return "none"
	case ' ':
		return "space"
	case '\r', '\n':
		return "enter"
	case ',':
		return "comma"
	}
	return string(b)
}

func tokenKey(s string) (byte, bool) {
	switch s {
	case "none":
		return 0, true
	case "space":
		return ' ', true
	case "enter":
		return '\r', true
	case "comma":
		return ',', true
	}
	if len(s) == 1 {
		return s[0], true
	}
	return 0, false
}

// editorBinds are the (fixed) editor camera/menu keys; they fill any byte a combat
// bind didn't claim. Edit/select is on V (E is the combat strafe-right key now).
var editorBinds = map[byte]int{'r': aEdUp, 'f': aEdDown, 'm': aEdMenu, 'v': aEdSelect, 'x': aEdDelete, 'g': aEdGrid}

// effectiveBinds builds the byte->action lookup from the defaults + a caller's
// overrides (action->key; key 0 = explicitly unbound). Each control's own key wins;
// then a letter binding also matches its OTHER case only if that byte is still free.
// So defaults give w=forward, W=cruise (W is claimed), b=B=secondary; but rebind
// CRUISE off W (say to the numpad) and FORWARD becomes w AND W automatically.
func effectiveBinds(custom map[int]byte) map[byte]int {
	m := map[byte]int{}
	keyOf := func(r bindRow) byte {
		if ck, ok := custom[r.act]; ok {
			return ck
		}
		return r.def
	}
	for _, r := range bindable { // pass 1: explicit keys win
		if k := keyOf(r); k != 0 {
			m[k] = r.act
		}
	}
	free := func(k byte, a int) {
		if k != 0 {
			if _, taken := m[k]; !taken {
				m[k] = a
			}
		}
	}
	for _, r := range bindable { // pass 2: the other case, only where free
		free(otherCase(keyOf(r)), r.act)
	}
	if m[','] == aTurretL { // '<' '>' aliases when aim is still on comma/period
		free('<', aTurretL)
	}
	if m['.'] == aTurretR {
		free('>', aTurretR)
	}
	if m['\r'] == aJump {
		free('\n', aJump)
	}
	for k, a := range editorBinds { // editor keys (both cases) fill leftover bytes
		free(k, a)
		free(otherCase(k), a)
	}
	return m
}

func otherCase(k byte) byte {
	switch {
	case k >= 'a' && k <= 'z':
		return k - 32
	case k >= 'A' && k <= 'Z':
		return k + 32
	}
	return 0
}

// keyLabel is a human-readable name for a bound key.
func keyLabel(b byte) string {
	switch b {
	case 0:
		return "(unbound)"
	case ' ':
		return "SPACE"
	case '\r', '\n':
		return "ENTER"
	}
	if b >= 0x21 && b < 0x7f {
		return "'" + string(b) + "'"
	}
	return fmt.Sprintf("0x%02x", b)
}

// effectiveKey is the key currently bound to an action (custom else default).
func effectiveKey(s *userSettings, act int) byte {
	if ck, ok := s.keyBinds[act]; ok {
		return ck
	}
	for _, r := range bindable {
		if r.act == act {
			return r.def
		}
	}
	return 0
}

// rebind sets key k for action act, stealing it from any other action that holds
// it and recording the override (k == 0 unbinds the action).
func rebind(s *userSettings, act int, k byte) {
	if s.keyBinds == nil {
		s.keyBinds = map[int]byte{}
	}
	if k != 0 {
		for _, r := range bindable {
			if r.act != act && effectiveKey(s, r.act) == k {
				s.keyBinds[r.act] = 0 // stolen: leave the other control unbound
			}
		}
	}
	s.keyBinds[act] = k
}

// runControls is the key-binding screen: select a control + press a key to map it,
// toggle case sensitivity, or reset to defaults. Changes apply live and persist.
func runControls(w *bufio.Writer, cols, rows int, ip *input, dropfile string, s *userSettings) {
	apply := func() { ip.setBinds(effectiveBinds(s.keyBinds)) }
	iReset := len(bindable)
	iBack := iReset + 1
	sel := 0

	// captureKey waits for the next printable key (Backspace cancels, Esc quits).
	captureKey := func() (byte, bool) {
		ip.drainRunes()
		msg := "  press a key to bind   (Backspace = cancel)  "
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;93m%s\x1b[0m", rows-1, (cols-len(msg))/2+1, msg)
		w.Flush()
		for {
			select {
			case <-ip.quitCh:
				return 0, false
			case k := <-ip.events:
				if k == mkBack {
					return 0, false
				}
			case r := <-ip.runes:
				if r >= 0x20 && r < 0x7f {
					return byte(r), true
				}
			}
		}
	}
	drainEvents := func() {
		for {
			select {
			case <-ip.events:
			default:
				return
			}
		}
	}

	// Compact single-column list (too many rows for the figlet-title menu).
	draw := func() {
		w.WriteString("\x1b[2J\x1b[H")
		title := "CONTROLS"
		fmt.Fprintf(w, "\x1b[1;%dH\x1b[1;96m%s\x1b[0m", (cols-len(title))/2+1, title)
		line := func(row, idx int, text string) {
			style := "\x1b[0;36m"
			if idx == sel {
				style = "\x1b[1;30;46m"
			}
			fmt.Fprintf(w, "\x1b[%d;4H%s %-32s \x1b[0m", row, style, text)
		}
		top := 3
		for i, r := range bindable {
			line(top+i, i, fmt.Sprintf("%-13s %s", r.name, bindLabel(s, r)))
		}
		line(top+len(bindable)+1, iReset, "RESET TO DEFAULTS")
		line(top+len(bindable)+2, iBack, "BACK")
		hint := "ENTER, then press a key to bind it."
		if sel < len(bindable) && bindable[sel].arrow != "" {
			hint = "ENTER to add a key (arrows always aim)."
		}
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;37m%s\x1b[0m", rows-2, (cols-len(hint))/2+1, hint)
		foot := "up/dn  select     ENTER  bind     < back"
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", rows-1, (cols-len(foot))/2+1, foot)
		w.Flush()
	}
	draw()
	for {
		select {
		case <-ip.quitCh:
			return
		case k := <-ip.events:
			switch k {
			case mkUp:
				sel = (sel - 1 + iBack + 1) % (iBack + 1)
				draw()
			case mkDown:
				sel = (sel + 1) % (iBack + 1)
				draw()
			case mkLeft:
				return
			case mkEnter:
				switch {
				case sel < len(bindable):
					if key, ok := captureKey(); ok {
						rebind(s, bindable[sel].act, key)
						apply()
						saveUserSettings(dropfile, *s)
					}
					drainEvents() // the bound key also queued a nav event; drop it
				case sel == iReset:
					s.keyBinds = map[int]byte{}
					apply()
					saveUserSettings(dropfile, *s)
				case sel == iBack:
					return
				}
				draw()
			}
		}
	}
}
