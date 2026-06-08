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
	difficulty gm.Difficulty
	aimAssist  bool
}

func defaultSettings() userSettings {
	return userSettings{difficulty: gm.DiffNormal, aimAssist: true}
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
	ini := loadINI(userSettingsPath(dropfile))
	if d, ok := gm.ParseDifficulty(ini["difficulty"]); ok {
		s.difficulty = d
	}
	switch strings.ToLower(ini["aimassist"]) {
	case "off", "false", "0", "no":
		s.aimAssist = false
	case "on", "true", "1", "yes":
		s.aimAssist = true
	}
	return s
}

// saveUserSettings persists the caller's preferences (best-effort, both keys).
func saveUserSettings(dropfile string, s userSettings) {
	p := userSettingsPath(dropfile)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	aa := "off"
	if s.aimAssist {
		aa = "on"
	}
	body := fmt.Sprintf("difficulty = %s\naimassist = %s\n", s.difficulty.String(), aa)
	_ = os.WriteFile(p, []byte(body), 0o644)
}
