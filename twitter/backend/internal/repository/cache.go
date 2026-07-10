package repository

import (
	"context"
	"sync"
)

// TweetCache caches tweet bodies for timeline hydration. The home timeline
// holds only ids; on read we fetch bodies here first and fall back to SQLite
// for misses (then warm the cache). This is the read-side analogue of the
// url-shortener count cache: SQLite stays the source of truth.
type TweetCache interface {
	// GetMany returns the cached tweets among ids (misses are simply absent).
	GetMany(ctx context.Context, ids []int64) (map[int64]Tweet, error)
	// SetMany warms the cache with authoritative tweets.
	SetMany(ctx context.Context, tweets map[int64]Tweet) error
}

// MemoryCache is an in-process TweetCache used when no Redis is configured and
// in tests. Tweets are immutable, so entries never need invalidation.
type MemoryCache struct {
	mu sync.RWMutex
	m  map[int64]Tweet
}

// NewMemoryCache returns an empty in-process TweetCache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{m: make(map[int64]Tweet)}
}

// GetMany returns the cached subset of ids.
func (c *MemoryCache) GetMany(_ context.Context, ids []int64) (map[int64]Tweet, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[int64]Tweet, len(ids))
	for _, id := range ids {
		if t, ok := c.m[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

// SetMany stores the given tweets.
func (c *MemoryCache) SetMany(_ context.Context, tweets map[int64]Tweet) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, t := range tweets {
		c.m[id] = t
	}
	return nil
}
