package lru

import (
	"sync"
	"testing"
)

func TestBasic(t *testing.T) {
	c := New[string, int](3)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)

	v, ok := c.Get("a")
	if !ok || v != 1 {
		t.Fatalf("expected a=1, got %v, %v", v, ok)
	}
}

func TestEviction(t *testing.T) {
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3) // evicts a

	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	v, ok := c.Get("b")
	if !ok || v != 2 {
		t.Fatalf("expected b=2, got %v, %v", v, ok)
	}
	v, ok = c.Get("c")
	if !ok || v != 3 {
		t.Fatalf("expected c=3, got %v, %v", v, ok)
	}
}

func TestUpdate(t *testing.T) {
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("a", 100)

	v, ok := c.Get("a")
	if !ok || v != 100 {
		t.Fatalf("expected updated a=100, got %v, %v", v, ok)
	}
}

func TestLRUOrder(t *testing.T) {
	c := New[string, int](3)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	c.Get("a")    // a moves to front
	c.Get("a")    // a stays front
	c.Put("d", 4) // should evict b (least recently used)

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should still be present")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should still be present")
	}
	if _, ok := c.Get("d"); !ok {
		t.Fatal("d should be present")
	}
}

func TestConcurrent(t *testing.T) {
	c := New[int, int](100)
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(base int) {
			for j := 0; j < 100; j++ {
				k := base*100 + j
				c.Put(k, k)
				c.Get(k)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestGetMissing verifies that Get on an absent key returns the zero value
// and false, and does not create a phantom entry.
func TestGetMissing(t *testing.T) {
	c := New[string, int](2)
	v, ok := c.Get("nope")
	if ok {
		t.Fatalf("expected miss, got ok=true")
	}
	if v != 0 {
		t.Fatalf("expected zero value on miss, got %v", v)
	}
	if c.Len() != 0 {
		t.Fatalf("miss must not add an entry, Len=%d", c.Len())
	}
}

// TestNewPanicsOnNonPositiveCapacity verifies New panics for capacity <= 0.
func TestNewPanicsOnNonPositiveCapacity(t *testing.T) {
	for _, cap := range []int{0, -1, -100} {
		func(cap int) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("New(%d) should panic", cap)
				}
			}()
			_ = New[string, int](cap)
		}(cap)
	}
}

// TestCapacityOne exercises the smallest valid cache: every new key evicts
// the previous one.
func TestCapacityOne(t *testing.T) {
	c := New[string, int](1)
	c.Put("a", 1)
	if c.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", c.Len())
	}
	c.Put("b", 2)
	if c.Len() != 1 {
		t.Fatalf("expected Len=1 after eviction, got %d", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted by b")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("expected b=2, got %v, %v", v, ok)
	}
}

// TestPutPromotesRecency verifies that updating an existing key via Put marks
// it most-recently-used, protecting it from the next eviction. This is a
// distinct code path from Get-based promotion.
func TestPutPromotesRecency(t *testing.T) {
	c := New[string, int](3)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	// Touch "a" via Put (not Get). Order (LRU->MRU) becomes: b, c, a.
	c.Put("a", 10)
	// Inserting "d" must evict the least recently used, which is now "b".
	c.Put("d", 4)

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted after a was promoted via Put")
	}
	if v, ok := c.Get("a"); !ok || v != 10 {
		t.Fatalf("a should survive with updated value 10, got %v, %v", v, ok)
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should still be present")
	}
	if _, ok := c.Get("d"); !ok {
		t.Fatal("d should be present")
	}
}

// TestLenTracksCapacity verifies Len grows up to capacity then stays capped
// as evictions occur.
func TestLenTracksCapacity(t *testing.T) {
	const cap = 5
	c := New[int, int](cap)
	for i := 0; i < 100; i++ {
		c.Put(i, i)
		want := i + 1
		if want > cap {
			want = cap
		}
		if got := c.Len(); got != want {
			t.Fatalf("after inserting %d keys: Len=%d, want %d", i+1, got, want)
		}
	}
}

// TestUpdateDoesNotEvict verifies that repeatedly updating existing keys never
// evicts any entry, since the item count does not change.
func TestUpdateDoesNotEvict(t *testing.T) {
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	for i := 0; i < 10; i++ {
		c.Put("a", i)
		c.Put("b", i)
	}
	if c.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", c.Len())
	}
	if v, ok := c.Get("a"); !ok || v != 9 {
		t.Fatalf("expected a=9, got %v, %v", v, ok)
	}
	if v, ok := c.Get("b"); !ok || v != 9 {
		t.Fatalf("expected b=9, got %v, %v", v, ok)
	}
}

// TestConcurrentBounded stresses the cache from many goroutines mixing Put,
// Get and Len, and asserts the size invariant (Len <= capacity) is never
// violated. Run with -race to also validate the locking.
func TestConcurrentBounded(t *testing.T) {
	const capacity = 50
	c := New[int, int](capacity)
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				k := (base*500 + j) % 200 // reuse keys to force updates + evictions
				c.Put(k, k)
				c.Get(k)
				if n := c.Len(); n > capacity {
					t.Errorf("Len=%d exceeded capacity=%d", n, capacity)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if n := c.Len(); n > capacity {
		t.Fatalf("final Len=%d exceeded capacity=%d", n, capacity)
	}
}
