package proto

import (
	"math"
	"testing"

	gm "spekder/internal/game"
)

// TestHelloRoundTrip: HELLO carries token/bbsid/handle/color/body/publish and the
// version tail intact; a legacy HELLO (missing trailing bytes) still decodes with
// zero-value defaults (clientProto 0, which the server's join guard rejects).
func TestHelloRoundTrip(t *testing.T) {
	c := [3]float64{0.30, 0.65, 0.90}
	full := EncodeHello("tok", "bbs", "Derdok", c, gm.BodySpider, false, ProtocolVersion, "1.2.3", "WOLVES")
	tok, bid, h, col, body, pub, cproto, cver, party, ok := DecodeHello(full)
	if !ok || tok != "tok" || bid != "bbs" || h != "Derdok" {
		t.Fatalf("hello fields lost: %q %q %q", tok, bid, h)
	}
	if pub {
		t.Fatalf("publish flag should be false")
	}
	if body != gm.BodySpider {
		t.Fatalf("body style lost: %d", body)
	}
	if cproto != ProtocolVersion || cver != "1.2.3" || party != "WOLVES" {
		t.Fatalf("version/party tail lost: proto=%d ver=%q party=%q", cproto, cver, party)
	}
	for i := range c {
		if math.Abs(col[i]-c[i]) > 1.0/255 {
			t.Fatalf("color channel %d lost: %v vs %v", i, col, c)
		}
	}
	// Missing version+party tail (all empty): drop the u16 proto + two empty
	// strings = 4 bytes. clientProto defaults to 0; everything before survives.
	noVer := EncodeHello("tok", "bbs", "h", [3]float64{}, gm.BodyTank, false, ProtocolVersion, "", "")
	noVer = noVer[:len(noVer)-4]
	if _, _, _, _, gb, _, gp, _, _, gok := DecodeHello(noVer); !gok || gb != gm.BodyTank || gp != 0 {
		t.Fatalf("hello w/o version tail: ok=%v body=%d proto=%d", gok, gb, gp)
	}
}

// TestHelloPublishRoundTrip: the publish flag survives the wire both ways.
func TestHelloPublishRoundTrip(t *testing.T) {
	for _, want := range []bool{false, true} {
		_, _, h, _, body, pub, _, _, _, ok := DecodeHello(
			EncodeHello("t", "b", "Derdok", [3]float64{}, gm.BodyTank, want, ProtocolVersion, "dev", ""))
		if !ok || h != "Derdok" || body != gm.BodyTank || pub != want {
			t.Fatalf("publish=%v round-trip: ok=%v handle=%q body=%d pub=%v", want, ok, h, body, pub)
		}
	}
}

// TestRejectRoundTrip: a server reject carries its reason back to the client.
func TestRejectRoundTrip(t *testing.T) {
	reason, ok := DecodeReject(EncodeReject("Version mismatch: update your client."))
	if !ok || reason != "Version mismatch: update your client." {
		t.Fatalf("reject reason lost: ok=%v %q", ok, reason)
	}
	if _, ok := DecodeReject(EncodeWelcome(3)); ok {
		t.Fatal("DecodeReject accepted a non-reject message")
	}
}

// TestPartyKickRoundTrip: a party-kick carries owner/party/target back intact.
func TestPartyKickRoundTrip(t *testing.T) {
	tok, owner, party, target, ok := DecodePartyKick(EncodePartyKick("tk", "DERDOK", "DERDOK", "RAZOR"))
	if !ok || tok != "tk" || owner != "DERDOK" || party != "DERDOK" || target != "RAZOR" {
		t.Fatalf("party-kick fields lost: %q %q %q %q ok=%v", tok, owner, party, target, ok)
	}
	if _, _, _, _, ok := DecodePartyKick(EncodeWelcome(1)); ok {
		t.Fatal("DecodePartyKick accepted a non-kick message")
	}
}
