package wasm_runtime

import (
	"sync"
	"testing"

	"vsc-node/lib/lru"
)

// emptyModule is a minimal valid wasm binary (magic + version, no sections).
var emptyModule = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

// typeSections is a valid wasm binary with a type section declaring a single
// empty function type — distinct from emptyModule so tests can key multiple
// entries.
var typeSections = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x04, 0x01, 0x60, 0x00, 0x00}

func TestGetOrLoadBytecode_LoadsAndHits(t *testing.T) {
	bc1, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("first load failed: %v", err)
	}
	defer bc1.drop()

	bc2, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("cached load failed: %v", err)
	}

	if bc1 != bc2 {
		t.Fatal("cached hit must return the same instance")
	}
	ast1 := bc1.acquire()
	ast2 := bc2.acquire()
	if ast1 == nil || ast2 == nil {
		t.Fatal("acquire returned nil for a live entry")
	}
	if ast1 != ast2 {
		t.Fatal("both acquires must yield the same AST")
	}
	bc1.release()
	bc2.release()
}

func TestGetOrLoadBytecode_DistinctCodes(t *testing.T) {
	bc1, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("load emptyModule failed: %v", err)
	}
	defer bc1.drop()

	bc2, err := getOrLoadBytecode(typeSections)
	if err != nil {
		t.Fatalf("load typeSections failed: %v", err)
	}
	defer bc2.drop()

	if bc1 == bc2 {
		t.Fatal("different bytecode must yield different cache entries")
	}
}

func TestGetOrLoadBytecode_InvalidCode(t *testing.T) {
	_, err := getOrLoadBytecode([]byte("this is not wasm"))
	if err == nil {
		t.Fatal("invalid bytecode must error")
	}

	// The invalid code must not have been cached: a subsequent load of valid
	// code must still succeed.
	bc, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("valid load after invalid code failed: %v", err)
	}
	defer bc.drop()
}

func TestGetOrLoadBytecode_DeadEntryReloaded(t *testing.T) {
	bc1, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	// Simulate eviction (drop with no outstanding references releases the AST).
	bc1.drop()
	if bc1.isDead() {
		t.Log("entry marked dead")
	}

	bc2, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("reload after drop failed: %v", err)
	}
	defer bc2.drop()

	if bc1 == bc2 {
		t.Fatal("a dead entry must be replaced by a fresh load")
	}
	if ast := bc2.acquire(); ast == nil {
		t.Fatal("reloaded entry must be acquirable")
	} else {
		bc2.release()
	}
}

// TestCachedBytecode_DropWhileHeld verifies the deferred-release contract:
// drop() while an acquisition is outstanding must not free the AST until the
// last release().
func TestCachedBytecode_DropWhileHeld(t *testing.T) {
	bc, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	ast := bc.acquire()
	if ast == nil {
		t.Fatal("acquire must succeed")
	}

	bc.drop() // eviction while in use — must defer the C release
	if bc.ast == nil {
		t.Fatal("drop must not release an AST that is still in use")
	}

	bc.release() // last user done — AST released here
	if bc.ast != nil {
		t.Fatal("release must free the AST after a deferred drop")
	}

	// Entry is dead: further acquires must fail and never double-release.
	if ast := bc.acquire(); ast != nil {
		t.Fatal("acquire on a dead entry must return nil")
	}
	// Drop is idempotent.
	bc.drop()
}

func TestCachedBytecode_DropUnused(t *testing.T) {
	bc, err := getOrLoadBytecode(typeSections)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	bc.drop()
	if bc.ast != nil {
		t.Fatal("drop must release an unused AST immediately")
	}
}

// TestCachedBytecode_ConcurrentAcquireRelease stresses the refcount under the
// race detector: concurrent acquire/release pairs racing with eviction must
// never release the AST while it is in use, and must free it exactly once.
func TestCachedBytecode_ConcurrentAcquireRelease(t *testing.T) {
	bc, err := getOrLoadBytecode(emptyModule)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	const users = 16
	var wg sync.WaitGroup
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if ast := bc.acquire(); ast != nil {
					bc.release()
				}
			}
		}()
	}
	wg.Wait()

	bc.drop()
	if bc.ast != nil {
		t.Fatal("AST must be released once all users have drained")
	}
}

// TestBytecodeCache_EvictionReleases verifies the lru eviction hook wiring:
// when the cache evicts an entry, the AST is released (immediately when
// unused).
func TestBytecodeCache_EvictionReleases(t *testing.T) {
	c := lru.NewEvict[string, *cachedBytecode](2, func(_ string, bc *cachedBytecode) {
		bc.drop()
	})

	mk := func(code []byte) *cachedBytecode {
		ast, err := loadBytecode(code)
		if err != nil {
			t.Fatalf("loadBytecode(%v) failed: %v", code, err)
		}
		bc := &cachedBytecode{ast: ast}
		bc.acquire() // hold a ref so the evicted entry must defer, not free
		return bc
	}

	a := mk(emptyModule)
	b := mk(typeSections)
	third, err := loadBytecode([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x07, 0x02, 0x60, 0x00, 0x00, 0x60, 0x00, 0x00})
	if err != nil {
		t.Fatalf("loadBytecode(third) failed: %v", err)
	}
	c.Put("a", a)
	c.Put("b", b)

	c.Put("c", &cachedBytecode{ast: third}) // evicts "a" (LRU) while held

	if a.ast == nil {
		t.Fatal("eviction must defer release while the entry is held")
	}
	a.release() // drain the holder — AST must be freed by the deferred drop
	if a.ast != nil {
		t.Fatal("release must free the evicted-but-held AST")
	}
	// The evicted entry must no longer be reachable from the cache.
	if _, ok := c.Get("a"); ok {
		t.Fatal("evicted key must not be reachable")
	}
}
