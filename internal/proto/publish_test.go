package proto

import (
	"testing"

	gm "spekder/internal/game"
)

// TestPublishRoundTrip: a published map survives the wire (same body as MsgMap,
// distinct opcode), and the ack carries its ok flag + message.
func TestPublishRoundTrip(t *testing.T) {
	m := gm.Map{
		Name: "PUB", Size: 16,
		Obstacles: []gm.Box{{Pos: gm.V3{X: 1, Y: 1, Z: 1}, Half: gm.V3{X: 1, Y: 1, Z: 1}, Color: [3]float64{0.4, 0.4, 0.5}}},
		Entities:  []gm.Entity{{Kind: "turret", Pos: gm.V3{Y: 1}, Half: gm.V3{X: 0.7, Y: 1, Z: 0.7}, Turret: &gm.TurretTrait{Range: 20, Dmg: 12, FireDelay: 1.2, TurnRate: 1.5}}},
	}
	enc := EncodePublish(m)
	if enc[0] != MsgPublish {
		t.Fatalf("opcode = %#x, want MsgPublish", enc[0])
	}
	if _, ok := DecodeMap(enc); ok {
		t.Fatal("DecodeMap should reject a publish payload (wrong opcode)")
	}
	dm, ok := DecodePublish(enc)
	if !ok {
		t.Fatal("DecodePublish failed")
	}
	if dm.Name != "PUB" || len(dm.Obstacles) != 1 || len(dm.Entities) != 1 || dm.Entities[0].Turret == nil {
		t.Fatalf("publish round-trip lost data: %+v", dm)
	}

	for _, tc := range []struct {
		ok  bool
		msg string
	}{{true, "published PUB"}, {false, "map has fatal errors"}} {
		gotOK, gotMsg, good := DecodePubAck(EncodePubAck(tc.ok, tc.msg))
		if !good || gotOK != tc.ok || gotMsg != tc.msg {
			t.Fatalf("ack round-trip: got (%v,%q,%v) want (%v,%q)", gotOK, gotMsg, good, tc.ok, tc.msg)
		}
	}
}
