package cache

import (
	"container/list"
	"sync"
	"time"
)

type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time
}

type LRU[K comparable, V any] struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	order *list.List
	items map[K]*list.Element
	now   func() time.Time
}

func New[K comparable, V any](capacity int, ttl time.Duration) *LRU[K, V] {
	return &LRU[K, V]{cap: max(1, capacity), ttl: ttl, order: list.New(), items: map[K]*list.Element{}, now: time.Now}
}

func (c *LRU[K, V]) WithClock(now func() time.Time) *LRU[K, V] {
	c.now = now
	return c
}

func (c *LRU[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.ttl > 0 && c.now().After(e.expires) {
		c.order.Remove(el)
		delete(c.items, key)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

func (c *LRU[K, V]) Set(key K, value V) {
	c.SetFor(key, value, c.ttl)
}

func (c *LRU[K, V]) SetFor(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := c.now().Add(ttl)
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		e.value, e.expires = value, expires
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&entry[K, V]{key: key, value: value, expires: expires})
	for c.order.Len() > c.cap {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry[K, V]).key)
	}
}

func (c *LRU[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}

func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
