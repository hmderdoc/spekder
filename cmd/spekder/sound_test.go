package main

import (
	"bytes"
	"sync"
	"testing"
)

// TestLockedWriterSerializes: concurrent writers through a lockedWriter never tear
// each other's writes (each Write lands whole). Run with -race to also confirm
// there's no data race on the shared underlying writer - this is the guarantee the
// background music goroutine relies on to share the terminal with the UI flushes.
func TestLockedWriterSerializes(t *testing.T) {
	var buf bytes.Buffer
	lw := &lockedWriter{w: &buf}
	const a, b = "AAAAAAAA", "BBBBBBBB" // distinct 8-byte blocks
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); lw.Write([]byte(a)) }()
		go func() { defer wg.Done(); lw.Write([]byte(b)) }()
	}
	wg.Wait()
	s := buf.String()
	if len(s) != 200*2*8 {
		t.Fatalf("short write: got %d bytes", len(s))
	}
	for i := 0; i+8 <= len(s); i += 8 {
		if blk := s[i : i+8]; blk != a && blk != b {
			t.Fatalf("torn write at offset %d: %q", i, blk)
		}
	}
}
