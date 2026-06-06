// fontscan catalogs a directory of .TDF fonts by how wide they render a sample
// string, so we can pick fonts that fit a given screen width.
//
//	fontscan <dir> [sample] [maxWidth]
//
// Prints: width height type name file[index]  (sorted by width, fitting first)
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"spekder/internal/tdf"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fontscan <dir> [sample] [maxWidth]")
		os.Exit(1)
	}
	dir := os.Args[1]
	sample := "SPEKDER"
	if len(os.Args) > 2 {
		sample = os.Args[2]
	}
	maxW := 0
	if len(os.Args) > 3 {
		maxW, _ = strconv.Atoi(os.Args[3])
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.tdf"))
	type row struct {
		w, h, typ, idx int
		name, file     string
	}
	var rows []row
	typeName := []string{"outline", "block", "color"}
	for _, fn := range files {
		data, err := os.ReadFile(fn)
		if err != nil {
			continue
		}
		for i, f := range tdf.ParseAll(data) {
			w, h := f.Measure(sample)
			if w == 0 {
				continue // font can't render the sample
			}
			tn := "?"
			if f.Type >= 0 && f.Type < len(typeName) {
				tn = typeName[f.Type][:1]
			}
			_ = tn
			rows = append(rows, row{w: w, h: h, typ: f.Type, idx: i, name: f.Name, file: filepath.Base(fn)})
		}
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].w < rows[b].w })

	fit := 0
	for _, r := range rows {
		if maxW > 0 && r.w > maxW {
			continue
		}
		t := "?"
		if r.typ >= 0 && r.typ < len(typeName) {
			t = typeName[r.typ]
		}
		fmt.Printf("%3dw %2dh  %-7s  %-14s  %s[%d]\n", r.w, r.h, t, strings.TrimSpace(r.name), r.file, r.idx)
		fit++
	}
	fmt.Fprintf(os.Stderr, "\n%d fonts (%d shown) over sample %q\n", len(rows), fit, sample)
}
