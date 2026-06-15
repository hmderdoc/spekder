package main

import (
	"bufio"
	"fmt"
	"math"
	"strings"
	"time"

	gm "spekder/internal/game"
)

// Point-buy custom vehicle (see VEHICLES.md). Six stats, each tuned in integer
// "pips"; the total pip count is capped by pbBudget so a build is always a
// tradeoff. A stat's value = base + step*level (FireDelay's step is negative, so
// spending pips lowers the delay = faster fire). Render/feel (body, scale, jump)
// come from the chosen chassis; only these sim stats are tuned.

const pbStatCount = 6
const pbBudget = 22 // total pips across all stats (max possible = 6*pbMaxLevel)
const pbMaxLevel = 8

type pbStatDef struct {
	name string
	base float64
	step float64
	unit string
}

var pbStats = [pbStatCount]pbStatDef{
	{"HP", 50, 12.5, ""},      // 50..150
	{"SPEED", 3.5, 0.6, ""},   // 3.5..8.3
	{"TURN", 1.0, 0.2, ""},    // 1.0..2.6
	{"FIRE", 0.9, -0.06, "s"}, // 0.9..0.42 (lower = better)
	{"AMMO", 5, 1.25, ""},     // 5..15
	{"REGEN", 1.0, 0.2, "/s"}, // 1.0..2.6
}

func defaultCustomLevels() [pbStatCount]int { return [pbStatCount]int{4, 4, 3, 4, 3, 4} } // 22 pips

func clampLevel(i, n int) int {
	if n < 0 {
		return 0
	}
	if n > pbMaxLevel {
		return pbMaxLevel
	}
	return n
}

func levelsSum(lv [pbStatCount]int) int {
	s := 0
	for _, v := range lv {
		s += v
	}
	return s
}

func pbValue(i, level int) float64 { return pbStats[i].base + pbStats[i].step*float64(level) }

// customFromLevels converts pip levels to the wire/sim stat block.
func customFromLevels(lv [pbStatCount]int) gm.CustomStats {
	return gm.CustomStats{
		MaxHP:     int(math.Round(pbValue(0, lv[0]))),
		Speed:     pbValue(1, lv[1]),
		HullTurn:  pbValue(2, lv[2]),
		FireDelay: pbValue(3, lv[3]),
		AmmoMax:   math.Round(pbValue(4, lv[4])),
		AmmoRegen: pbValue(5, lv[5]),
	}
}

func pbFormat(i, level int) string {
	v := pbValue(i, level)
	if i == 0 || i == 4 { // HP, AMMO show as integers
		return fmt.Sprintf("%.0f%s", v, pbStats[i].unit)
	}
	return fmt.Sprintf("%.2f%s", v, pbStats[i].unit)
}

// runCustomEditor is the point-buy screen: tune the six stats and pick an
// appearance (any chassis or creature body) within the pip budget, with a live
// auto-framed preview. ENTER saves to the caller's settings and returns the build
// (stats, chassis, body); ESC cancels.
func runCustomEditor(w *bufio.Writer, cols, rows int, ip *input, s *userSettings, dropfile string, color [3]float64) (gm.CustomStats, int, int, bool) {
	lv := s.customLevels
	// Appearance options: every builtin chassis + every creature body (no CUSTOM).
	var aps []vehEntry
	for _, e := range vehicleEntries() {
		if !e.custom {
			aps = append(aps, e)
		}
	}
	apIdx := 0
	for i, e := range aps { // restore the saved appearance
		if e.vehicle == s.customChassis && e.body == s.customBody {
			apIdx = i
			break
		}
	}
	nRows := pbStatCount + 1 // stats + an APPEARANCE row
	sel := 0

	leftW := cols / 3
	if leftW < 30 {
		leftW = 30
	} else if leftW > 38 {
		leftW = 38
	}
	panelCol0 := leftW + 3
	panelW := cols - panelCol0
	twoPane := cols >= 74 && rows >= 18 && panelW >= 10
	panelRow0, previewRows := 4, rows-8
	var pr *Renderer
	if twoPane {
		pr = newRenderer(panelW, 2*previewRows)
		pr.fogCol = [3]float64{0, 0, 0}
	}

	w.WriteString("\x1b[2J\x1b[H")
	fmt.Fprintf(w, "\x1b[2;2H\x1b[1;96mCUSTOM  BUILD\x1b[0m")
	fmt.Fprintf(w, "\x1b[%d;2H\x1b[0;90mup/dn pick  </> adjust  ENTER save  ESC cancel\x1b[0m", rows-1)

	dirty := true
	var panelPrev []byte
	angle := 0.0
	start := time.Now()
	last := start
	budget := time.Second / 12
	save := func() (gm.CustomStats, int, int, bool) {
		s.customLevels = lv
		s.customChassis = aps[apIdx].vehicle
		s.customBody = aps[apIdx].body
		s.hasCustom = true
		saveUserSettings(dropfile, *s)
		return customFromLevels(lv), aps[apIdx].vehicle, aps[apIdx].body, true
	}
	for {
		select {
		case <-ip.quitCh:
			return gm.CustomStats{}, aps[apIdx].vehicle, aps[apIdx].body, false
		default:
		}
	drain:
		for {
			select {
			case k := <-ip.events:
				switch k {
				case mkUp:
					sel, dirty = (sel-1+nRows)%nRows, true
				case mkDown:
					sel, dirty = (sel+1)%nRows, true
				case mkLeft:
					if sel == pbStatCount { // appearance row
						apIdx = (apIdx - 1 + len(aps)) % len(aps)
					} else if lv[sel] > 0 {
						lv[sel]--
					}
					dirty = true
				case mkRight:
					if sel == pbStatCount {
						apIdx = (apIdx + 1) % len(aps)
					} else if lv[sel] < pbMaxLevel && levelsSum(lv) < pbBudget {
						lv[sel]++
					}
					dirty = true
				case mkEnter:
					return save()
				}
			default:
				break drain
			}
		}
		now := time.Now()
		if twoPane {
			dt := now.Sub(last).Seconds()
			last = now
			angle += 0.7 * dt
			tank := gm.TankSnap{Vehicle: aps[apIdx].vehicle, Body: aps[apIdx].body, Color: color}
			tris := appendTank(nil, &tank, now.Sub(start).Seconds())
			pr.renderModel(fitCam(tris, pr.W, pr.H, angle, previewPad(aps[apIdx].body)), tris)
			panelPrev = blitPanel(w, pr, panelPrev, panelCol0, panelRow0)
		}
		if dirty {
			drawCustomPanel(w, leftW, rows, lv, aps[apIdx].name, sel)
			dirty = false
		}
		w.Flush()
		if d := budget - time.Since(now); d > 0 {
			time.Sleep(d)
		}
	}
}

// drawCustomPanel renders the stat bars + pip budget + appearance selector.
func drawCustomPanel(w *bufio.Writer, leftW, rows int, lv [pbStatCount]int, apName string, sel int) {
	pad := func(s string) string {
		if len(s) < leftW {
			return s + strings.Repeat(" ", leftW-len(s))
		}
		return s[:leftW]
	}
	row := 5
	for i := 0; i < pbStatCount; i++ {
		mark, style := "  ", "\x1b[0;37m"
		if i == sel {
			mark, style = "> ", "\x1b[1;30;46m"
		}
		bar := strings.Repeat("\xfe", lv[i]) + strings.Repeat("-", pbMaxLevel-lv[i])
		line := fmt.Sprintf("%s%-6s %s %8s", mark, pbStats[i].name, bar, pbFormat(i, lv[i]))
		fmt.Fprintf(w, "\x1b[%d;2H%s%s\x1b[0m", row, style, pad(line))
		row++
	}
	row++
	used := levelsSum(lv)
	col := "\x1b[0;37m"
	if used >= pbBudget {
		col = "\x1b[1;33m" // at the cap
	}
	fmt.Fprintf(w, "\x1b[%d;2H%s%s\x1b[0m", row, col, pad(fmt.Sprintf("PIPS  %d / %d", used, pbBudget)))
	row += 2
	mark, style := "  ", "\x1b[0;36m"
	if sel == pbStatCount {
		mark, style = "> ", "\x1b[1;30;46m"
	}
	fmt.Fprintf(w, "\x1b[%d;2H%s%s\x1b[0m", row, style, pad(fmt.Sprintf("%sLOOK  %s", mark, apName)))
}
