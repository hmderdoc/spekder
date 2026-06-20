// Persistent, content-addressed cache for lobby map previews. It lives in a
// single directory on the BBS (data/map-cache next to the binary) shared by every
// door instance, so a map's geometry is downloaded from the arena once and then
// reused across sessions and users. Files are keyed by map name + content hash
// (proto.MapHash): an edited map changes its hash, so the stale entry is simply
// never matched again (and a fresh copy is fetched and written).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	gm "spekder/internal/game"
	"spekder/internal/proto"
)

func mapCacheDir() string {
	dir := filepath.Join("data", "map-cache")
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Join(filepath.Dir(exe), "data", "map-cache")
	}
	return dir
}

// mapCachePath is the on-disk file for a (name, hash) pair. The hash makes it a
// version: change the map, change the file.
func mapCachePath(name string, hash uint32) string {
	return filepath.Join(mapCacheDir(), fmt.Sprintf("%s-%08x.spkmap", sanitizeKey(name), hash))
}

// mapCacheLoad returns the cached map for (name, hash) if a valid file exists.
// A missing or corrupt file reads as a miss, so the caller refetches.
func mapCacheLoad(name string, hash uint32) (gm.Map, bool) {
	data, err := os.ReadFile(mapCachePath(name, hash))
	if err != nil {
		return gm.Map{}, false
	}
	m, ok := proto.DecodeMap(data) // stored as the MsgMap wire form
	if !ok {
		return gm.Map{}, false
	}
	return m, true
}

// mapCacheStore persists a map under (name, hash). The write is atomic (temp file
// + rename) so a second door instance reading concurrently never sees a partial
// file. Best-effort: any error just leaves the entry uncached.
func mapCacheStore(name string, hash uint32, m gm.Map) {
	dir := mapCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	final := mapCachePath(name, hash)
	tmp := fmt.Sprintf("%s.tmp%d", final, os.Getpid())
	if err := os.WriteFile(tmp, proto.EncodeMap(m), 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
	}
}
