package game

import "testing"

// drive runs the world forward enough ticks for a phase change at the given step.
func drive(w *World, secs, dt float64, in map[int]Input) {
	for t := 0.0; t < secs; t += dt {
		w.Update(dt, in)
	}
}

func TestSurvivalWavesAndLives(t *testing.T) {
	w := NewWorld(0, ModeSurvival)
	me := w.AddPlayer([3]float64{}, 0)
	in := map[int]Input{me: {}}

	// Count in past the countdown so the match goes active and wave 1 spawns.
	drive(w, countdownTime+0.2, 1.0/30, in)
	if w.Phase != PhaseActive {
		t.Fatalf("expected active phase, got %v", w.Phase)
	}
	if w.wave != 1 {
		t.Fatalf("expected wave 1, got %d", w.wave)
	}
	if got := w.activeBots(); got == 0 {
		t.Fatalf("wave 1 should have spawned bots, got %d active", got)
	}
	if w.Tanks[me].lives != survivalLives {
		t.Fatalf("human should start with %d lives, got %d", survivalLives, w.Tanks[me].lives)
	}

	// Kill the whole wave; the wave-clear hook should advance to wave 2.
	for i := range w.Tanks {
		if w.Tanks[i].Bot {
			w.Tanks[i].Dead = true
		}
	}
	w.Update(1.0/30, in)
	if w.wave != 2 {
		t.Fatalf("clearing a wave should advance to wave 2, got %d", w.wave)
	}
	if w.activeBots() == 0 {
		t.Fatalf("wave 2 should have active bots")
	}

	// Burn the human's lives; once out, the match should end (not respawn).
	w.Tanks[me].lives = 1
	w.Tanks[me].Dead = true
	w.Tanks[me].lives-- // simulate the death decrement
	w.Update(1.0/30, in)
	if w.Phase != PhaseEnded {
		t.Fatalf("match should end when the human is out of lives, got %v", w.Phase)
	}
	if w.WinnerID != -1 {
		t.Fatalf("survival is co-op; winner should be -1, got %d", w.WinnerID)
	}
}
