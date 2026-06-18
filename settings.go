package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gm "spekder/internal/game"
)

// Per-BBS-user settings (offline difficulty + aim assist). Keyed by a sanitized
// DOOR32 handle so each caller's preferences are their own; stored next to the
// binary under data/. See DIFFICULTY.md.

type userSettings struct {
	difficulty   gm.Difficulty
	aimAssist    bool
	sound        bool // ANSI-music sound effects + music; opt-out
	soundTested  bool // whether the first-run sound check has been done (else: ask)
	colorMode    int  // colorTrue/color256/color16 (see color.go)
	campaignBest int  // highest campaign level cleared (0 = never)

	// Custom controls (see controls.go). keyBinds maps a game action -> its key
	// (0 = explicitly unbound); absent = the default key.
	keyBinds map[int]byte
}

func defaultSettings() userSettings {
	return userSettings{difficulty: gm.DiffNormal, aimAssist: true, sound: true, colorMode: colorTrue, keyBinds: map[int]byte{}}
}

// sanitizeKey reduces a handle to a safe, stable filename fragment.
func sanitizeKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "player"
	}
	k := b.String()
	if len(k) > 32 {
		k = k[:32]
	}
	return k
}

// authorMapsDir is where the editor saves maps and the door loads them from
// (usermaps/ next to the binary).
func authorMapsDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "usermaps")
	}
	return "usermaps"
}

// userSettingsPath returns the per-user settings file path (data/ next to the binary).
func userSettingsPath(dropfile string) string {
	_, handle := door32Identity(dropfile)
	key := sanitizeKey(handle)
	dir := "data"
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Join(filepath.Dir(exe), "data")
	}
	return filepath.Join(dir, "spekder-"+key+".ini")
}

// loadUserSettings reads the caller's saved preferences (defaults if absent).
func loadUserSettings(dropfile string) userSettings {
	s := defaultSettings()
	// The door ini may set a system-wide default color mode (e.g. a board whose
	// callers are mostly on classic terminals); the user's own pick overrides it.
	if m, ok := parseColorMode(loadINI(defaultINIPath())["color_mode"]); ok {
		s.colorMode = m
	}
	ini := loadINI(userSettingsPath(dropfile))
	if d, ok := gm.ParseDifficulty(ini["difficulty"]); ok {
		s.difficulty = d
	}
	if m, ok := parseColorMode(ini["colormode"]); ok {
		s.colorMode = m
	}
	if n, ok := atoiOK(ini["campaignbest"]); ok {
		s.campaignBest = n
	}
	switch strings.ToLower(ini["aimassist"]) {
	case "off", "false", "0", "no":
		s.aimAssist = false
	case "on", "true", "1", "yes":
		s.aimAssist = true
	}
	switch strings.ToLower(ini["sound"]) {
	case "off", "false", "0", "no":
		s.sound = false
	case "on", "true", "1", "yes":
		s.sound = true
	}
	switch strings.ToLower(ini["soundtested"]) {
	case "on", "true", "1", "yes":
		s.soundTested = true
	}
	if bs := ini["binds"]; bs != "" {
		s.keyBinds = map[int]byte{}
		for _, entry := range strings.Split(bs, ",") {
			entry = strings.TrimSpace(entry)
			i := strings.IndexByte(entry, ':')
			if i <= 0 {
				continue
			}
			act, ok := slugToAct(entry[:i])
			if !ok {
				continue
			}
			if k, ok := tokenKey(entry[i+1:]); ok {
				s.keyBinds[act] = k
			}
		}
	}
	setSoundOn(s.sound) // sync the emit gate to the loaded preference
	return s
}

func atoiOK(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// saveUserSettings persists the caller's preferences (best-effort, both keys).
func saveUserSettings(dropfile string, s userSettings) {
	p := userSettingsPath(dropfile)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	aa := "off"
	if s.aimAssist {
		aa = "on"
	}
	snd := "off"
	if s.sound {
		snd = "on"
	}
	stested := "off"
	if s.soundTested {
		stested = "on"
	}
	body := fmt.Sprintf("difficulty = %s\naimassist = %s\nsound = %s\nsoundtested = %s\ncolormode = %s\n", s.difficulty.String(), aa, snd, stested, colorModeSlug(s.colorMode))
	if s.campaignBest > 0 {
		body += fmt.Sprintf("campaignbest = %d\n", s.campaignBest)
	}
	if len(s.keyBinds) > 0 {
		var parts []string
		for _, r := range bindable { // stable order
			if k, ok := s.keyBinds[r.act]; ok {
				parts = append(parts, r.slug+":"+keyToken(k))
			}
		}
		if len(parts) > 0 {
			body += "binds = " + strings.Join(parts, ",") + "\n"
		}
	}
	_ = os.WriteFile(p, []byte(body), 0o644)
}
