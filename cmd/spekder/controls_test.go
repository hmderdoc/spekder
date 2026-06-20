package main

import "testing"

func TestResolveTipDefaults(t *testing.T) {
	s := defaultSettings()
	cases := map[string]string{
		"Press {forward} to roll":      "Press w to roll",
		"Tap {jump} to leap":           "Tap ENTER to leap",
		"{fire} shoots, {secondary} B": "SPACE shoots, b B",
		"Swing with {aim}":             "Swing with arrows",
		"Hold {cruise} to latch":       "Hold Shift to latch",
		"unknown {bogus} stays":        "unknown {bogus} stays",
	}
	for in, want := range cases {
		if got := resolveTip(in, &s); got != want {
			t.Errorf("resolveTip(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveTipHonorsRemap(t *testing.T) {
	s := defaultSettings()
	s.keyBinds[aJump] = 'j' // remap JUMP off ENTER
	if got := resolveTip("Tap {jump} to leap", &s); got != "Tap j to leap" {
		t.Errorf("remapped jump tip = %q, want %q", got, "Tap j to leap")
	}
	// aim always shows arrows, even though it's technically remappable
	if got := resolveTip("{aim}", &s); got != "arrows" {
		t.Errorf("aim tip = %q, want arrows", got)
	}
}
