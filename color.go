package main

import (
	"fmt"
	"strings"
)

// Output color depth for everything we draw. Truecolor is the renderer's
// native output; 256 quantizes to the xterm palette (color SGRs are roughly
// half the bytes, kinder to slow links); 16 quantizes to the classic CGA
// palette with ordered dithering, for terminals with no extended-color
// support at all. One door process serves one caller, so a global is safe;
// it only changes in OPTIONS, between render loops (every loop re-enters
// with prev=nil, so there is no stale-delta hazard).
const (
	colorTrue = iota
	color256
	color16
	colorModes // count, for cycling
)

var colorMode = colorTrue

func colorModeName(m int) string {
	switch m {
	case color256:
		return "256-COLOR"
	case color16:
		return "16-COLOR"
	}
	return "TRUECOLOR"
}

// colorModeSlug is the ini-file spelling (see parseColorMode).
func colorModeSlug(m int) string {
	switch m {
	case color256:
		return "256"
	case color16:
		return "16"
	}
	return "truecolor"
}

func parseColorMode(v string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "truecolor", "true", "rgb", "24bit":
		return colorTrue, true
	case "256", "256color", "xterm":
		return color256, true
	case "16", "16color", "classic", "ansi":
		return color16, true
	}
	return 0, false
}

// cdist is a luma-weighted squared RGB distance (green counts most).
func cdist(r, g, b int, p [3]byte) int {
	dr, dg, db := r-int(p[0]), g-int(p[1]), b-int(p[2])
	return 2*dr*dr + 4*dg*dg + 3*db*db
}

// bayer4 is the classic 4x4 ordered-dither threshold matrix.
var bayer4 = [4][4]int{{0, 8, 2, 10}, {12, 4, 14, 6}, {3, 11, 1, 9}, {15, 7, 13, 5}}

// twinPenalty[i] is how much bright color 8+i loses when snapped to its dark
// twin i (used when both halves of a cell want a bright background).
var twinPenalty [8]int

func init() {
	for i := 0; i < 8; i++ {
		c := cgaRGB[8+i]
		twinPenalty[i] = cdist(int(c[0]), int(c[1]), int(c[2]), cgaRGB[i])
	}
}

func clampInt(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// nearest16 maps RGB to the classic 16-color palette with a position-fixed
// ordered-dither bias, so flat gradients blend between the two nearest
// palette colors instead of banding. The bias depends only on screen
// position: a static scene still deltas to nothing frame-over-frame.
func nearest16(r, g, b byte, x, y int) byte {
	d := (bayer4[y&3][x&3]*2 - 15) * 3 // ~ +-45, about half a palette step
	rr, gg, bb := clampInt(int(r)+d), clampInt(int(g)+d), clampInt(int(b)+d)
	best, bestD := 0, 1<<31-1
	for i := range cgaRGB {
		if dd := cdist(rr, gg, bb, cgaRGB[i]); dd < bestD {
			best, bestD = i, dd
		}
	}
	return byte(best)
}

// xterm256 maps RGB to the xterm 256-color palette: the 6x6x6 cube or the
// 24-step gray ramp, whichever lands closer.
func xterm256(r, g, b byte) byte {
	ci := func(v byte) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return int(v-35) / 40
	}
	steps := [6]byte{0, 95, 135, 175, 215, 255}
	cr, cg, cb := ci(r), ci(g), ci(b)
	cubeD := cdist(int(r), int(g), int(b), [3]byte{steps[cr], steps[cg], steps[cb]})
	avg := (int(r) + int(g) + int(b)) / 3
	gi := (avg - 3) / 10
	if gi < 0 {
		gi = 0
	}
	if gi > 23 {
		gi = 23
	}
	gv := byte(8 + 10*gi)
	if cdist(int(r), int(g), int(b), [3]byte{gv, gv, gv}) < cubeD {
		return byte(232 + gi)
	}
	return byte(16 + 36*cr + 6*cg + cb)
}

// packCell16 resolves a half-block cell (top/bottom palette indexes) into
// classic-ANSI fg/bg/glyph. Classic backgrounds only span the 8 dark colors
// (no iCE color assumption), so a bright bottom half flips the glyph to the
// lower-half block - the bright color rides in the foreground; if both
// halves are bright, the one that loses least snaps to its dark twin.
func packCell16(t16, b16 byte) (fg, bg, glyph byte) {
	if t16 == b16 {
		if t16 < 8 {
			return 7, t16, ' ' // solid dark cell: a space on that background
		}
		return t16, 0, 0xDB // solid bright cell: full block
	}
	if t16 >= 8 && b16 >= 8 {
		if twinPenalty[b16-8] <= twinPenalty[t16-8] {
			b16 -= 8
		} else {
			t16 -= 8
		}
		if t16 == b16 { // twins collided (e.g. yellow+white both near gray)
			return 7, t16, ' '
		}
	}
	if b16 >= 8 {
		return b16, t16, 0xDC // lower-half block: fg paints the bottom
	}
	return t16, b16, 0xDF // upper-half block: fg paints the top
}

// sgr16 builds the classic SGR params for a packCell16 result. bg is always
// one of the 8 dark colors there, so plain 40-47 suffices.
func sgr16(fg, bg byte) string {
	if fg < 8 {
		return fmt.Sprintf("0;3%d;4%d", fg, bg)
	}
	return fmt.Sprintf("1;3%d;4%d", fg-8, bg)
}

// quantCell converts one half-block cell (top/bottom pixels at fb[tp:]/fb[bp:])
// into its 6-byte display record for the given mode, written to dst (assumed
// zeroed - both delta encoders allocate cur fresh each frame). The record is
// what the terminal will show: the encoders compare these for their temporal
// delta, so the palette modes also swallow sub-palette color jitter.
func quantCell(mode int, dst, fb []byte, tp, bp, x, y int) {
	switch mode {
	case color256:
		dst[0] = xterm256(fb[tp], fb[tp+1], fb[tp+2])
		dst[1] = xterm256(fb[bp], fb[bp+1], fb[bp+2])
	case color16:
		t16 := nearest16(fb[tp], fb[tp+1], fb[tp+2], x, 2*y)
		b16 := nearest16(fb[bp], fb[bp+1], fb[bp+2], x, 2*y+1)
		dst[0], dst[1], dst[2] = packCell16(t16, b16)
	default:
		copy(dst[0:3], fb[tp:tp+3])
		copy(dst[3:6], fb[bp:bp+3])
	}
}

// cellSGR renders a quantCell record as SGR params plus the glyph to print.
func cellSGR(mode int, c []byte) (string, byte) {
	switch mode {
	case color256:
		return fmt.Sprintf("38;5;%d;48;5;%d", c[0], c[1]), 0xDF
	case color16:
		return sgr16(c[0], c[1]), c[2]
	}
	return fmt.Sprintf("38;2;%d;%d;%d;48;2;%d;%d;%d", c[0], c[1], c[2], c[3], c[4], c[5]), 0xDF
}

// fgSGR returns the SGR params (no ESC[ prefix or m) that best render the
// given RGB as a foreground in the current color mode. The 16-color form
// resets attributes to manage bold, so callers must be on a default (black)
// background - true of every HUD/overlay caller today.
func fgSGR(r, g, b byte) string {
	switch colorMode {
	case color256:
		return fmt.Sprintf("38;5;%d", xterm256(r, g, b))
	case color16:
		i := nearest16(r, g, b, 0, 0)
		if i < 8 {
			return fmt.Sprintf("0;3%d", i)
		}
		return fmt.Sprintf("1;3%d", i-8)
	}
	return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
}

// fgEsc is fgSGR wrapped in the full escape sequence.
func fgEsc(r, g, b byte) string { return "\x1b[" + fgSGR(r, g, b) + "m" }

// barCellSGR returns the SGR params (no ESC[ prefix or m) painting a foreground
// RGB over a background RGB, quantized to the active color mode - so the
// embedded-label stat bars (text on a colored fill) render in every mode. The
// 16-color background is masked to the 8 basic codes (no high-intensity bg).
func barCellSGR(fr, fg, fb, br, bg, bb byte) string {
	switch colorMode {
	case color256:
		return fmt.Sprintf("38;5;%d;48;5;%d", xterm256(fr, fg, fb), xterm256(br, bg, bb))
	case color16:
		return sgr16(nearest16(fr, fg, fb, 0, 0), nearest16(br, bg, bb, 0, 0)&7)
	}
	return fmt.Sprintf("38;2;%d;%d;%d;48;2;%d;%d;%d", fr, fg, fb, br, bg, bb)
}
