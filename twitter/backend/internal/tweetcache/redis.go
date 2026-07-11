package tweetcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
)

const keyPrefix = "tweet:"

// Redis is a Redis-backed Cache. Each tweet is stored as a JSON string under
// "tweet:{id}" with a TTL; reads use MGET so hydrating a whole timeline page is
// a single round trip.
type Redis struct {
	rdb *redis.Client
	ttl time.Duration
}

var _ Cache = (*Redis)(nil)

// NewRedis returns a Redis Cache using rdb, writing entries with the given TTL.
func NewRedis(rdb *redis.Client, ttl time.Duration) *Redis {
	return &Redis{rdb: rdb, ttl: ttl}
}

func (c *Redis) key(id int64) string {
	return keyPrefix + strconv.FormatInt(id, 10)
}

// cachedTweet is the JSON shape stored in Redis (independent of the domain type
// so the wire format is explicit).
type cachedTweet struct {
	ID           int64     `json:"id"`
	AuthorID     int64     `json:"author_id"`
	AuthorHandle string    `json:"author_handle"`
	Text         string    `json:"text"`
	CreatedAt    time.Time `json:"created_at"`
}

// GetMany fetches the cached subset of ids in a single MGET.
func (c *Redis) GetMany(ctx context.Context, ids []int64) (map[int64]repository.Tweet, error) {
	if len(ids) == 0 {
		return map[int64]repository.Tweet{}, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = c.key(id)
	}
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("mget tweets: %w", err)
	}
	out := make(map[int64]repository.Tweet, len(ids))
	for _, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue // miss (nil)
		}
		var ct cachedTweet
		if err := json.Unmarshal([]byte(s), &ct); err != nil {
			continue // treat a corrupt entry as a miss
		}
		out[ct.ID] = repository.Tweet{
			ID:           ct.ID,
			AuthorID:     ct.AuthorID,
			AuthorHandle: ct.AuthorHandle,
			Text:         ct.Text,
			CreatedAt:    ct.CreatedAt,
		}
	}
	return out, nil
}

// SetMany writes the given tweets in one pipeline, each with the configured TTL.
func (c *Redis) SetMany(ctx context.Context, tweets map[int64]repository.Tweet) error {
	if len(tweets) == 0 {
		return nil
	}
	pipe := c.rdb.Pipeline()
	for id, t := range tweets {
		b, err := json.Marshal(cachedTweet{
			ID:           t.ID,
			AuthorID:     t.AuthorID,
			AuthorHandle: t.AuthorHandle,
			Text:         t.Text,
			CreatedAt:    t.CreatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal tweet: %w", err)
		}
		pipe.Set(ctx, c.key(id), b, c.ttl)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("pipeline set tweets: %w", err)
	}
	return nil
}
