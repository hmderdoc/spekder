package proto

import (
	"testing"

	gm "spekder/internal/game"
)

func TestInputRoundTripRecenter(t *testing.T) {
	for _, want := range []bool{false, true} {
		in := gm.Input{Throttle: true, TurretR: true, Recenter: want, Vote: -1}
		got, ok := DecodeInput(EncodeInput(in))
		if !ok {
			t.Fatal("DecodeInput failed")
		}
		if got.Recenter != want {
			t.Fatalf("Recenter lost over wire: want %v got %v", want, got.Recenter)
		}
		if !got.Throttle || !got.TurretR {
			t.Fatalf("other buttons corrupted: %+v", got)
		}
	}
}

// A legacy 3-byte INPUT (no flags byte) must still decode, with Recenter false.
func TestInputBackwardCompat(t *testing.T) {
	got, ok := DecodeInput([]byte{MsgInput, 0x01, 0xFF})
	if !ok {
		t.Fatal("legacy 3-byte input should still decode")
	}
	if got.Recenter {
		t.Fatal("legacy input should default Recenter to false")
	}
}
