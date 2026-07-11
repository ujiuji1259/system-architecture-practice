// Package tweetcache caches tweet bodies for the read path. The home timeline
// holds only ids; on read the service hydrates bodies through Cache first and
// the repository falls back to SQLite for misses (then warms the cache).
//
// This package owns the Cache interface and both backends (in-process and
// Redis). The repository accepts a Cache through a structural interface, so it
// does not import tweetcache and stays independent of go-redis.
package tweetcache

import (
	"context"
	"sync"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
)

// Cache is the tweet-body cache the read path hits before falling back to the
// source of truth. Tweets are immutable, so entries never need invalidation.
type Cache interface {
	// GetMany returns the cached tweets among ids (misses are simply absent).
	GetMany(ctx context.Context, ids []int64) (map[int64]repository.Tweet, error)
	// SetMany warms the cache with authoritative tweets.
	SetMany(ctx context.Context, tweets map[int64]repository.Tweet) error
}

// Memory is an in-process Cache used when no Redis is configured and in tests.
type Memory struct {
	mu sync.RWMutex
	m  map[int64]repository.Tweet
}

var _ Cache = (*Memory)(nil)

// NewMemory returns an empty in-process Cache.
func NewMemory() *Memory {
	return &Memory{m: make(map[int64]repository.Tweet)}
}

// GetMany returns the cached subset of ids.
func (c *Memory) GetMany(_ context.Context, ids []int64) (map[int64]repository.Tweet, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[int64]repository.Tweet, len(ids))
	for _, id := range ids {
		if t, ok := c.m[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

// SetMany stores the given tweets.
func (c *Memory) SetMany(_ context.Context, tweets map[int64]repository.Tweet) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, t := range tweets {
		c.m[id] = t
	}
	return nil
}
