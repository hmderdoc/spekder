package main

import (
	"strings"
	"testing"
)

// Guards the paged-doc rendering: markup must be consumed (no stray * or `
// leaks) and no laid-out line may overflow the content width.
func TestHelpRenderSanity(t *testing.T) {
	cols := 80
	width := cols - 8
	for _, pg := range helpPages(cols) {
		for i, ln := range pg.body {
			var plain strings.Builder
			for _, s := range ln.segs {
				plain.WriteString(s.text)
			}
			p := plain.String()
			if strings.Contains(p, "**") {
				t.Errorf("[%s] line %d: leaked bold markup: %q", pg.name, i, p)
			}
			if strings.Contains(p, "`") {
				t.Errorf("[%s] line %d: leaked accent markup: %q", pg.name, i, p)
			}
			if w := len([]rune(p)) + ln.indent; w > width {
				t.Errorf("[%s] line %d: width %d > %d: %q", pg.name, i, w, width, p)
			}
		}
	}
}
