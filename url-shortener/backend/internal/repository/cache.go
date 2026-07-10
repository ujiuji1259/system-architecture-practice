package repository

import (
	"context"
	"sync"
)

// CountCache is a fast, best-effort view of visit counts used internally by the
// repository. It is a disposable materialized view of the event log: on a miss
// the value is rebuilt by counting events, so it may be absent or stale but is
// never authoritative.
type CountCache interface {
	// Incr increments the cached count for code only if it is already present.
	// If absent it is a no-op, so the next read rebuilds it from the event log
	// (which already includes the just-recorded event).
	Incr(ctx context.Context, code string) error
	// Get returns the cached count and whether it was present.
	Get(ctx context.Context, code string) (int64, bool, error)
	// Set warms the cache with an authoritative count.
	Set(ctx context.Context, code string, count int64) error
	// Del removes a code from the cache.
	Del(ctx context.Context, code string) error
}

// MemoryCache is an in-process CountCache. It is suitable for a single instance
// (counts are not shared across processes) and is the default when no external
// cache is configured.
type MemoryCache struct {
	mu sync.Mutex
	m  map[string]int64
}

// NewMemoryCache returns an empty MemoryCache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{m: make(map[string]int64)}
}

// Incr increments code's count only if it is already cached.
func (c *MemoryCache) Incr(_ context.Context, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[code]; ok {
		c.m[code]++
	}
	return nil
}

// Get returns the cached count and whether it was present.
func (c *MemoryCache) Get(_ context.Context, code string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.m[code]
	return n, ok, nil
}

// Set stores an authoritative count for code.
func (c *MemoryCache) Set(_ context.Context, code string, count int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[code] = count
	return nil
}

// Del removes code from the cache.
func (c *MemoryCache) Del(_ context.Context, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, code)
	return nil
}
