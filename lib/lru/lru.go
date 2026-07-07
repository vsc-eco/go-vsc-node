// Package lru provides a simple generic thread-safe LRU cache.
package lru

import (
	"container/list"
	"sync"
)

// Cache is a generic LRU cache with capacity limit.
// Thread-safe for concurrent use.
// K must be comparable, V can be any type.
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	items    map[K]*list.Element
	order    *list.List
}

type entry[K comparable, V any] struct {
	key   K
	value V
}

// New creates an LRU cache with the given capacity.
// Panics if capacity <= 0.
func New[K comparable, V any](capacity int) *Cache[K, V] {
	if capacity <= 0 {
		panic("lru: capacity must be > 0")
	}
	return &Cache[K, V]{
		capacity: capacity,
		items:    make(map[K]*list.Element),
		order:    list.New(),
	}
}

// Get returns the value for the key if present, and moves it to front.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		return elem.Value.(*entry[K, V]).value, true
	}
	var zero V
	return zero, false
}

// Put adds or updates a key-value pair, evicting the least recently used
// entry if the cache is at capacity.
func (c *Cache[K, V]) Put(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		elem.Value.(*entry[K, V]).value = value
		return
	}
	if c.order.Len() >= c.capacity {
		c.evictOldest()
	}
	elem := c.order.PushFront(&entry[K, V]{key: key, value: value})
	c.items[key] = elem
}

// Len returns the current number of items in the cache.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *Cache[K, V]) evictOldest() {
	// caller must hold c.mu
	elem := c.order.Back()
	if elem == nil {
		return
	}
	c.order.Remove(elem)
	kv := elem.Value.(*entry[K, V])
	delete(c.items, kv.key)
}
