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
	in.Rows[gm.BodyTrex].MaxHP = 123
	in.Bodies[gm.BodyTrex].HPRegen = 7.5

	enc := EncodeBalance(in)
	if enc[0] != MsgBalance {
		t.Fatalf("wrong type byte: %#x", enc[0])
	}
	out, ok := DecodeBalance(enc)
	if !ok {
		t.Fatal("DecodeBalance returned not-ok on a valid payload")
	}
	if len(out.Rows) != len(in.Rows) || len(out.Bodies) != len(in.Bodies) {
		t.Fatalf("length mismatch: rows %d/%d bodies %d/%d",
			len(out.Rows), len(in.Rows), len(out.Bodies), len(in.Bodies))
	}
	if out.Rows[gm.BodyTrex].MaxHP != 123 {
		t.Errorf("MaxHP lost: got %d", out.Rows[gm.BodyTrex].MaxHP)
	}
	if !approx(out.Bodies[gm.BodyTrex].HPRegen, 7.5) {
		t.Errorf("HPRegen lost: got %v", out.Bodies[gm.BodyTrex].HPRegen)
	}
	for i := range in.Rows {
		if !approx(out.Rows[i].Speed, in.Rows[i].Speed) {
			t.Errorf("row %d Speed drift: %v != %v", i, out.Rows[i].Speed, in.Rows[i].Speed)
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
