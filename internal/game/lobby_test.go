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
	me := w.AddPlayer([3]float64{}, "DERDOK", BodyTank)
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

	// Lock in with ENTER. With only ONE human present, the lobby holds a last-call
	// window (lobbySoloFloor) instead of launching instantly, so a joiner can slip in.
	w.Update(dt, map[int]Input{me: {Vote: target, Ready: true}})
	if w.Match().Ready != 1 {
		t.Fatalf("ready count = %d, want 1", w.Match().Ready)
	}
	if w.Timer > lobbySoloFloor+1e-6 {
		t.Fatalf("a lone human should fast-forward only to %.2f, got %.2f", lobbySoloFloor, w.Timer)
	}
	if w.Timer <= lobbyFastFwd {
		t.Fatalf("a lone human must NOT fast-forward all the way to %.2f, got %.2f", lobbyFastFwd, w.Timer)
	}

	// A second human readies up: now there's a crowd, so it fast-forwards fully.
	two := w.AddPlayer([3]float64{}, "PARTNER", BodyTank)
	w.Tanks[two].vote = -1
	w.Update(dt, map[int]Input{me: {Vote: target, Ready: true}, two: {Vote: target, Ready: true}})
	if w.Timer > lobbyFastFwd+1e-6 {
		t.Fatalf("two locked humans should fast-forward to %.2f, got %.2f", lobbyFastFwd, w.Timer)
	}
}

// A WAIT vote holds the lobby open (re-arming the timer) instead of launching,
// up to maxLobbyExtends, after which the match starts regardless.
func TestLobbyWaitExtends(t *testing.T) {
	if len(Maps) == 0 {
		t.Skip("no maps registered")
	}
	w := NewWorld(0, ModeDeathmatch)
	w.Lobby = true
	me := w.AddPlayer([3]float64{}, "DERDOK", BodyTank)
	w.Phase, w.Timer = PhaseLobby, lobbyTime

	const dt = 1.0 / 30
	for ext := 1; ext <= maxLobbyExtends; ext++ {
		w.Timer = 0 // force the launch decision this tick
		w.Update(dt, map[int]Input{me: {WaitVote: true}})
		if w.Phase != PhaseLobby {
			t.Fatalf("extension %d: WAIT should keep the lobby open, phase=%v", ext, w.Phase)
		}
		if w.Timer <= lobbyFastFwd {
			t.Fatalf("extension %d: WAIT should re-arm the timer, got %.2f", ext, w.Timer)
		}
	}
	// Cap reached: the next expiry launches despite the WAIT vote.
	w.Timer = 0
	w.Update(dt, map[int]Input{me: {WaitVote: true}})
	if w.Phase == PhaseLobby {
		t.Fatalf("past the extend cap the match must start, still in lobby")
	}
}

// Humans-only benches the CPU pool. Non-team modes drop every bot; team modes
// split humans across both sides and keep one substitute only when odd.
func TestHumansOnlyBenchesBots(t *testing.T) {
	botsGone := func(w *World) (bots, gone int) {
		for i := range w.Tanks {
			if w.Tanks[i].Bot {
				bots++
				if w.Tanks[i].gone {
					gone++
				}
			}
		}
		return
	}
	// FFA (deathmatch), 2 humans: all bots benched.
	w := NewWorld(3, ModeDeathmatch)
	w.AddPlayer([3]float64{}, "A", BodyTank)
	w.AddPlayer([3]float64{}, "B", BodyTank)
	w.humansOnly = true
	w.benchBotsHumansOnly(Rulesets[ModeDeathmatch])
	if b, g := botsGone(w); g != b {
		t.Fatalf("DM humans-only should bench all %d bots, benched %d", b, g)
	}
	// Lone human: humans-only ignored (a solo "humans only" isn't a thing).
	w1 := NewWorld(2, ModeDeathmatch)
	w1.AddPlayer([3]float64{}, "solo", BodyTank)
	w1.humansOnly = true
	w1.benchBotsHumansOnly(Rulesets[ModeDeathmatch])
	if _, g := botsGone(w1); g != 0 {
		t.Fatalf("1 human: bots must NOT be benched, benched %d", g)
	}
	// Team mode (CTF), 3 humans (odd): exactly one substitute bot kept.
	wc := NewWorld(4, ModeCTF)
	for i := 0; i < 3; i++ {
		wc.AddPlayer([3]float64{}, "H", BodyTank)
	}
	wc.humansOnly = true
	wc.assignTeams()
	wc.benchBotsHumansOnly(Rulesets[ModeCTF])
	b, g := botsGone(wc)
	if b-g != 1 {
		t.Fatalf("odd team humans-only should keep 1 substitute bot, kept %d", b-g)
	}
}

// A mid-match joiner takes over a bot slot and lands on their PARTY's team.
func TestJoinTakeoverHonorsParty(t *testing.T) {
	w := NewWorld(4, ModeCTF)
	w.Phase = PhaseActive
	me := w.AddPlayer([3]float64{}, "MATE", BodyTank) // existing party member on team 1
	w.Tanks[me].Team = 1
	w.SetPlayerParty(me, "squad")
	bn := 0
	for i := range w.Tanks { // split the bots across both teams
		if w.Tanks[i].Bot {
			w.Tanks[i].Team = bn % 2
			bn++
		}
	}
	idx := w.JoinPlayer([3]float64{}, "JOINER", "squad", BodyTank)
	if w.Tanks[idx].Bot {
		t.Fatal("joiner should occupy a (former bot) human slot")
	}
	if w.Tanks[idx].Team != 1 {
		t.Fatalf("party joiner should land on their party's team (1), got %d", w.Tanks[idx].Team)
	}
	// A solo joiner (no party) still takes a slot without crashing.
	if si := w.JoinPlayer([3]float64{}, "SOLO", "", BodyTank); w.Tanks[si].Bot {
		t.Fatal("solo joiner should occupy a human slot")
	}
}
