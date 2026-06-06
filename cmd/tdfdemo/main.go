// tdfdemo renders text with an embedded TheDraw font, for previewing/validating
// the tdf package:  tdfdemo <font> <text...>
package main

import (
	"fmt"
	"os"
	"strings"

	"spekder/internal/tdf"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: tdfdemo <font> <text...>")
		os.Exit(1)
	}
	f, err := tdf.Embedded(os.Args[1], 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	text := strings.Join(os.Args[2:], " ")
	lines, w, h := f.Render(text, tdf.RenderOpts{})
	fmt.Fprintf(os.Stderr, "font=%q type=%d spacing=%d  block=%dx%d\n", f.Name, f.Type, f.Spacing, w, h)
	for _, ln := range lines {
		fmt.Println(ln + "\x1b[0m")
	}
}
