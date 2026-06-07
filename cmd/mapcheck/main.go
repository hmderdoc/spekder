// Command mapcheck validates Spekder map JSON files against the schema and
// reports issues. Authors run it before shipping a map; the editor will reuse
// the same game.ValidateMap so on-disk and in-editor checks always agree.
//
// Usage:
//
//	mapcheck map1.json [map2.json ...]
//
// Exits non-zero if any file fails to parse or has a fatal validation error.
package main

import (
	"fmt"
	"os"

	gm "spekder/internal/game"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mapcheck <map.json> [more.json ...]")
		os.Exit(2)
	}
	bad := false
	for _, path := range os.Args[1:] {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("%s: cannot read: %v\n", path, err)
			bad = true
			continue
		}
		m, err := gm.ParseMapJSON(data)
		if err != nil {
			fmt.Printf("%s: invalid JSON: %v\n", path, err)
			bad = true
			continue
		}
		issues := gm.ValidateMap(m)
		name := m.Name
		if name == "" {
			name = "(unnamed)"
		}
		if len(issues) == 0 {
			fmt.Printf("%s: OK  %q  (%d obstacles, %d ramps, %d entities)\n",
				path, name, len(m.Obstacles), len(m.Ramps), len(m.Entities))
			continue
		}
		fmt.Printf("%s: %q\n", path, name)
		for _, is := range issues {
			fmt.Printf("  %s\n", is)
		}
		if gm.FatalIssues(issues) {
			bad = true
		}
	}
	if bad {
		os.Exit(1)
	}
}
