package main

import "testing"

func TestVerNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.4.0", "v0.3.0", true},
		{"0.3.1", "0.3.0", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.3.0", "v0.4.0", false},
		{"v0.3.0-rc1", "v0.3.0", false}, // suffix ignored -> equal -> not newer
		{"dev", "v0.3.0", false},        // unstamped parses as 0.0.0
		{"v0.3.0", "dev", true},
	}
	for _, c := range cases {
		if got := verNewer(c.a, c.b); got != c.want {
			t.Errorf("verNewer(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// A dev build must never nag about updates, even if a newer version is recorded.
func TestUpdateAvailableDevSilent(t *testing.T) {
	savedVer := version
	t.Cleanup(func() { version = savedVer })
	updateMu.Lock()
	savedLatest := updateState.latest
	updateState.latest = "v9.9.9"
	updateMu.Unlock()
	t.Cleanup(func() {
		updateMu.Lock()
		updateState.latest = savedLatest
		updateMu.Unlock()
	})

	version = "dev"
	if avail, _ := updateAvailable(); avail {
		t.Fatal("dev build should not report an update")
	}
	version = "v0.3.0"
	if avail, latest := updateAvailable(); !avail || latest != "v9.9.9" {
		t.Fatalf("stamped build should see the update: avail=%v latest=%q", avail, latest)
	}
}
