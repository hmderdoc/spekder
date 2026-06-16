package proto

import (
	"testing"

	gm "spekder/internal/game"
)

// TestBalanceWireRoundTrip confirms the balance push survives encode->decode
// intact (within float32 wire precision) and that a truncated/garbage payload
// decodes as not-ok rather than panicking - the same graceful path an older
// client takes when it sees the message at all.
func TestBalanceWireRoundTrip(t *testing.T) {
	in := gm.CurrentBalance()
	in.Chassis[0].MaxHP = 123
	in.Bodies[gm.BodyTrex].HPRegen = 7.5

	enc := EncodeBalance(in)
	if enc[0] != MsgBalance {
		t.Fatalf("wrong type byte: %#x", enc[0])
	}
	out, ok := DecodeBalance(enc)
	if !ok {
		t.Fatal("DecodeBalance returned not-ok on a valid payload")
	}
	if len(out.Chassis) != len(in.Chassis) || len(out.Bodies) != len(in.Bodies) {
		t.Fatalf("length mismatch: chassis %d/%d bodies %d/%d",
			len(out.Chassis), len(in.Chassis), len(out.Bodies), len(in.Bodies))
	}
	if out.Chassis[0].MaxHP != 123 {
		t.Errorf("MaxHP lost: got %d", out.Chassis[0].MaxHP)
	}
	if !approx(out.Bodies[gm.BodyTrex].HPRegen, 7.5) {
		t.Errorf("HPRegen lost: got %v", out.Bodies[gm.BodyTrex].HPRegen)
	}
	for i := range in.Chassis {
		if !approx(out.Chassis[i].Speed, in.Chassis[i].Speed) {
			t.Errorf("chassis %d Speed drift: %v != %v", i, out.Chassis[i].Speed, in.Chassis[i].Speed)
		}
	}

	if _, ok := DecodeBalance([]byte{MsgBalance, 99}); ok {
		t.Error("truncated payload decoded as ok")
	}
	if _, ok := DecodeBalance([]byte{MsgState}); ok {
		t.Error("wrong type decoded as ok")
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-3
}
