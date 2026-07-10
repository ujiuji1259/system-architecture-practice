package timeline

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "tl:"

// pushScript adds a member (score 0, ordered by the padded id member), trims the
// timeline to the newest maxLen entries, and refreshes the TTL — in one atomic
// step. Ranks are ascending, so the newest ids have the highest ranks; removing
// ranks [0, -maxLen-1] drops the oldest and keeps the top maxLen. ARGV: [member,
// maxLen, ttlMillis]; ttlMillis <= 0 leaves the key persistent.
var pushScript = redis.NewScript(`
redis.call('ZADD', KEYS[1], 0, ARGV[1])
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -tonumber(ARGV[2]) - 1)
if tonumber(ARGV[3]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[3]) end
return 1
`)

// fillScript adds many members to one timeline, trims, and sets the TTL. ARGV:
// [maxLen, ttlMillis, member1, member2, ...].
var fillScript = redis.NewScript(`
for i = 3, #ARGV do
  redis.call('ZADD', KEYS[1], 0, ARGV[i])
end
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -tonumber(ARGV[1]) - 1)
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 1
`)

// Redis is a Redis-backed Store. Each timeline is a ZSET at "tl:{userID}" whose
// members are zero-padded tweet ids (all score 0), ordered lexicographically.
type Redis struct {
	rdb *redis.Client
	max int
	ttl time.Duration // 0 keeps timelines persistent
}

var _ Store = (*Redis)(nil)

// NewRedis returns a Redis-backed Store capping each timeline at maxLen entries.
// A non-zero ttl makes timelines expire, so inactive users' timelines are
// evicted and rebuilt from the source of truth on their next read.
func NewRedis(rdb *redis.Client, maxLen int, ttl time.Duration) *Redis {
	return &Redis{rdb: rdb, max: maxLen, ttl: ttl}
}

// ttlMillis returns the TTL in milliseconds (0 = persistent).
func (r *Redis) ttlMillis() int64 { return r.ttl.Milliseconds() }

func (r *Redis) key(userID int64) string {
	return fmt.Sprintf("%s%d", keyPrefix, userID)
}

// Push adds tweetID to userID's timeline, trimming to the cap.
func (r *Redis) Push(ctx context.Context, userID, tweetID int64) error {
	return r.push(ctx, r.key(userID), tweetID)
}

// PushMany adds tweetID to each user's timeline. The per-user add+trim scripts
// are pipelined into a single round trip.
//
// Inside a pipeline the script runs via EVALSHA, which cannot transparently fall
// back to EVAL the way a single Script.Run does. So if the script is not in
// Redis's cache yet (first use, or after a Redis restart/failover) the pipeline
// fails with NOSCRIPT; we then SCRIPT LOAD it and retry once.
func (r *Redis) PushMany(ctx context.Context, userIDs []int64, tweetID int64) error {
	if len(userIDs) == 0 {
		return nil
	}
	member := pad(tweetID)
	run := func() error {
		pipe := r.rdb.Pipeline()
		for _, u := range userIDs {
			pushScript.Run(ctx, pipe, []string{r.key(u)}, member, r.max, r.ttlMillis())
		}
		_, err := pipe.Exec(ctx)
		return err
	}

	err := run()
	if err != nil && redis.HasErrorPrefix(err, "NOSCRIPT") {
		if _, lerr := pushScript.Load(ctx, r.rdb).Result(); lerr != nil {
			return fmt.Errorf("load push script: %w", lerr)
		}
		err = run()
	}
	if err != nil {
		return fmt.Errorf("fan-out push: %w", err)
	}
	return nil
}

// Fill (re)populates one user's timeline with the given ids, trims to the cap,
// and sets the TTL. Used by a rebuild. It runs as a single Script.Run, so it
// falls back to EVAL automatically and never hits NOSCRIPT.
func (r *Redis) Fill(ctx context.Context, userID int64, tweetIDs []int64) error {
	if len(tweetIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(tweetIDs)+2)
	args = append(args, r.max, r.ttlMillis())
	for _, id := range tweetIDs {
		args = append(args, pad(id))
	}
	if err := fillScript.Run(ctx, r.rdb, []string{r.key(userID)}, args...).Err(); err != nil {
		return fmt.Errorf("timeline fill: %w", err)
	}
	return nil
}

func (r *Redis) push(ctx context.Context, key string, tweetID int64) error {
	if err := pushScript.Run(ctx, r.rdb, []string{key}, pad(tweetID), r.max, r.ttlMillis()).Err(); err != nil {
		return fmt.Errorf("timeline push: %w", err)
	}
	return nil
}

// Range returns up to limit ids with id < beforeID, newest first, via a reverse
// lexicographic range over the padded-id members.
func (r *Redis) Range(ctx context.Context, userID, beforeID int64, limit int) ([]int64, error) {
	upper := "+"
	if beforeID > 0 {
		upper = "(" + pad(beforeID) // exclusive upper bound
	}
	// With Rev the bounds are given high-to-low: Start is the (exclusive) upper
	// bound, Stop the lower.
	members, err := r.rdb.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:   r.key(userID),
		Start: upper,
		Stop:  "-",
		ByLex: true,
		Rev:   true,
		Count: int64(limit),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("timeline range: %w", err)
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		id, err := unpad(m)
		if err != nil {
			return nil, fmt.Errorf("parse member %q: %w", m, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
