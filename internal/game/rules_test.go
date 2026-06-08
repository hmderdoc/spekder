package game

import "testing"

// TestRulesOverlay: a map's MapRules override the effective ruleset (time/target/
// lives) without mutating the shared Rulesets table; -1/nil defer to the mode.
func TestRulesOverlay(t *testing.T) {
	saved := Maps
	defer func() { Maps = saved }()

	baseCount := RulesetFor(ModeDeathmatch).Win[0].Count

	Maps = []Map{{Name: "R", Rules: &MapRules{Mode: -1, TimeLimit: 90, Target: 7, Lives: 2}}}
	w := &World{Mode: ModeDeathmatch, MapIdx: 0}
	r := w.rules()
	if r.TimeLimit != 90 || r.Lives != 2 || len(r.Win) == 0 || r.Win[0].Count != 7 {
		t.Fatalf("overlay not applied: time=%v lives=%d win=%+v", r.TimeLimit, r.Lives, r.Win)
	}
	if RulesetFor(ModeDeathmatch).Win[0].Count != baseCount {
		t.Fatal("overlay mutated the shared Rulesets table")
	}

	// All-default rules == base ruleset.
	Maps = []Map{{Name: "D", Rules: &MapRules{Mode: -1, TimeLimit: -1, Target: -1, Lives: -1}}}
	if w.rules().Win[0].Count != baseCount {
		t.Fatal("default (-1) rules should defer to the mode")
	}

	// Nil rules == base ruleset.
	Maps = []Map{{Name: "N"}}
	if w.rules().TimeLimit != RulesetFor(ModeDeathmatch).TimeLimit {
		t.Fatal("nil rules should defer to the mode")
	}
}

// TestEffectiveMode: an explicit Rules.Mode wins; otherwise it falls back to the
// objective-implied NaturalMode.
func TestEffectiveMode(t *testing.T) {
	if got := EffectiveMode(Map{Rules: &MapRules{Mode: int(ModeCTF)}}); got != ModeCTF {
		t.Fatalf("explicit mode: got %v want CTF", got)
	}
	zone := Map{Entities: []Entity{{Zone: &ZoneTrait{Capture: 4}}}}
	if got := EffectiveMode(zone); got != ModeFFAKotH {
		t.Fatalf("auto from zone: got %v want KotH", got)
	}
	// Auto (-1) defers to NaturalMode even when Rules exists.
	zone.Rules = &MapRules{Mode: -1}
	if got := EffectiveMode(zone); got != ModeFFAKotH {
		t.Fatalf("auto rules: got %v want KotH", got)
	}
}
