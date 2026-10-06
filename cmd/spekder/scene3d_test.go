package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

type scene3DTestTerm struct{ bytes.Buffer }

func (t *scene3DTestTerm) Close() error { return nil }
func (t *scene3DTestTerm) Read([]byte) (int, error) {
	return 0, io.EOF
}
func (t *scene3DTestTerm) ReadTimeout([]byte, time.Duration) (int, error) {
	return 0, nil
}

func TestParseScene3DVersion(t *testing.T) {
	v, ok := parseScene3DVersion([]byte("3DS:Ver;0;3"))
	if !ok || v.major != 0 || v.minor != 3 || !v.supportsTextLayers() {
		t.Fatalf("parsed version = %#v, %v", v, ok)
	}
	for _, bad := range []string{"", "3DS:Ver;0", "3DS:Ver;x;3", "OTHER;0;3"} {
		if _, ok := parseScene3DVersion([]byte(bad)); ok {
			t.Errorf("accepted invalid reply %q", bad)
		}
	}
	if (scene3DVersion{minor: 2}).supportsTextLayers() {
		t.Error("protocol 0.2 unexpectedly reports text-layer support")
	}
}

func TestReadScene3DAPC(t *testing.T) {
	in := &input{}
	buf := []byte("\x1b_3DS:Ver;0;3\x1b\\")
	i := 1 // reader has already consumed ESC _
	payload, ok := in.readAPC(&scene3DTestTerm{}, buf, len(buf), &i)
	if !ok || string(payload) != "3DS:Ver;0;3" {
		t.Fatalf("APC payload = %q, %v", payload, ok)
	}
}

func TestDetectScene3DInstallsLayerTable(t *testing.T) {
	old := scene3DActive
	defer func() { scene3DActive = old }()
	scene3DActive = false
	term := &scene3DTestTerm{}
	replies := make(chan scene3DVersion, 1)
	replies <- scene3DVersion{minor: 3}
	v, ok := detectScene3D(term, replies, time.Second)
	if !ok || v.minor != 3 || !scene3DActive {
		t.Fatalf("detection = %#v, %v; active=%v", v, ok, scene3DActive)
	}
	out := term.String()
	for _, want := range []string{
		scene3DQuery,
		"\x1b[=0;0*z",
		"\x1b[=1;55*z",
		"\x1b[=2;310*z",
		"\x1b[=3;470*z",
		"\x1b[=15;0*z",
		scene3DSelectSeqRaw(scene3DLayerUI),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("installed output missing %q", want)
		}
	}
}

func TestOlderScene3DProtocolStaysTwoDimensional(t *testing.T) {
	old := scene3DActive
	defer func() { scene3DActive = old }()
	scene3DActive = false
	term := &scene3DTestTerm{}
	replies := make(chan scene3DVersion, 1)
	replies <- scene3DVersion{minor: 2}
	v, ok := detectScene3D(term, replies, time.Second)
	if ok || v.minor != 2 || scene3DActive {
		t.Fatalf("old protocol detection = %#v, %v; active=%v", v, ok, scene3DActive)
	}
	if got := term.String(); got != scene3DQuery {
		t.Fatalf("old protocol received layer controls: %q", got)
	}
}

func TestEncodeTracksSemanticDepth(t *testing.T) {
	old3D, oldColor := scene3DActive, colorMode
	defer func() { scene3DActive, colorMode = old3D, oldColor }()
	scene3DActive, colorMode = true, colorTrue

	r := newRenderer(3, 2)
	r.clearBlack()
	// A world cell, then a HUD cell; the third is the protected bottom-right.
	r.paintLayer = scene3DLayerWorld
	r.putPx(0, 0, [3]byte{80, 100, 120})
	r.putPx(0, 1, [3]byte{30, 40, 50})
	r.paintLayer = scene3DLayerUI
	r.putPx(1, 0, [3]byte{200, 220, 240})
	r.putPx(1, 1, [3]byte{100, 110, 120})

	frame, prev := encode(r, 3, 1, nil)
	out := string(frame)
	for _, layer := range []int{scene3DLayerBackdrop, scene3DLayerWorld, scene3DLayerUI} {
		if !strings.Contains(out, scene3DSelectSeqRaw(layer)) {
			t.Errorf("encoded frame missing layer %d selector: %q", layer, out)
		}
	}
	if !strings.HasSuffix(out, scene3DSelectSeqRaw(scene3DLayerUI)) {
		t.Errorf("frame did not restore UI layer: %q", out)
	}
	if len(prev) != 3*7 {
		t.Fatalf("depth-aware previous frame has %d bytes, want 21", len(prev))
	}

	// A layer-only change must dirty the cell even though its RGB is unchanged.
	r.layer[0], r.layer[3] = scene3DLayerBackdrop, scene3DLayerBackdrop
	delta, _ := encode(r, 3, 1, prev)
	if !strings.Contains(string(delta), scene3DSelectSeqRaw(scene3DLayerBackdrop)) {
		t.Errorf("layer-only change was not emitted: %q", delta)
	}
}

func TestEncodeOmitsLayerControlsOnOrdinaryTerminal(t *testing.T) {
	old3D, oldColor := scene3DActive, colorMode
	defer func() { scene3DActive, colorMode = old3D, oldColor }()
	scene3DActive, colorMode = false, colorTrue
	r := newRenderer(2, 2)
	r.clearBlack()
	frame, _ := encode(r, 2, 1, nil)
	if bytes.Contains(frame, []byte("\x1b[=")) {
		t.Fatalf("2-D terminal received private layer CSI: %q", frame)
	}
}
