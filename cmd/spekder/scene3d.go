package main

// Nintendo 3DS text-depth support.
//
// 3dBBS identifies itself with a private APC round trip, then accepts a layer
// tag on every subsequently-written terminal cell.  Layers are depths behind
// the display glass (centi-world-units on the wire).  Ordinary terminals ignore
// the one detection APC and never receive any of the private CSI layer controls.

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	scene3DQuery         = "\x1b_3DS:Query\x1b\\"
	scene3DMaxLayers     = 16
	scene3DLayerAlert    = 0 // urgent prompts and modal UI: at the glass
	scene3DLayerUI       = 1 // ordinary HUD and menus: just behind the glass
	scene3DLayerWorld    = 2 // rasterized geometry
	scene3DLayerBackdrop = 3 // sky, empty preview space, music visualizer
)

// A restrained spread reads clearly on the small upper display without making
// text uncomfortable: interface elements stay close, while the arena appears
// through a window and its sky sits one step farther away.
var scene3DDepth = [scene3DMaxLayers]int{
	0,   // alert
	55,  // UI (0.55 world units)
	310, // world geometry (3.10)
	470, // backdrop (4.70)
}

type scene3DVersion struct{ major, minor int }

var scene3DActive bool

func parseScene3DVersion(payload []byte) (scene3DVersion, bool) {
	parts := strings.Split(string(payload), ";")
	if len(parts) != 3 || parts[0] != "3DS:Ver" {
		return scene3DVersion{}, false
	}
	major, err1 := strconv.Atoi(parts[1])
	minor, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return scene3DVersion{}, false
	}
	return scene3DVersion{major: major, minor: minor}, true
}

func (v scene3DVersion) supportsTextLayers() bool {
	return v.major > 0 || v.minor >= 3
}

func scene3DSelectSeq(layer int) string {
	if !scene3DActive {
		return ""
	}
	return scene3DSelectSeqRaw(layer)
}

func scene3DSelectSeqRaw(layer int) string {
	if layer < 0 {
		layer = 0
	} else if layer >= scene3DMaxLayers {
		layer = scene3DMaxLayers - 1
	}
	return fmt.Sprintf("\x1b[=%dz", layer)
}

func scene3DDefineSeqRaw(layer, depthCenti int) string {
	if layer < 0 {
		layer = 0
	} else if layer >= scene3DMaxLayers {
		layer = scene3DMaxLayers - 1
	}
	if depthCenti < 0 {
		depthCenti = 0
	} else if depthCenti > 1800 {
		depthCenti = 1800
	}
	return fmt.Sprintf("\x1b[=%d;%d*z", layer, depthCenti)
}

func scene3DInstallSeq() string {
	var b strings.Builder
	for layer := 0; layer < scene3DMaxLayers; layer++ {
		b.WriteString(scene3DDefineSeqRaw(layer, scene3DDepth[layer]))
	}
	b.WriteString(scene3DSelectSeqRaw(scene3DLayerUI))
	return b.String()
}

func scene3DResetSeq() string {
	if !scene3DActive {
		return ""
	}
	var b strings.Builder
	b.WriteString(scene3DSelectSeqRaw(scene3DLayerAlert))
	for layer := 0; layer < scene3DMaxLayers; layer++ {
		b.WriteString(scene3DDefineSeqRaw(layer, 0))
	}
	return b.String()
}

// detectScene3D runs after the input goroutine starts.  That goroutine consumes
// the private APC reply while continuing to route real keystrokes normally, so
// detection cannot steal a caller's first key (unlike a synchronous raw read).
func detectScene3D(t Term, replies <-chan scene3DVersion, timeout time.Duration) (scene3DVersion, bool) {
	scene3DActive = false
	if _, err := io.WriteString(t, scene3DQuery); err != nil {
		return scene3DVersion{}, false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case v := <-replies:
		if !v.supportsTextLayers() {
			return v, false
		}
		// Activate before publishing the table so all later helpers begin emitting
		// controls atomically with a successful installation.
		scene3DActive = true
		if _, err := io.WriteString(t, scene3DInstallSeq()); err != nil {
			scene3DActive = false
			return v, false
		}
		return v, true
	case <-timer.C:
		return scene3DVersion{}, false
	}
}
