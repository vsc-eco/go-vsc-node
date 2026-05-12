package lru

import "testing"

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
	c.Get("a") // a moves to front
	c.Get("a") // a stays front
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
