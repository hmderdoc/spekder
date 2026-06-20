package main

import (
	"testing"

	gm "spekder/internal/game"
)

func bodyExtents(body int) (wx, wy, wz float64) {
	id := func(l V3) V3 { return l }
	w := [3]float64{1, 1, 1}
	tris := buildBody(nil, body, id, w, w, 1, 0)
	if len(tris) == 0 {
		return
	}
	lo, hi := tris[0].v[0], tris[0].v[0]
	for _, tr := range tris {
		for _, v := range tr.v {
			lo = V3{min(lo.X, v.X), min(lo.Y, v.Y), min(lo.Z, v.Z)}
			hi = V3{max(hi.X, v.X), max(hi.Y, v.Y), max(hi.Z, v.Z)}
		}
	}
	return hi.X - lo.X, hi.Y - lo.Y, hi.Z - lo.Z
}

func TestButterflyWingsFaceViewer(t *testing.T) {
	wx, wy, wz := bodyExtents(gm.BodyButterfly)
	t.Logf("butterfly extents: X(width)=%.2f Y(height)=%.2f Z(depth)=%.2f", wx, wy, wz)
	if wx < wz*1.8 {
		t.Errorf("wings not spread sideways (X=%.2f must dominate Z=%.2f) - wrong plane", wx, wz)
	}
	if wx < 1.0 {
		t.Errorf("wings too narrow: X=%.2f", wx)
	}
}
