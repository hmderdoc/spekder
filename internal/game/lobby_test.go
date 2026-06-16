package game

import "testing"

// The vote lobby announces commits, and once every human locks in (ENTER) it
// fast-forwards instead of waiting out the full timer.
func TestLobbyVoteFastForwardAndLog(t *testing.T) {
	if len(Maps) == 0 {
		t.Skip("no maps registered")
	}
	w := NewWorld(0, ModeDeathmatch)
	w.Lobby = true
	me := w.AddPlayer([3]float64{}, 0, "DERDOK", BodyTank)
	w.Tanks[me].vote = -1 // as the real lobby entry resets it
	w.Phase, w.Timer = PhaseLobby, lobbyTime

	const dt = 1.0 / 30
	target := 0

	// Commit a vote (not yet locked): one announcement, timer keeps ticking.
	w.Update(dt, map[int]Input{me: {Vote: target}})
	if log := w.DrainVoteLog(); len(log) != 1 || log[0].Who != "DERDOK" || log[0].MapIdx != target {
		t.Fatalf("want one vote event (DERDOK -> map %d), got %+v", target, log)
	}
	// Re-sending the same vote must not re-announce.
	w.Update(dt, map[int]Input{me: {Vote: target}})
	if log := w.DrainVoteLog(); len(log) != 0 {
		t.Fatalf("re-vote should not re-log: %+v", log)
	}
	if w.Timer <= lobbyFastFwd {
		t.Fatalf("timer fast-forwarded without anyone locking in: %.2f", w.Timer)
	}

	// Lock in with ENTER: the only human is ready, so the lobby fast-forwards.
	w.Update(dt, map[int]Input{me: {Vote: target, Ready: true}})
	if w.Match().Ready != 1 {
		t.Fatalf("ready count = %d, want 1", w.Match().Ready)
	}
	if w.Timer > lobbyFastFwd+1e-6 {
		t.Fatalf("timer should fast-forward to %.2f once locked, got %.2f", lobbyFastFwd, w.Timer)
	}
}
