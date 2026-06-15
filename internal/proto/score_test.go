package proto

import "testing"

func TestScoreRoundTrip(t *testing.T) {
	sub := ScoreSubmit{Token: "tok", Mode: "SURVIVAL", Name: "DERDOK", BBS: "Cheese BBS", Map: "WARDEN", Score: 1234, When: 1700000000}
	got, ok := DecodeScore(EncodeScore(sub))
	if !ok || got != sub {
		t.Fatalf("score submit round-trip: %+v vs %+v (ok=%v)", got, sub, ok)
	}
	if tok, ok := DecodeScoreQuery(EncodeScoreQuery("tok")); !ok || tok != "tok" {
		t.Fatalf("score query: %q ok=%v", tok, ok)
	}
	rows := []ScoreRow{
		{Mode: "DEATHMATCH", Name: "A", BBS: "X", Map: "M", Score: 99, When: 1},
		{Mode: "SURVIVAL", Name: "B", BBS: "Y", Map: "WARDEN", Score: 500, When: 2},
	}
	out, ok := DecodeScores(EncodeScores(rows))
	if !ok || len(out) != 2 || out[1].Score != 500 || out[1].BBS != "Y" {
		t.Fatalf("scores round-trip: %+v ok=%v", out, ok)
	}
}
