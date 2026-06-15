package main

import (
	"bufio"
	"fmt"
	"strings"
	"time"
)

// Title base colors for the paged screens.
const (
	titleGreen   = "\x1b[1;92m" // light green (PLAYER CARD)
	titleRed     = "\x1b[1;91m" // light red (HIGH SCORES)
	titleBlue    = "\x1b[1;94m" // light blue (HELP)
	titleMagenta = "\x1b[1;95m" // light magenta (PARTY)
)

// shimmerTitle renders a screen title in `base` with a 2-char bright band at
// position `phase`, so stepping phase over time sweeps a shimmer across the text
// (with a pause once the band runs off the end). ASCII titles only.
func shimmerTitle(s string, phase int, base string) string {
	const hot = "\x1b[1;97m" // bright sparkle
	var b strings.Builder
	cur := ""
	for i, r := range s {
		want := base
		if d := i - phase; d == 0 || d == 1 {
			want = hot
		}
		if want != cur {
			b.WriteString(want)
			cur = want
		}
		b.WriteRune(r)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// docPage is one section of a paged screen: a short tab name plus its already
// laid-out body lines (build with proseLines or tableLines).
type docPage struct {
	name string
	body []rline
}

// runPagedDoc is the shared viewer behind HELP / PLAYER CARD / HIGH SCORES. It
// shows one section at a time, pages left/right between sections, and scrolls a
// long section up/down. With a single page the section chrome is hidden and it
// reads like a plain styled screen. ENTER/Bksp leave; ESC quits the door.
// rebuild (optional) regenerates the pages for a new terminal width - pass it for
// width-reflowing content like HELP; nil for fixed-width tables that just need a
// re-centered, re-heighted layout on resize.
func runPagedDoc(w *bufio.Writer, cols, rows int, ip *input, screenTitle, titleSGR string, pages []docPage, rebuild func(cols int) []docPage) {
	if len(pages) == 0 {
		pages = []docPage{{body: tableLines([]string{"(nothing here yet)"}, docTheme)}}
	}
	pg, scroll := 0, 0
	leftCol, phase := 5, 0
	// Title on the top row, footer on the very last row, content filling the rest.
	// A multi-page screen gets an airy header band - title, blank, section line,
	// blank, then the list - while a single nameless page hugs the title. relayout
	// recomputes all of this from the current cols/rows (re-run on a resize).
	var footRow, chromeRow, contentTop, viewH, titleCol int
	relayout := func() {
		footRow = rows
		chromeRow, contentTop = 0, 2
		switch {
		case len(pages) > 1:
			chromeRow, contentTop = 3, 5 // title r1 / gap / selector r3 / gap / content r5
		case pages[0].name != "":
			chromeRow, contentTop = 2, 3
		}
		viewH = footRow - 1 - contentTop + 1 // content rows contentTop..footRow-1
		if viewH < 1 {
			viewH = 1
		}
		titleCol = (cols-len(screenTitle))/2 + 1
	}
	relayout()

	// paintTitle repaints just the (shimmering) title row, cheap enough to run on
	// the animation ticker without touching the rest of the screen.
	paintTitle := func() {
		fmt.Fprintf(w, "\x1b[1;%dH%s", titleCol, shimmerTitle(screenTitle, phase, titleSGR))
		w.Flush()
	}

	draw := func() {
		body := pages[pg].body
		maxScroll := len(body) - viewH
		if maxScroll < 0 {
			maxScroll = 0
		}
		if scroll > maxScroll {
			scroll = maxScroll
		}
		if scroll < 0 {
			scroll = 0
		}

		var b strings.Builder
		b.WriteString("\x1b[2J\x1b[H")
		fmt.Fprintf(&b, "\x1b[1;%dH%s", titleCol, shimmerTitle(screenTitle, phase, titleSGR))

		// Section chrome: < name (n/m) > when there's more than one page. The
		// arrows are white (they signal navigation), the section text yellow.
		if len(pages) > 1 {
			inner := fmt.Sprintf("%s  (%d/%d)", pages[pg].name, pg+1, len(pages))
			plain := "< " + inner + " >"
			fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[1;97m< \x1b[1;93m%s\x1b[1;97m >\x1b[0m", chromeRow, (cols-len(plain))/2+1, inner)
		} else if pages[pg].name != "" {
			fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[1;93m%s\x1b[0m", chromeRow, (cols-len(pages[pg].name))/2+1, pages[pg].name)
		}

		for i := 0; i < viewH && scroll+i < len(body); i++ {
			ln := body[scroll+i]
			if len(ln.segs) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\x1b[%d;%dH%s", contentTop+i, leftCol+ln.indent, renderSegs(ln.segs))
		}

		foot := "ENTER / Bksp  back     ESC  quit"
		if len(pages) > 1 {
			foot = "left/right  section    up/dn  scroll    ENTER / Bksp  back"
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", footRow, (cols-len(foot))/2+1, foot)
		// Scroll affordance (only when the section overflows): far right of the footer.
		if len(body) > viewH {
			more := fmt.Sprintf("[ %d-%d of %d ]", scroll+1, min(scroll+viewH, len(body)), len(body))
			fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[0;90m%s\x1b[0m", footRow, cols-len(more), more)
		}
		w.WriteString(b.String())
		w.Flush()
	}

	draw()
	// Title shimmer: a slow sweep, with a pause once the band runs off the end.
	shimmer := time.NewTicker(130 * time.Millisecond)
	defer shimmer.Stop()
	resizeTick := time.NewTicker(1500 * time.Millisecond)
	defer resizeTick.Stop()
	for {
		select {
		case <-ip.quitCh:
			return
		case <-shimmer.C:
			phase++
			if phase > len(screenTitle)+10 {
				phase = 0
			}
			paintTitle()
		case <-resizeTick.C:
			w.WriteString("\x1b[18t") // request a window report (telnet-safe live resize)
			w.Flush()
			if c, r := ip.winSize(); c >= 20 && r >= 8 && (c != cols || r != rows) {
				cols, rows = c, r
				if rebuild != nil {
					if np := rebuild(cols); len(np) > 0 {
						pages = np
						if pg >= len(pages) {
							pg = len(pages) - 1
						}
						scroll = 0
					}
				}
				relayout()
				draw()
			}
		case k := <-ip.events:
			switch k {
			case mkEnter, mkBack:
				return
			case mkLeft:
				if len(pages) > 1 {
					pg = (pg - 1 + len(pages)) % len(pages)
					scroll = 0
					draw()
				}
			case mkRight:
				if len(pages) > 1 {
					pg = (pg + 1) % len(pages)
					scroll = 0
					draw()
				}
			case mkUp:
				if scroll > 0 {
					scroll--
					draw()
				}
			case mkDown:
				scroll++
				draw()
			}
		}
	}
}
