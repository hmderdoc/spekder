package game

import "testing"

// TestFreeColorDedup: a requested color is honored unless another *human* already
// wears it (bots don't reserve colors); taken picks shift to a free swatch.
func TestFreeColorDedup(t *testing.T) {
	w := &World{}
	w.Tanks = []Tank{
		{Color: SelectColors[0]},            // human wearing swatch 0
		{Bot: true, Color: SelectColors[1]}, // bot wearing swatch 1
	}
	if got := w.freeColor(SelectColors[0]); colorClose(got, SelectColors[0]) {
		t.Fatal("a human-taken color should be reassigned")
	}
	if got := w.freeColor(SelectColors[1]); !colorClose(got, SelectColors[1]) {
		t.Fatal("a bot's color is not reserved; the pick should stand")
	}
	if got := w.freeColor(SelectColors[5]); !colorClose(got, SelectColors[5]) {
		t.Fatal("a free color should pass through unchanged")
	}
}
