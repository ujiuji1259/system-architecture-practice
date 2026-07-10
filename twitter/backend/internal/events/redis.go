package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// streamKey is the Redis list used as the TweetPosted event channel. LPUSH
// publishes, BRPOP consumes; it survives restarts so projection resumes after a
// crash.
const streamKey = "events:tweet-posted"

// RedisBus is a Redis-backed event bus (a list used as a FIFO channel).
type RedisBus struct {
	rdb   *redis.Client
	block time.Duration
}

// NewRedisBus returns a RedisBus. block bounds each BRPOP so context
// cancellation is noticed promptly; pass 0 for a 1s default.
func NewRedisBus(rdb *redis.Client, block time.Duration) *RedisBus {
	if block <= 0 {
		block = time.Second
	}
	return &RedisBus{rdb: rdb, block: block}
}

// Publish pushes an event onto the head of the list.
func (b *RedisBus) Publish(ctx context.Context, e TweetPosted) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if err := b.rdb.LPush(ctx, streamKey, payload).Err(); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// Poll blocks (in block-sized waits) for the next event, retrying on timeout
// until one arrives or ctx is done.
func (b *RedisBus) Poll(ctx context.Context) (TweetPosted, bool, error) {
	for {
		if ctx.Err() != nil {
			return TweetPosted{}, false, nil
		}
		res, err := b.rdb.BRPop(ctx, b.block, streamKey).Result()
		if errors.Is(err, redis.Nil) {
			continue // timed out with nothing; retry
		}
		if err != nil {
			if ctx.Err() != nil {
				return TweetPosted{}, false, nil
			}
			return TweetPosted{}, false, fmt.Errorf("poll: %w", err)
		}
		// res is [key, value].
		var e TweetPosted
		if err := json.Unmarshal([]byte(res[1]), &e); err != nil {
			slog.Warn("events: skipping malformed event", "value", res[1])
			continue
		}
		return e, true, nil
	}
}
