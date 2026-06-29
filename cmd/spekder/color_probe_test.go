package main

import "testing"

// parseColorReadback must be conservative: it may only DOWNGRADE to 256 on an
// explicit quantized DECRQSS reply. A silent terminal, a non-DECRQSS reply, or a
// preserved 24-bit echo must never yield anything but "stay truecolor" — that's
// what protects truecolor callers (iTerm2, SyncTERM, the webv4 ftelnet mod).
func TestParseColorReadback(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantMode int
		wantOK   bool
	}{
		{"silent / no reply", "", colorTrue, false},
		{"truecolor preserved (semicolon)", "\x1bP1$r0;38;2;1;2;3m\x1b\\", colorTrue, true},
		{"truecolor preserved (colon subparams, iTerm2 style)", "\x1bP1$r0;38:2::1:2:3m\x1b\\", colorTrue, true},
		{"quantized to 256 (semicolon)", "\x1bP1$r0;38;5;16m\x1b\\", color256, true},
		{"quantized to 256 (colon)", "\x1bP1$r0;38:5:16m\x1b\\", color256, true},
		{"DECRQSS unsupported (P0) / only reset", "\x1bP0$r\x1b\\", colorTrue, false},
		{"not a DECRQSS reply (stray CPR)", "\x1b[24;80R", colorTrue, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, ok := parseColorReadback([]byte(c.in))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && mode != c.wantMode {
				t.Fatalf("mode = %s, want %s", colorModeName(mode), colorModeName(c.wantMode))
			}
		})
	}
}
