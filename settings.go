package main

import (
	"os"
	"path/filepath"
	"strings"

	gm "spekder/internal/game"
)

// Per-BBS-user settings (offline difficulty for now). Keyed by a sanitized
// DOOR32 handle so each caller's preference is their own; stored next to the
// binary under data/. See DIFFICULTY.md.

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

// userSettingsPath returns the per-user settings file path (data/ next to the binary).
func userSettingsPath(key string) string {
	dir := "data"
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Join(filepath.Dir(exe), "data")
	}
	return filepath.Join(dir, "spekder-"+key+".ini")
}

// userKey derives the per-user settings key from the dropfile's handle.
func userKey(dropfile string) string {
	_, handle := door32Identity(dropfile)
	return sanitizeKey(handle)
}

// loadUserDifficulty reads the caller's saved tier (default NORMAL).
func loadUserDifficulty(dropfile string) gm.Difficulty {
	ini := loadINI(userSettingsPath(userKey(dropfile)))
	if d, ok := gm.ParseDifficulty(ini["difficulty"]); ok {
		return d
	}
	return gm.DiffNormal
}

// saveUserDifficulty persists the caller's chosen tier (best-effort).
func saveUserDifficulty(dropfile string, d gm.Difficulty) {
	p := userSettingsPath(userKey(dropfile))
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("difficulty = "+d.String()+"\n"), 0o644)
}
