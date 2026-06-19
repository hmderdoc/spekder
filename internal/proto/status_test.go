package proto

import (
	"testing"

	gm "spekder/internal/game"
)

func TestStatusRoundTrip(t *testing.T) {
	token, party, ok := DecodeStatusQuery(EncodeStatusQuery("secret", "derdok"))
	if !ok || token != "secret" || party != "derdok" {
		t.Fatalf("status query round-trip: got %q %q %v", token, party, ok)
	}

	want := ArenaStatus{
		Humans: 3,
		Phase:  gm.PhaseLobby,
		Mode:   gm.ModeCTF,
		Map:    "Citadel",
		Presence: []Presence{{
			Session: "bbs:alice:123",
			BBSID:   "bbs",
			Handle:  "alice",
			State:   "menu",
			Detail:  "waiting",
			Updated: 99,
			Party:   "WOLVES",
		}},
		Chat: []ChatMessage{{
			Seq:    7,
			Time:   1234,
			BBSID:  "bbs",
			Handle: "alice",
			Text:   "hello",
		}},
		ServerVersion: "0.4.0",
		LatestClient:  "0.4.0",
	}
	got, ok := DecodeStatus(EncodeStatus(want))
	if !ok {
		t.Fatal("DecodeStatus failed")
	}
	if got.Humans != want.Humans || got.Phase != want.Phase || got.Mode != want.Mode || got.Map != want.Map ||
		len(got.Presence) != 1 || got.Presence[0] != want.Presence[0] ||
		len(got.Chat) != 1 || got.Chat[0] != want.Chat[0] {
		t.Fatalf("status round-trip: got %+v want %+v", got, want)
	}
	if got.ServerVersion != "0.4.0" || got.LatestClient != "0.4.0" {
		t.Fatalf("status version tail lost: %q %q", got.ServerVersion, got.LatestClient)
	}
	if got.Presence[0].Party != "WOLVES" {
		t.Fatalf("status party tail lost: %q", got.Presence[0].Party)
	}

	ptok2, pr2, ok2 := DecodePresence(EncodePresence("secret", want.Presence[0]))
	if !ok2 || ptok2 != "secret" || pr2.Party != "WOLVES" {
		t.Fatalf("presence party round-trip: %q %+v %v", ptok2, pr2, ok2)
	}

	ptok, pr, ok := DecodePresence(EncodePresence("secret", want.Presence[0]))
	if !ok || ptok != "secret" || pr.Session != want.Presence[0].Session || pr.State != "menu" {
		t.Fatalf("presence round-trip: got %q %+v %v", ptok, pr, ok)
	}

	ctok, cm, ok := DecodeChat(EncodeChat("secret", want.Chat[0]))
	if !ok || ctok != "secret" || cm.Handle != "alice" || cm.Text != "hello" {
		t.Fatalf("chat round-trip: got %q %+v %v", ctok, cm, ok)
	}
}
