package proto

import (
	"testing"

	gm "spekder/internal/game"
)

// TestMapReqRoundTrip locks the lazy-preview request wire format.
func TestMapReqRoundTrip(t *testing.T) {
	for _, idx := range []int{0, 1, 7, 300} {
		got, ok := DecodeMapReq(EncodeMapReq(idx))
		if !ok || got != idx {
			t.Fatalf("MapReq round-trip idx=%d: got %d ok=%v", idx, got, ok)
		}
	}
	if _, ok := DecodeMapReq([]byte{MsgInput}); ok {
		t.Fatal("DecodeMapReq accepted a non-MapReq message")
	}
}

// TestMapPreviewRoundTrip ensures a preview carries its index plus the full map
// geometry (same body as MsgMap) and is distinct from MsgMap on the wire.
func TestMapPreviewRoundTrip(t *testing.T) {
	m := gm.Map{
		Name: "ESCORT", Size: 24,
		Obstacles: []gm.Box{{Pos: gm.V3{X: 2, Z: -1}, Half: gm.V3{X: 1, Y: 2, Z: 1}, Color: [3]float64{0.3, 0.4, 0.5}}},
		Ramps:     []gm.Ramp{{Pos: gm.V3{X: -3}, Half: gm.V3{X: 2, Z: 2}, H: 4, Dir: 1, Color: [3]float64{0.2, 0.2, 0.2}}},
		Spawns:    []gm.V3{{X: -10, Z: -10}, {X: 10, Z: 10}},
		Rules:     &gm.MapRules{Mode: int(gm.ModeDeathmatch), TimeLimit: 180, Target: 0, Lives: 0},
	}
	enc := EncodeMapPreview(5, m)
	if enc[0] != MsgMapPreview {
		t.Fatalf("preview tag = %#x, want %#x", enc[0], MsgMapPreview)
	}
	if _, ok := DecodeMap(enc); ok {
		t.Fatal("DecodeMap accepted a MsgMapPreview (tags must be distinct)")
	}
	idx, got, ok := DecodeMapPreview(enc)
	if !ok {
		t.Fatal("DecodeMapPreview failed")
	}
	if idx != 5 {
		t.Fatalf("preview idx = %d, want 5", idx)
	}
	if got.Name != m.Name || got.Size != m.Size || len(got.Obstacles) != 1 || len(got.Ramps) != 1 || len(got.Spawns) != 2 {
		t.Fatalf("preview map mismatch: %+v", got)
	}
	if got.Rules == nil || got.Rules.Mode != int(gm.ModeDeathmatch) || got.Rules.TimeLimit != 180 {
		t.Fatalf("preview rules mismatch: %+v", got.Rules)
	}
}

// TestMapHashDeterministic locks the cache-key contract: the same map hashes to
// the same value, an edited map hashes differently.
func TestMapHashDeterministic(t *testing.T) {
	m := gm.Map{Name: "PIT", Size: 20, Spawns: []gm.V3{{X: 1}, {X: -1}}}
	if MapHash(m) != MapHash(m) {
		t.Fatal("MapHash not deterministic")
	}
	m2 := m
	m2.Size = 21 // any content change must move the hash
	if MapHash(m) == MapHash(m2) {
		t.Fatal("MapHash unchanged after edit")
	}
}

// TestLobbyHashRoundTrip ensures each lobby entry carries its map's content hash.
func TestLobbyHashRoundTrip(t *testing.T) {
	if len(gm.Maps) == 0 {
		t.Skip("no maps registered")
	}
	entries, ok := DecodeLobby(EncodeLobby())
	if !ok || len(entries) != len(gm.Maps) {
		t.Fatalf("lobby decode: ok=%v got %d want %d", ok, len(entries), len(gm.Maps))
	}
	for i, e := range entries {
		if e.Hash != MapHash(gm.Maps[i]) {
			t.Fatalf("entry %d hash %08x != MapHash %08x", i, e.Hash, MapHash(gm.Maps[i]))
		}
	}
}
