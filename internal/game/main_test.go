package game

import (
	"math/rand"
	"os"
	"testing"
)

// TestMain seeds the global RNG to a fixed value so the package's bot-behaviour
// tests (KOTH climbs, demo flag runs, duel/team sims) are deterministic run to
// run. Go 1.25 auto-seeds math/rand per process, which made those stochastic
// tests flake and - because redeploy.sh runs `go test` under `set -e` - randomly
// blocked deploys even when nothing was actually broken.
func TestMain(m *testing.M) {
	rand.Seed(1)
	os.Exit(m.Run())
}
