package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Update detection. Two independent signals feed one shared state: the arena
// server reports its release in MsgStatus (noteArenaVersion, from the menu's
// status poll), and a one-shot GitHub-releases probe runs at startup
// (startUpdateCheck). The menu nudges "update available" when either reports a
// version newer than this build, and the VERSION INFO screen reports the detail.
//
// Dev builds (version == "dev") never nag: an unstamped binary has no real
// version to compare, so updateAvailable stays false there.

var updateMu sync.Mutex
var updateState struct {
	arenaVersion string // arena's release, from MsgStatus ("" = unknown)
	latest       string // newest version seen across all signals
	source       string // where `latest` came from ("the arena" / "GitHub")
	downloadURL  string // where to grab a new client
}

// noteArenaVersion records the arena's reported version (from a status poll) and
// folds it into the newest-seen tally.
func noteArenaVersion(serverVer, recommendedClient string) {
	updateMu.Lock()
	updateState.arenaVersion = serverVer
	updateMu.Unlock()
	// The arena recommends the client build that ships with it.
	if recommendedClient != "" {
		ini := loadINI(defaultINIPath())
		url := ini["download_url"]
		if url == "" {
			url = defaultDownloadURL(ini)
		}
		noteLatest(recommendedClient, "the arena", url)
	}
}

// noteLatest folds a candidate version into the newest-seen state, keeping the
// highest. source/url describe where to get it.
func noteLatest(ver, source, url string) {
	if ver == "" {
		return
	}
	updateMu.Lock()
	defer updateMu.Unlock()
	if updateState.latest == "" || verNewer(ver, updateState.latest) {
		updateState.latest, updateState.source, updateState.downloadURL = ver, source, url
	}
}

// updateAvailable reports whether a build newer than this one is known, and the
// version string. Always false for a dev build (nothing real to compare).
func updateAvailable() (bool, string) {
	if version == "dev" || version == "" {
		return false, ""
	}
	updateMu.Lock()
	defer updateMu.Unlock()
	if updateState.latest != "" && verNewer(updateState.latest, version) {
		return true, updateState.latest
	}
	return false, ""
}

// updateSnapshot returns the current detection state for the VERSION INFO screen.
func updateSnapshot() (arena, latest, source, url string) {
	updateMu.Lock()
	defer updateMu.Unlock()
	return updateState.arenaVersion, updateState.latest, updateState.source, updateState.downloadURL
}

// defaultDownloadURL is the GitHub releases page for the configured repo.
func defaultDownloadURL(ini map[string]string) string {
	repo := ini["update_repo"]
	if repo == "" {
		repo = defaultRepo
	}
	return "https://github.com/" + repo + "/releases/latest"
}

const defaultRepo = "hmderdoc/spekder"

// startUpdateCheck probes GitHub for the latest release once, in the background
// (best-effort; any error is silently ignored - an update nudge is a nicety, not
// a requirement, and the BBS box may have no outbound HTTPS). The endpoint and
// repo are configurable so a fork can point elsewhere; update_url=off disables.
func startUpdateCheck() {
	go func() {
		ini := loadINI(defaultINIPath())
		repo := ini["update_repo"]
		if repo == "" {
			repo = defaultRepo
		}
		api := "https://api.github.com/repos/" + repo + "/releases/latest"
		if u := ini["update_url"]; u != "" {
			if strings.EqualFold(u, "off") || strings.EqualFold(u, "none") {
				return // sysop disabled the GitHub probe
			}
			api = u
		}
		req, err := http.NewRequest("GET", api, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "spekder/"+version)
		client := &http.Client{Timeout: 6 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		var rel struct {
			TagName string `json:"tag_name"`
			HTMLURL string `json:"html_url"`
		}
		if json.NewDecoder(resp.Body).Decode(&rel) != nil || rel.TagName == "" {
			return
		}
		url := rel.HTMLURL
		if url == "" {
			url = defaultDownloadURL(ini)
		}
		noteLatest(rel.TagName, "GitHub", url)
	}()
}

// parseVer splits a "vX.Y.Z" (or "X.Y.Z", with an optional -suffix) into three
// ints; missing/garbage parts read as 0, so "dev" -> {0,0,0}.
func parseVer(s string) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var out [3]int
	for i, part := range strings.SplitN(s, ".", 3) {
		if i >= 3 {
			break
		}
		// stop at the first non-digit (drops a "-rc1" style suffix)
		end := 0
		for end < len(part) && part[end] >= '0' && part[end] <= '9' {
			end++
		}
		out[i], _ = strconv.Atoi(part[:end])
	}
	return out
}

// verNewer reports whether release a is strictly newer than release b.
func verNewer(a, b string) bool {
	x, y := parseVer(a), parseVer(b)
	for i := 0; i < 3; i++ {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}
