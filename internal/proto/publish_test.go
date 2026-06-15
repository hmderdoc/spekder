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
		Rules:     &gm.MapRules{Mode: int(gm.ModeFFAKotH), TimeLimit: -1, Target: 80, Lives: -1},
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
	if dm.Rules == nil || dm.Rules.Mode != int(gm.ModeFFAKotH) || dm.Rules.Target != 80 || dm.Rules.TimeLimit != -1 || dm.Rules.Lives != -1 {
		t.Fatalf("publish round-trip lost rules: %+v", dm.Rules)
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

// TestPublishCarriesBehaviors: publish goes via JSON, so event vars/logic and
// entity tag/watch/behaviors survive the round-trip to the arena repo.
func TestPublishCarriesBehaviors(t *testing.T) {
	m := gm.Map{
		Name: "WB", Version: 4, Size: 16,
		Vars:  map[string]int{"phase": 1},
		Logic: []gm.Behavior{{On: "start", Do: []gm.Action{{Act: "message", Text: "hi"}}}},
		Entities: []gm.Entity{{
			Kind: "turret", Tag: "boss", Half: gm.V3{X: 1, Y: 1, Z: 1},
			Turret: &gm.TurretTrait{Range: 10}, Destruct: &gm.DestructTrait{MaxHP: 100},
			Watch: []float64{50},
			Behaviors: []gm.Behavior{{On: "hp_below", Once: true,
				When: []gm.Condition{{Kind: "hp", Sel: "self", Op: "<=", N: 50}},
				Do:   []gm.Action{{Act: "spawn", What: "spider", Count: 2}}}},
		}},
	}
	got, ok := DecodePublish(EncodePublish(m))
	if !ok {
		t.Fatal("DecodePublish failed")
	}
	if got.Vars["phase"] != 1 || len(got.Logic) != 1 {
		t.Fatalf("director logic lost: %+v %+v", got.Vars, got.Logic)
	}
	e := got.Entities[0]
	if e.Tag != "boss" || len(e.Watch) != 1 || len(e.Behaviors) != 1 || e.Behaviors[0].Do[0].What != "spider" {
		t.Fatalf("entity behaviors lost over publish: %+v", e)
	}
}
