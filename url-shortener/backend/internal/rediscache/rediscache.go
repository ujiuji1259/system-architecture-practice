// Package rediscache implements repository.CountCache backed by Redis.
package rediscache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/repository"
)

// keyPrefix namespaces the count keys.
const keyPrefix = "count:"

// incrIfExists atomically increments the key only when it already exists,
// returning the new value, or -1 when the key is absent. Incrementing an absent
// key would fabricate a wrong count (e.g. 1 for a link with a long history), so
// we skip it and let the next read rebuild from the event log.
var incrIfExists = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
    return redis.call('INCR', KEYS[1])
end
return -1
`)

// Cache is a Redis-backed count cache. Entries are written with a TTL so that
// stale values are periodically rebuilt from the source of truth.
type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

var _ repository.CountCache = (*Cache)(nil)

// New returns a Cache using rdb, writing entries with the given TTL.
func New(rdb *redis.Client, ttl time.Duration) *Cache {
	return &Cache{rdb: rdb, ttl: ttl}
}

func (c *Cache) key(code string) string { return keyPrefix + code }

// Incr increments the count only if the key is present (see incrIfExists).
func (c *Cache) Incr(ctx context.Context, code string) error {
	return incrIfExists.Run(ctx, c.rdb, []string{c.key(code)}).Err()
}

// Get returns the cached count and whether it was present.
func (c *Cache) Get(ctx context.Context, code string) (int64, bool, error) {
	n, err := c.rdb.Get(ctx, c.key(code)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// Set warms the cache with an authoritative count, with a TTL.
func (c *Cache) Set(ctx context.Context, code string, count int64) error {
	return c.rdb.Set(ctx, c.key(code), count, c.ttl).Err()
}

// Del removes a code from the cache.
func (c *Cache) Del(ctx context.Context, code string) error {
	return c.rdb.Del(ctx, c.key(code)).Err()
}
