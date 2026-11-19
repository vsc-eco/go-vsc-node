package main

import (
	"errors"
	"testing"
)

// I-42: a block waiting for depth is downloaded once, not on every pass; a new
// hash (the next block, or a reorg at the same height) is downloaded; a failed
// download is retried, not cached.
func TestI42_AWaitingBlockIsDownloadedOnce(t *testing.T) {
	calls := map[string]int{}
	fail := false
	fetch := func(h string) ([]byte, error) {
		calls[h]++
		if fail {
			return nil, errors.New("429")
		}
		return []byte("block " + h), nil
	}
	var c rawBlockCache
	for i := 0; i < 30; i++ { // 30 passes while block A is not yet deep enough
		b, err := c.get("A", fetch)
		if err != nil || string(b) != "block A" {
			t.Fatalf("pass %d: %q %v", i, b, err)
		}
	}
	if calls["A"] != 1 {
		t.Fatalf("block A downloaded %d times while it waited, want 1", calls["A"])
	}
	if b, _ := c.get("A2", fetch); string(b) != "block A2" || calls["A2"] != 1 {
		t.Fatalf("a reorged block at the same height (new hash) must be downloaded: %q, %d", b, calls["A2"])
	}
	fail = true
	if _, err := c.get("B", fetch); err == nil {
		t.Fatal("a failed download must return its error")
	}
	fail = false
	if b, err := c.get("B", fetch); err != nil || string(b) != "block B" || calls["B"] != 2 {
		t.Fatalf("a failed download must not be cached: %q %v, %d calls", b, err, calls["B"])
	}
	if b, _ := c.get("A2", fetch); string(b) != "block A2" || calls["A2"] != 2 {
		t.Fatalf("only the last block is kept: %q, %d", b, calls["A2"])
	}
	// GetRawBlock ends in io.ReadAll, which returns what it read with the
	// error when the body breaks off: a partial block must never be kept.
	partial := func(h string) ([]byte, error) { calls[h]++; return []byte("blo"), errors.New("unexpected EOF") }
	if _, err := c.get("C", partial); err == nil {
		t.Fatal("a broken body must return its error")
	}
	if b, err := c.get("C", fetch); err != nil || string(b) != "block C" {
		t.Fatalf("a partial body was cached: %q %v", b, err)
	}
}
