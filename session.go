package main

import gm "spekder/internal/game"

// viewState is what one tick of a session yields: the tanks/projectiles to draw,
// our own authoritative tank (for HUD + flash), and the camera pose. Offline and
// net produce the same struct; net fills camPos/camYaw from client prediction
// and tanks from interpolation.
type viewState struct {
	ready  bool // false until the first server snapshot arrives (net only)
	tanks  []gm.TankSnap
	shots  []gm.V3
	me     int
	self   gm.TankSnap // our tank, authoritative (HUD/flash)
	camPos gm.V3       // our tank ground position (caller adds eye height)
	camYaw float64     // hull + turret

	// viewTurret is the turret offset consistent with camYaw (predicted in net
	// mode, authoritative offline). The body-direction pip = -viewTurret, so it
	// never lags the camera.
	viewTurret float64

	// match lifecycle + Flag Run / CTF state
	mode       gm.Mode
	phase      gm.Phase
	timer      float64
	winnerID   int
	flags      []gm.FlagSnap   // flags to draw (Flag Run pickups / CTF team flags)
	pickups    []gm.PickupSnap // power-up drops to draw
	flagsLeft  int
	flagsTotal int
	votes      [4]int // lobby vote tally per mode index
	mapIdx     int    // active map index
	wave       int    // Survival: current wave
	teamScore  [2]int // CTF: captures per team
	winnerTeam int    // CTF: winning team (-1 = tie/none)
	myTeam     int    // CTF: our team (-1 if none)
	gmap       gm.Map // active map definition (renderer rebuilds geometry on change)
}

// session feeds one tick of input and yields a viewState. Offline runs the sim
// in-process; the net session (net.go) syncs from the arena server. The render
// path in main is identical either way.
type session interface {
	step(dt float64, in gm.Input) viewState
	close()
}

// offlineSession is the local single-player game: bots + us, simulated here.
type offlineSession struct {
	w  *gm.World
	me int
}

func newOfflineSession(numBots int, mode gm.Mode, vehicle int) *offlineSession {
	w := gm.NewWorld(numBots, mode)
	return &offlineSession{w: w, me: w.AddPlayer([3]float64{}, vehicle)}
}

func (s *offlineSession) step(dt float64, in gm.Input) viewState {
	s.w.Update(dt, map[int]gm.Input{s.me: in})
	tanks, shots, flags, pickups := s.w.Snapshot()
	var self gm.TankSnap
	for i := range tanks {
		if tanks[i].ID == s.me {
			self = tanks[i]
			break
		}
	}
	m := s.w.Match()
	return viewState{
		ready: true, tanks: tanks, shots: shots, me: s.me, self: self,
		camPos: self.Pos, camYaw: self.HullYaw + self.TurretYaw, viewTurret: self.TurretYaw,
		mode: m.Mode, phase: m.Phase, timer: m.Timer, winnerID: m.WinnerID,
		flags: flags, pickups: pickups, flagsLeft: m.FlagsLeft, flagsTotal: m.FlagsTotal, mapIdx: m.MapIdx,
		wave: m.Wave, teamScore: m.TeamScore, winnerTeam: m.WinnerTeam, myTeam: self.Team,
		gmap: s.w.ActiveMap(),
	}
}

func (s *offlineSession) close() {}
