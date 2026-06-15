package main

import "strings"

// Lightweight rich-text for the info/help screens. Markup is a tiny, CP437-safe
// dialect (no Unicode, no raw ESC in the source strings): inline **bold** and
// `accent` spans, plus line-level prefixes ("# " header, "## " subhead, "- "
// bullet, "> " indent). It renders to ANSI escapes the same way the rest of the
// menu code does. The point is to retire the flat centered-gray runInfoScreen.

// seg is a run of text sharing one SGR style ("" = no styling / caller default).
type seg struct {
	text string
	sgr  string
}

// rline is one already-laid-out screen line: a left indent plus styled segments.
type rline struct {
	indent int
	segs   []seg
}

// styledLine wraps a whole pre-formatted string in one SGR style. Unlike markup
// it adds no inline escapes mid-string, so column alignment is exact - the right
// tool for leaderboard rows that need both color and fixed columns.
func styledLine(sgr, text string) rline {
	return rline{segs: []seg{{text, sgr}}}
}

// theme maps the markup roles to ANSI escapes so a screen can recolor them.
type theme struct {
	body, head, sub, term, bold string
}

// docTheme is the shared look for HELP / PLAYER CARD / HIGH SCORES.
var docTheme = theme{
	body: "\x1b[0;37m", // white
	head: "\x1b[1;95m", // bright magenta section headers (distinct from the cyan title)
	sub:  "\x1b[1;93m", // bright yellow subheads
	term: "\x1b[1;96m", // bright cyan key terms (`accent`)
	bold: "\x1b[1;97m", // bright white emphasis (**bold**)
}

// parseEmph turns one line of inline markup into styled segments over a base
// style. **...** is bold, `...` is an accent term; everything else is base.
func parseEmph(s, base string, th theme) []seg {
	var segs []seg
	var cur strings.Builder
	bold, term := false, false
	style := func() string {
		switch {
		case bold:
			return th.bold
		case term:
			return th.term
		default:
			return base
		}
	}
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, seg{cur.String(), style()})
			cur.Reset()
		}
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		if r[i] == '*' && i+1 < len(r) && r[i+1] == '*' {
			flush()
			bold = !bold
			i++
			continue
		}
		if r[i] == '`' {
			flush()
			term = !term
			continue
		}
		cur.WriteRune(r[i])
	}
	flush()
	return segs
}

// segWidth is the visible column count of a word/segment list. Content is ASCII
// (or single-byte CP437 glyphs), so rune count == columns.
func segWidth(w []seg) int {
	n := 0
	for _, s := range w {
		n += len([]rune(s.text))
	}
	return n
}

// splitWords breaks styled segments into space-delimited words, each word a
// (possibly multi-style) segment list. Runs of spaces collapse - this is for
// flowing prose; tabular content must NOT go through here (use tableLines).
func splitWords(segs []seg) [][]seg {
	var words [][]seg
	var cur []seg
	flush := func() {
		if len(cur) > 0 {
			words = append(words, cur)
			cur = nil
		}
	}
	for _, s := range segs {
		parts := strings.Split(s.text, " ")
		for i, p := range parts {
			if i > 0 {
				flush()
			}
			if p != "" {
				cur = append(cur, seg{p, s.sgr})
			}
		}
	}
	flush()
	return words
}

// wrapWords greedily packs words into lines no wider than width.
func wrapWords(words [][]seg, width int) [][]seg {
	if width < 1 {
		width = 1
	}
	var lines [][]seg
	var line []seg
	lineW := 0
	for _, wd := range words {
		ww := segWidth(wd)
		if lineW > 0 && lineW+1+ww > width {
			lines = append(lines, line)
			line, lineW = nil, 0
		}
		if lineW > 0 {
			line = append(line, seg{" ", ""})
			lineW++
		}
		line = append(line, wd...)
		lineW += ww
	}
	if len(line) > 0 {
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, nil)
	}
	return lines
}

// renderSegs emits styled segments as an ANSI string (reset after each styled
// run so a later unstyled run can't inherit color).
func renderSegs(segs []seg) string {
	var b strings.Builder
	for _, s := range segs {
		if s.sgr != "" {
			b.WriteString(s.sgr)
		}
		b.WriteString(s.text)
		if s.sgr != "" {
			b.WriteString("\x1b[0m")
		}
	}
	return b.String()
}

// proseLines lays out flowing markup (HELP/PARTY copy) into wrapped rlines.
func proseLines(markup []string, width int, th theme) []rline {
	var out []rline
	for _, raw := range markup {
		if raw == "" {
			out = append(out, rline{})
			continue
		}
		base, indent, hang := th.body, 0, 0
		s := raw
		switch {
		case strings.HasPrefix(s, "# "):
			base, s = th.head, s[2:]
		case strings.HasPrefix(s, "## "):
			base, s = th.sub, s[3:]
		case strings.HasPrefix(s, "- "):
			// ASCII hyphen bullet. hang = 2 so wrapped continuation lines sit
			// under the text, not the bullet.
			indent, hang = 2, 2
		case strings.HasPrefix(s, "> "):
			indent, s = 2, s[2:]
		}
		segs := parseEmph(s, base, th)
		for j, wl := range wrapWords(splitWords(segs), width-indent-hang) {
			ind := indent
			if j > 0 {
				ind += hang
			}
			out = append(out, rline{indent: ind, segs: wl})
		}
	}
	return out
}

// tableLines lays out pre-aligned content (PLAYER CARD / HIGH SCORES columns):
// inline **bold**/`accent` still apply, but spacing is preserved verbatim - no
// word wrap - so column alignment survives. Line-level prefixes still work.
func tableLines(rawLines []string, th theme) []rline {
	var out []rline
	for _, raw := range rawLines {
		if raw == "" {
			out = append(out, rline{})
			continue
		}
		base, indent := th.body, 0
		s := raw
		switch {
		case strings.HasPrefix(s, "# "):
			base, s = th.head, s[2:]
		case strings.HasPrefix(s, "## "):
			base, s = th.sub, s[3:]
		}
		out = append(out, rline{indent: indent, segs: parseEmph(s, base, th)})
	}
	return out
}
