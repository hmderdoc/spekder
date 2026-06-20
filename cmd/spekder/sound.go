package main

import (
	"bufio"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ANSI "music" (cterm / SyncTERM). A sound is the sequence
//
//	CSI | <music string> 0x0e        i.e.  ESC [ | <MML> 0x0e
//
// where the music string is a subset of GW-BASIC PLAY (MML). We prefix MB so it
// plays in the BACKGROUND (MF would freeze the terminal until it finished). The
// 0x0e (^N) is the terminator: cterm plays the buffer and leaves music mode the
// instant it sees it (cterm.c play_music). We used to append a 0x0f (shift-in)
// as a guard for dumb VT terminals, but on cterm the 0x0e is consumed (never
// acts as shift-out) so the lone 0x0f rendered as a stray CP437 sun glyph.
// Spekder is a CP437 door, so the raw bytes pass through untouched. The beeper
// is MONOPHONIC, so sounds interrupt one
// another - hence in-game we use short event blips and the background music is
// suspended while a match is active.
//
// Two producers write sound: the game loop (SFX, via the bufio writer) and the
// background music goroutine (via a lockedWriter). The whole music sequence is
// emitted in ONE write so the lockedWriter can keep the two from splitting each
// other's escape sequences. Users opt out via Options -> SOUND.

// soundOn mirrors userSettings.sound; musicSuspended is set true during active
// gameplay and the map editor. Both are read by the music goroutine and written
// by UI goroutines, so they're atomic.
var (
	soundOn        atomic.Bool
	musicSuspended atomic.Bool
	sfxOn          atomic.Bool // in-game sound effects (master must also be on)
	gameMusicOn    atomic.Bool // play the background music DURING gameplay in lieu of SFX
)

func init() {
	soundOn.Store(true)
	sfxOn.Store(true)
}

// setSoundOn / suspendMusic are the UI-side controls.
func setSoundOn(on bool)    { soundOn.Store(on) }
func suspendMusic(v bool)   { musicSuspended.Store(v) }
func setSfxOn(on bool)      { sfxOn.Store(on) }
func setGameMusic(on bool)  { gameMusicOn.Store(on) }

// songReq lets the SOUND TEST hand a specific song to the background loop to play
// immediately (buffered 1; newest wins).
var songReq = make(chan musSong, 1)

func requestSong(s musSong) {
	select {
	case <-songReq: // drop a stale pending request
	default:
	}
	select {
	case songReq <- s:
	default:
	}
}

// The song the background loop is currently playing + when it started, so the
// SOUND TEST visualizer can hook the live music (not just songs it generated).
var (
	musCurMu    sync.Mutex
	musCurSong  musSong
	musCurStart time.Time
	musCurSet   bool
)

func setCurrentSong(s musSong, t time.Time) {
	musCurMu.Lock()
	musCurSong, musCurStart, musCurSet = s, t, true
	musCurMu.Unlock()
}

// nowPlayingSong returns the loop's current song, its start time, and whether one
// is playing.
func nowPlayingSong() (musSong, time.Time, bool) {
	musCurMu.Lock()
	defer musCurMu.Unlock()
	return musCurSong, musCurStart, musCurSet
}

// sfxEnabled reports whether in-game effects should play: master + SFX on, and
// not overridden by in-game background music.
func sfxEnabled() bool { return soundOn.Load() && sfxOn.Load() && !gameMusicOn.Load() }

// lockedWriter serializes the music goroutine's writes with the UI's bufio
// flushes so the two never interleave mid-escape-sequence. Each Write is one
// atomic, locked call (bufio.Flush writes its whole buffer in a single Write, and
// emitMusic writes a whole music sequence in a single Write).
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

var lastHitSfx time.Time // rate-limit the hit blip

const hitSfxGap = 110 * time.Millisecond

// emitMusic writes a complete background ANSI-music sequence in one write.
func emitMusic(w io.Writer, body string) {
	if !soundOn.Load() {
		return
	}
	io.WriteString(w, "\x1b[|MB"+body+"\x0e")
}

// sfxHit is a short, low, descending blip for when the local player takes damage.
func sfxHit(w *bufio.Writer, now time.Time) {
	if now.Sub(lastHitSfx) < hitSfxGap {
		return
	}
	lastHitSfx = now
	emitMusic(w, "T220O2L32GEC")
}

// sfxKill is a short, bright, rising stinger for a frag.
func sfxKill(w *bufio.Writer) { emitMusic(w, "T200O4L16CEG>C") }

// sfxDeath is a falling, somber phrase for the local player's death / kill-cam.
func sfxDeath(w *bufio.Writer) { emitMusic(w, "T120O3L8GED+O2L4A+") }

// sfxTick is the medium pre-match countdown beep (5..2); sfxGo is the higher,
// longer "GO" tone on 1.
func sfxTick(w *bufio.Writer) { emitMusic(w, "T240L16O4C") }
func sfxGo(w *bufio.Writer)   { emitMusic(w, "T200L8O5C") }

// sfxFlag is the flag-pickup jingle: a bright, rising major arpeggio that resolves
// up an octave - a cheerful "got it!" distinct from the lower kill arpeggio.
func sfxFlag(w *bufio.Writer) { emitMusic(w, "T220O5L16CEGE>L8C") }

// sfxPickup is a quick, bright two-note blip for grabbing a power-up/ammo/repair -
// shorter and lighter than the flag jingle so the two don't read alike.
func sfxPickup(w *bufio.Writer) { emitMusic(w, "T240O5L16CE") }

// sfxBounce is a fast rising run - a "whoop" launch - for hitting a trampoline.
func sfxBounce(w *bufio.Writer) { emitMusic(w, "T255O2L32CEGO3CEG") }

// sfxBreak is a low, short descending "crunch" for a destructible wall/item
// shattering - down in the bass so it reads as rubble, not a tune.
func sfxBreak(w *bufio.Writer) { emitMusic(w, "T240O2L16ECO1L8G") }

// --- background music --------------------------------------------------------
// A single goroutine composes and plays music on its own clock, so it keeps
// playing across ANY non-gameplay screen (menus, submenus, the online lobby, the
// attract demo) without each loop having to drive it. It writes through the
// lockedWriter, and pauses whenever sound is off or music is suspended (active
// gameplay / editor), cutting the still-queued tune with a single short note.

// cutMusic stops whatever is still queued on the terminal (a new note preempts
// the queue - see the ANSI-music-stop reference). Always emitted, even with sound
// "off", since its job is to silence.
func cutMusic(w io.Writer) { io.WriteString(w, "\x1b[|MBT255L64O2C\x0e") }

// startMusicLoop launches the background composer/player, writing through out
// (the lockedWriter). Runs for the life of the process.
func startMusicLoop(out io.Writer) {
	go func() {
		tick := time.NewTicker(120 * time.Millisecond)
		defer tick.Stop()
		var (
			song       musSong
			level, sec int
			next       time.Time
			playing    bool
		)
		begin := func(now time.Time) {
			song = composeSong()
			level, sec = 0, 0
			if len(song.levels) == 0 {
				return
			}
			setCurrentSong(song, now)
			emitMusic(out, song.levels[0][0])
			next, playing = now.Add(song.durs[0][0]), true
		}
		advance := func(now time.Time) {
			sec++
			if sec >= len(song.levels[level]) {
				sec, level = 0, level+1
				if level >= len(song.levels) { // every variation played: compose a fresh song
					level, song = 0, composeSong()
					setCurrentSong(song, now)
				}
			}
			emitMusic(out, song.levels[level][sec])
			next = now.Add(song.durs[level][sec])
		}
		for range tick.C {
			now := time.Now()
			if !soundOn.Load() || musicSuspended.Load() {
				if playing {
					cutMusic(out)
					playing = false
				}
				continue
			}
			select {
			case rs := <-songReq: // SOUND TEST asked for a specific song: play it at once
				song, level, sec = rs, 0, 0
				if len(song.levels) > 0 {
					setCurrentSong(song, now)
					emitMusic(out, song.levels[0][0])
					next, playing = now.Add(song.durs[0][0]), true
				}
				continue
			default:
			}
			switch {
			case !playing:
				begin(now)
			case !now.Before(next):
				advance(now)
			}
		}
	}()
}
