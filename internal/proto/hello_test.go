package proto

import (
	"math"
	"testing"

	gm "spekder/internal/game"
)

// TestHelloRoundTrip: HELLO carries token/bbsid/handle/vehicle/color intact, no
// custom block by default, and a legacy HELLO (no trailing bytes) still decodes.
func TestHelloRoundTrip(t *testing.T) {
	c := [3]float64{0.30, 0.65, 0.90}
	tok, bid, h, v, col, custom, body, ok := DecodeHello(EncodeHello("tok", "bbs", "Derdok", 2, c, nil, gm.BodySpider))
	if !ok || tok != "tok" || bid != "bbs" || h != "Derdok" || v != 2 {
		t.Fatalf("hello fields lost: %q %q %q %d", tok, bid, h, v)
	}
	if custom != nil {
		t.Fatalf("expected no custom block, got %+v", custom)
	}
	if body != gm.BodySpider {
		t.Fatalf("body style lost: %d", body)
	}
	for i := range c {
		if math.Abs(col[i]-c[i]) > 1.0/255 {
			t.Fatalf("color channel %d lost: %v vs %v", i, col, c)
		}
	}
	// Legacy HELLO (no trailing color/custom/body bytes) must still parse, color = zero.
	legacy := EncodeHello("tok", "bbs", "h", 1, [3]float64{}, nil, gm.BodyTank)
	legacy = legacy[:len(legacy)-5] // drop color (3) + custom flag (1) + body (1)
	if _, _, _, gv, gc, _, _, gok := DecodeHello(legacy); !gok || gv != 1 || gc != ([3]float64{}) {
		t.Fatalf("legacy hello: ok=%v v=%d c=%v", gok, gv, gc)
	}
}

// TestHelloCustomRoundTrip: a custom build's sim stats survive the wire.
func TestHelloCustomRoundTrip(t *testing.T) {
	cs := gm.CustomStats{MaxHP: 130, Speed: 7.2, HullTurn: 2.1, FireDelay: 0.46, AmmoMax: 11, AmmoRegen: 1.9}
	_, _, _, v, _, got, _, ok := DecodeHello(EncodeHello("t", "b", "h", 3, [3]float64{0.5, 0.5, 0.5}, &cs, gm.BodyTank))
	if !ok || got == nil {
		t.Fatal("custom block lost")
	}
	if v != 3 || got.MaxHP != cs.MaxHP ||
		math.Abs(got.Speed-cs.Speed) > 1e-4 || math.Abs(got.HullTurn-cs.HullTurn) > 1e-4 ||
		math.Abs(got.FireDelay-cs.FireDelay) > 1e-4 || math.Abs(got.AmmoMax-cs.AmmoMax) > 1e-4 ||
		math.Abs(got.AmmoRegen-cs.AmmoRegen) > 1e-4 {
		t.Fatalf("custom stats mismatch: %+v vs %+v (chassis %d)", *got, cs, v)
	}
}
