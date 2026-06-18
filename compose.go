package main

import (
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// --- music composition ------------------------------------------------------
// Songs are built from scale DEGREES against a key (root pitch + scale), so a
// riff transposes diatonically onto each chord of a progression. On one
// (monophonic) voice harmony is IMPLIED: the line arpeggiates each chord's tones
// - plus an interleaved bass note - as the progression moves, so the ear hears
// the changes with no second voice. A song is 32 bars in an AABA form; the player
// (sound.go) composes a fresh one after songPlayCount plays. Each 8-bar section is
// rendered as one legato MML string (smooth within a phrase; a breath between).

type musScale struct {
	name string
	off  [7]int // semitone offset of each scale degree (1..7) from the root
}

var musScales = []musScale{
	{"major", [7]int{0, 2, 4, 5, 7, 9, 11}},
	{"natural minor", [7]int{0, 2, 3, 5, 7, 8, 10}},
	{"harmonic minor", [7]int{0, 2, 3, 5, 7, 8, 11}}, // raised 7th -> a major V
	{"dorian", [7]int{0, 2, 3, 5, 7, 9, 10}},
	{"mixolydian", [7]int{0, 2, 4, 5, 7, 9, 10}}, // flat 7
}

func (s musScale) minorish() bool { return s.name != "major" && s.name != "mixolydian" }

// degreePitch is the absolute semitone (0 = C0) of scale degree d (1-based; may
// exceed 7 or go below 1 to climb/drop octaves) above the given root pitch.
func degreePitch(root int, s musScale, d int) int {
	d--
	oct := d / 7
	i := d % 7
	if i < 0 {
		i += 7
		oct--
	}
	return root + 12*oct + s.off[i]
}

var noteNames = [12]string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}

// pitchMML renders a pitch as an MML token, prefixing the octave only when it
// changes from prevOct (octave persists within one music string). Returns the
// token and the new current octave.
func pitchMML(p, prevOct int, lengthTok string) (string, int) {
	if p < 0 {
		p = 0
	}
	oct := p / 12
	if oct > 6 {
		oct = 6
	}
	tok := ""
	if oct != prevOct {
		tok = "O" + strconv.Itoa(oct)
	}
	return tok + noteNames[p%12] + lengthTok, oct
}

// lenTok maps a duration in sixteenth-ticks to an MML length token (the reciprocal
// of the whole-note fraction; a trailing "." is the x1.5 dotted value).
func lenTok(ticks int) string {
	switch ticks {
	case 1:
		return "16"
	case 2:
		return "8"
	case 3:
		return "8."
	case 4:
		return "4"
	case 6:
		return "4."
	case 8:
		return "2"
	case 12:
		return "2."
	case 16:
		return "1"
	}
	return "8"
}

// rnote is one riff step: role picks a tone relative to the current chord; ticks
// is its length in sixteenths.
type rnote struct {
	role  byte // B bass root, b bass 5th, 1 root, 3 third, 5 fifth, 7 seventh, 8 octave root, - rest
	ticks int
}

type riff []rnote // one bar (sums to 16 sixteenth-ticks)

// riffs: intervallic/arpeggiated motifs (NOT scale runs), with rhythm and bass
// interleave so a single voice implies bass + harmony.
var riffs = []riff{
	{{'1', 2}, {'3', 2}, {'5', 2}, {'8', 2}, {'7', 2}, {'5', 2}, {'3', 4}},                                           // arpeggio up & down
	{{'B', 2}, {'5', 2}, {'1', 2}, {'5', 2}, {'B', 2}, {'5', 2}, {'8', 2}, {'5', 2}},                                 // oom-pah (bass/chord)
	{{'1', 3}, {'-', 1}, {'5', 2}, {'8', 3}, {'-', 1}, {'5', 2}, {'3', 2}, {'-', 2}},                                 // syncopated
	{{'8', 4}, {'7', 2}, {'5', 2}, {'3', 4}, {'1', 4}},                                                               // descending call
	{{'1', 2}, {'B', 2}, {'3', 2}, {'5', 2}, {'8', 4}, {'5', 2}, {'3', 2}},                                           // bass walk + arp
	{{'B', 4}, {'8', 2}, {'5', 2}, {'b', 4}, {'3', 2}, {'1', 2}},                                                     // half-time bass + answer
	{{'1', 3}, {'1', 1}, {'5', 2}, {'8', 3}, {'8', 1}, {'5', 2}, {'3', 4}},                                           // gallop (dotted)
	{{'1', 2}, {'8', 2}, {'1', 2}, {'8', 2}, {'5', 4}, {'3', 2}, {'1', 2}},                                           // octave bounce
	{{'-', 2}, {'1', 2}, {'-', 2}, {'5', 2}, {'-', 2}, {'8', 2}, {'5', 2}, {'3', 2}},                                 // off-beat stabs
	{{'1', 8}, {'3', 2}, {'5', 2}, {'8', 4}},                                                                         // held root + climb
	{{'B', 2}, {'b', 2}, {'1', 2}, {'B', 2}, {'5', 2}, {'3', 2}, {'1', 4}},                                           // walking bass
	{{'5', 2}, {'3', 2}, {'1', 4}, {'5', 2}, {'8', 2}, {'7', 4}},                                                     // call & response
	{{'1', 1}, {'3', 1}, {'5', 1}, {'8', 1}, {'5', 2}, {'3', 2}, {'1', 1}, {'3', 1}, {'5', 1}, {'8', 1}, {'7', 2}, {'5', 2}}, // fast arpeggio run
	{{'B', 2}, {'8', 2}, {'-', 1}, {'8', 1}, {'5', 2}, {'B', 2}, {'3', 2}, {'5', 2}, {'8', 2}},                       // syncopated bass + high
}

// rolePitch resolves a riff role to an absolute pitch against the chord on scale
// degree cd. ok=false for a rest.
func rolePitch(root int, s musScale, cd int, role byte) (p int, ok bool) {
	switch role {
	case '-':
		return 0, false
	case 'B':
		return degreePitch(root, s, cd) - 12, true
	case 'b':
		return degreePitch(root, s, cd+4) - 12, true
	case '1':
		return degreePitch(root, s, cd), true
	case '3':
		return degreePitch(root, s, cd+2), true
	case '5':
		return degreePitch(root, s, cd+4), true
	case '7':
		return degreePitch(root, s, cd+6), true
	case '8':
		return degreePitch(root, s, cd) + 12, true
	}
	return degreePitch(root, s, cd), true
}

// Chord progressions as scale degrees, one chord PER BAR (any length - a 12-bar
// blues is just a 12-entry row). Chord QUALITY comes from the scale automatically,
// so the same degree set sounds major in a major scale and minor in a minor one.
// Major/dominant context (major & mixolydian scales): pop, blues, jazz flavors.
var progsMajorCtx = [][]int{
	{1, 4, 5, 1},                         // I-IV-V  (3-chord pop)
	{1, 4, 5, 4},                         // I-IV-V-IV
	{1, 5, 6, 4},                         // I-V-vi-IV (pop)
	{1, 6, 4, 5},                         // I-vi-IV-V (50s doo-wop)
	{6, 4, 1, 5},                         // vi-IV-I-V
	{2, 5, 1, 1},                         // ii-V-I (jazz)
	{1, 6, 2, 5},                         // I-vi-ii-V (rhythm-changes turnaround)
	{1, 1, 1, 1, 4, 4, 1, 1, 5, 4, 1, 5}, // 12-bar blues
}

// Minor context (natural/harmonic minor, dorian).
var progsMinorCtx = [][]int{
	{1, 6, 3, 7},                         // i-VI-III-VII
	{1, 4, 5, 1},                         // i-iv-V (harmonic minor -> major V pull)
	{1, 7, 6, 7},                         // i-VII-VI-VII
	{1, 4, 1, 5},                         // i-iv-i-V
	{1, 6, 7, 5},                         // i-VI-VII-V
	{2, 5, 1, 1},                         // ii-V-i (minor jazz)
	{1, 1, 1, 1, 4, 4, 1, 1, 5, 5, 1, 1}, // minor 12-bar blues
}

type musSong struct {
	desc   string            // key/scale
	levels [][]string        // [variation step][section] -> MML; step 0 is the untouched base
	durs   [][]time.Duration // matching section durations (transforms preserve bar length)
}

func pickRiffs(n int) []riff {
	idx := rand.Perm(len(riffs))
	out := make([]riff, 0, n)
	for i := 0; i < n && i < len(idx); i++ {
		out = append(out, riffs[idx[i]])
	}
	return out
}

// renderSection renders 8 bars: bar i takes prog[i] (looping the 4-chord set) with
// riff rs[i] (looping the chosen riffs). Returns the legato MML string and its play
// duration (plus a small breath at the end so the next section doesn't clip).
func renderSection(root int, s musScale, prog []int, rs []riff, tempo int, xforms []riffXform, finale bool) (string, time.Duration) {
	var b strings.Builder
	b.WriteString("MLT")
	b.WriteString(strconv.Itoa(tempo))
	oct, ticks := -1, 0
	bars := len(prog) // a short progression fills out to 8 bars; a long one (12-bar blues) plays as-is
	if bars < 8 {
		bars = 8
	}
	for bar := 0; bar < bars; bar++ {
		if finale && bar == bars-1 {
			// Walk-out: a descending scale run that lands on the tonic, leading into
			// the next song (which shares this tonal centre - see composeSong).
			for _, d := range []int{8, 7, 6, 5, 4, 3, 2, 1} {
				tok, no := pitchMML(degreePitch(root, s, d), oct, "8")
				oct = no
				b.WriteString(tok)
				ticks += 2
			}
			continue
		}
		cd := prog[bar%len(prog)]
		rf := applyXforms(rs[bar%len(rs)], xforms) // base riff, reshaped by this step's variation transforms
		for _, n := range rf {
			lt := lenTok(n.ticks)
			ticks += n.ticks
			if n.role == '-' {
				b.WriteString("P" + lt)
				continue
			}
			p, _ := rolePitch(root, s, cd, n.role)
			tok, no := pitchMML(p, oct, lt)
			oct = no
			b.WriteString(tok)
		}
	}
	sixteenth := time.Minute / time.Duration(tempo) / 4 // 60/tempo/4 sec
	return b.String(), time.Duration(ticks)*sixteenth + 150*time.Millisecond
}

// --- riff variation transforms ---------------------------------------------
// A song repeats 3x; each repeat reshapes its riffs with a transform (or none).
// EVERY transform MUST preserve a bar's 16 ticks and stay on the riff's own
// chord/scale tones, so timing never drifts and harmony holds; the base riff is
// reshaped, never destroyed. COMPLICATE adds motion/density, SIMPLIFY thins out.
type riffXform func(riff) riff

func applyXforms(rf riff, xs []riffXform) riff {
	for _, x := range xs {
		rf = x(rf)
	}
	return rf
}

// ornamentRole picks an accent tone for an ornament - favours the octave (that
// "octave arpeggiation" feel), else a neighbour chord tone.
func ornamentRole(r byte, i int) byte {
	switch r {
	case '1', '3':
		if i%2 == 0 {
			return '8'
		}
		return '5'
	case '5':
		if i%2 == 0 {
			return '8'
		}
		return '7'
	case '7', '8':
		return '5'
	case 'B', 'b':
		return '1'
	}
	return '8'
}

// COMPLICATE: ornament/runs - split a note's tail into a quick accent note.
func xOrnament(rf riff) riff {
	out := make(riff, 0, len(rf)+4)
	for i, n := range rf {
		if n.role == '-' || n.ticks < 2 || rand.Float64() < 0.5 {
			out = append(out, n)
			continue
		}
		acc := ornamentRole(n.role, i)
		switch {
		case n.ticks >= 4:
			out = append(out, rnote{n.role, n.ticks - 2}, rnote{acc, 1}, rnote{n.role, 1})
		case n.ticks == 3:
			out = append(out, rnote{n.role, 2}, rnote{acc, 1})
		default:
			out = append(out, rnote{n.role, 1}, rnote{acc, 1})
		}
	}
	return out
}

// COMPLICATE: octave arpeggio - fill a long mid/high tone with a 1-3-5-8 climb.
func xOctaveArp(rf riff) riff {
	seq := []byte{'1', '3', '5', '8'}
	out := make(riff, 0, len(rf)+6)
	for _, n := range rf {
		if n.role == '-' || n.role == 'B' || n.role == 'b' || n.ticks < 4 || rand.Float64() < 0.4 {
			out = append(out, n)
			continue
		}
		base, extra := n.ticks/len(seq), n.ticks%len(seq)
		for j, r := range seq {
			t := base
			if j < extra {
				t++
			}
			if t > 0 {
				out = append(out, rnote{r, t})
			}
		}
	}
	return out
}

// COMPLICATE: light shuffle - keep the rhythm, swap the roles of two interior
// (non-downbeat) notes among the riff's own tones, so it stays recognisable.
func xShuffle(rf riff) riff {
	var idx []int
	for i := 1; i < len(rf); i++ {
		if rf[i].role != '-' {
			idx = append(idx, i)
		}
	}
	if len(idx) < 2 {
		return rf
	}
	out := append(riff(nil), rf...)
	a, b := idx[rand.Intn(len(idx))], idx[rand.Intn(len(idx))]
	out[a].role, out[b].role = out[b].role, out[a].role
	return out
}

// COMPLICATE: light syncopation - push one interior note off the beat (a rest
// steals its first tick).
func xSyncopate(rf riff) riff {
	out := make(riff, 0, len(rf)+2)
	pushed := false
	for i, n := range rf {
		if !pushed && i > 0 && n.role != '-' && n.ticks >= 2 && rand.Float64() < 0.5 {
			out = append(out, rnote{'-', 1}, rnote{n.role, n.ticks - 1})
			pushed = true
			continue
		}
		out = append(out, n)
	}
	return out
}

// SIMPLIFY: skeleton - merge adjacent notes into longer held tones (fewer notes).
func xSkeleton(rf riff) riff {
	out := make(riff, 0, len(rf))
	for i := 0; i < len(rf); {
		if i+1 < len(rf) && rf[i].role != '-' && rf[i+1].role != '-' {
			out = append(out, rnote{rf[i].role, rf[i].ticks + rf[i+1].ticks})
			i += 2
		} else {
			out = append(out, rf[i])
			i++
		}
	}
	return out
}

// SIMPLIFY: roots+bass - collapse busier tones (3rds/7ths) toward the root for a
// sparser harmonic outline; rhythm unchanged.
func xRootsBass(rf riff) riff {
	out := append(riff(nil), rf...)
	for i := range out {
		switch out[i].role {
		case '3', '7':
			out[i].role = '1'
		case 'b':
			out[i].role = 'B'
		}
	}
	return out
}

// SIMPLIFY: rest-thin - rest out some weak-beat notes for space (downbeat kept).
func xRestThin(rf riff) riff {
	out := append(riff(nil), rf...)
	for i := 1; i < len(out); i++ {
		if out[i].role != '-' && rand.Float64() < 0.4 {
			out[i].role = '-'
		}
	}
	return out
}

var complicateXforms = []riffXform{xOrnament, xOctaveArp, xShuffle, xSyncopate}
var simplifyXforms = []riffXform{xSkeleton, xRootsBass, xRestThin}

// makePlan returns one variation step per repeat (3). Step 0 is the untouched
// base; the rest form a build/wave/bookend arc, picked at random for diversity.
func makePlan() [][]riffXform {
	c1 := complicateXforms[rand.Intn(len(complicateXforms))]
	c2 := complicateXforms[rand.Intn(len(complicateXforms))]
	s1 := simplifyXforms[rand.Intn(len(simplifyXforms))]
	switch rand.Intn(3) {
	case 0: // build: base -> complicate -> busier
		return [][]riffXform{nil, {c1}, {c1, c2}}
	case 1: // wave: base -> simplify -> complicate
		return [][]riffXform{nil, {s1}, {c1}}
	default: // bookend: simplify -> base -> complicate
		return [][]riffXform{{s1}, nil, {c1}}
	}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func progIsBlues(p []int) bool { return len(p) >= 12 }

func sameProg(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pickProg draws a progression that isn't `avoid` (and isn't blues when avoidBlues),
// so consecutive songs - and the two sections of one song - differ.
func pickProg(pool [][]int, avoidBlues bool, avoid []int) []int {
	for tries := 0; tries < 16; tries++ {
		p := pool[rand.Intn(len(pool))]
		if (avoidBlues && progIsBlues(p)) || (avoid != nil && sameProg(p, avoid)) {
			continue
		}
		return p
	}
	for _, p := range pool { // fallback: any non-blues
		if !avoidBlues || !progIsBlues(p) {
			return p
		}
	}
	return pool[0]
}

// lastMus remembers the previous song so the next one is audibly different and
// transitions smoothly (pitch axis: hold the tonal centre, change the mode).
var lastMus struct {
	scaleIdx, root, tempo int
	progA                 []int
	have                  bool
}

// composeSong builds a fresh AABA song, deliberately different from the previous
// one - new mode, tempo, and progression (never blues-after-blues), with the key
// held on a pitch axis (same tonal centre, sometimes a 4th/5th modulation) so the
// hand-off is smooth. It renders once per variation step (the song repeats 3x
// through its variation arc), and the final pass walks out to the tonic.
func composeSong() musSong {
	// Mode: change from last song (big feel shift; smooth because the root holds).
	si := rand.Intn(len(musScales))
	for tries := 0; lastMus.have && si == lastMus.scaleIdx && tries < 8; tries++ {
		si = rand.Intn(len(musScales))
	}
	s := musScales[si]

	// Key (pitch axis): keep the previous tonal centre, sometimes modulate a 4th/5th.
	root := 36 + rand.Intn(13) // C3..C4 for the first song
	if lastMus.have {
		root = lastMus.root
		if rand.Intn(2) == 0 {
			root += []int{5, 7, -5, -7}[rand.Intn(4)]
		}
		for root < 33 {
			root += 12
		}
		for root > 50 {
			root -= 12
		}
	}

	// Tempo: noticeably different from last.
	tempo := 116 + rand.Intn(34)
	for tries := 0; lastMus.have && absInt(tempo-lastMus.tempo) < 12 && tries < 8; tries++ {
		tempo = 116 + rand.Intn(34)
	}

	progSet := progsMajorCtx
	if s.minorish() {
		progSet = progsMinorCtx
	}
	// Progression: differ from last song (no blues-after-blues); the two sections
	// differ too, and never both blues (which would make the song drag).
	avoidBluesA := lastMus.have && progIsBlues(lastMus.progA)
	progA := pickProg(progSet, avoidBluesA, nil)
	if lastMus.have && sameProg(progA, lastMus.progA) {
		progA = pickProg(progSet, avoidBluesA, lastMus.progA)
	}
	progB := pickProg(progSet, progIsBlues(progA), progA)

	rA, rB := pickRiffs(2), pickRiffs(2)
	plan := makePlan()
	blues := progIsBlues(progA)
	var levels [][]string
	var durs [][]time.Duration
	for li, step := range plan {
		finale := li == len(plan) - 1 // final pass walks out to the tonic on its closing bar
		a, ad := renderSection(root, s, progA, rA, tempo, step, false)
		end, endD := a, ad
		if finale {
			end, endD = renderSection(root, s, progA, rA, tempo, step, true)
		}
		if blues {
			// Blues is played as choruses, not AABA: two 12-bar choruses per pass, so
			// a blues song caps at 2*12*3 = 72 bars instead of bloating to ~132.
			levels = append(levels, []string{a, end})
			durs = append(durs, []time.Duration{ad, endD})
		} else {
			b, bd := renderSection(root, s, progB, rB, tempo, step, false)
			levels = append(levels, []string{a, a, b, end}) // AABA
			durs = append(durs, []time.Duration{ad, ad, bd, endD})
		}
	}
	lastMus.scaleIdx, lastMus.root, lastMus.tempo, lastMus.progA, lastMus.have = si, root, tempo, progA, true
	return musSong{desc: s.name, levels: levels, durs: durs}
}
