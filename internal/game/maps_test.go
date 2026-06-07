package game

import "testing"

// TestEmbeddedMapsLoad guards against a malformed map JSON: the loader skips
// files that fail to parse, so a typo would silently drop a map. Assert the
// demo maps are present with the trait content they're meant to showcase.
func TestEmbeddedMapsLoad(t *testing.T) {
	if len(Maps) < 8 {
		t.Fatalf("expected the embedded map set to load (>=8), got %d", len(Maps))
	}
	redoubt := FindMap("REDOUBT")
	if redoubt < 0 {
		t.Fatal("REDOUBT map missing (parse error?)")
	}
	if turrets := countTurrets(Maps[redoubt]); turrets == 0 {
		t.Fatal("REDOUBT should have turret entities")
	}

	ascent := FindMap("ASCENT")
	if ascent < 0 {
		t.Fatal("ASCENT map missing (parse error?)")
	}
	m := Maps[ascent]
	if len(m.Ramps) == 0 {
		t.Fatal("ASCENT should define ramps")
	}
	bounces := 0
	for _, e := range m.Entities {
		if e.Bounce != nil {
			bounces++
		}
	}
	if bounces == 0 {
		t.Fatal("ASCENT should have trampoline (bounce) entities")
	}
}

// TestEmbeddedMapsValidate ensures every shipped map passes the validator with
// no fatal issues (and surfaces any warnings in the test log).
func TestEmbeddedMapsValidate(t *testing.T) {
	for _, m := range Maps {
		for _, is := range ValidateMap(m) {
			if is.Fatal {
				t.Errorf("map %q: %s", m.Name, is)
			} else {
				t.Logf("map %q: %s", m.Name, is)
			}
		}
	}
}

func countTurrets(m Map) int {
	n := 0
	for _, e := range m.Entities {
		if e.Turret != nil {
			n++
		}
	}
	return n
}
