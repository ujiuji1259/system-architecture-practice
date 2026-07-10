package store

import (
	"context"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// runStoreSuite exercises the Store contract against any implementation.
func runStoreSuite(t *testing.T, newStore func(maxLen int) Store) {
	ctx := context.Background()

	t.Run("newest first with cap", func(t *testing.T) {
		s := newStore(3)
		for _, id := range []int64{10, 20, 30, 40, 50} {
			if err := s.Push(ctx, 1, id); err != nil {
				t.Fatalf("Push: %v", err)
			}
		}
		got, err := s.Range(ctx, 1, 0, 10)
		if err != nil {
			t.Fatalf("Range: %v", err)
		}
		want := []int64{50, 40, 30} // capped to the newest 3
		assertIDs(t, got, want)
	})

	t.Run("cursor excludes ids >= beforeID", func(t *testing.T) {
		s := newStore(10)
		for _, id := range []int64{10, 20, 30, 40, 50} {
			mustPush(t, s, 1, id)
		}
		got, err := s.Range(ctx, 1, 40, 10)
		if err != nil {
			t.Fatalf("Range: %v", err)
		}
		assertIDs(t, got, []int64{30, 20, 10})
	})

	t.Run("limit bounds the page", func(t *testing.T) {
		s := newStore(10)
		for _, id := range []int64{1, 2, 3, 4, 5} {
			mustPush(t, s, 1, id)
		}
		got, err := s.Range(ctx, 1, 0, 2)
		if err != nil {
			t.Fatalf("Range: %v", err)
		}
		assertIDs(t, got, []int64{5, 4})
	})

	t.Run("duplicate push is idempotent", func(t *testing.T) {
		s := newStore(10)
		mustPush(t, s, 1, 42)
		mustPush(t, s, 1, 42)
		got, _ := s.Range(ctx, 1, 0, 10)
		assertIDs(t, got, []int64{42})
	})

	t.Run("PushMany fans a tweet to many users", func(t *testing.T) {
		s := newStore(10)
		if err := s.PushMany(ctx, []int64{1, 2, 3}, 99); err != nil {
			t.Fatalf("PushMany: %v", err)
		}
		for _, u := range []int64{1, 2, 3} {
			got, _ := s.Range(ctx, u, 0, 10)
			assertIDs(t, got, []int64{99})
		}
		// A non-recipient is unaffected.
		if got, _ := s.Range(ctx, 4, 0, 10); len(got) != 0 {
			t.Fatalf("user 4 timeline = %v, want empty", got)
		}
	})

	t.Run("Fill repopulates one timeline, capped", func(t *testing.T) {
		s := newStore(3)
		if err := s.Fill(ctx, 1, []int64{10, 20, 30, 40, 50}); err != nil {
			t.Fatalf("Fill: %v", err)
		}
		got, _ := s.Range(ctx, 1, 0, 10)
		assertIDs(t, got, []int64{50, 40, 30}) // newest 3
	})

	t.Run("large ids keep exact order", func(t *testing.T) {
		// Two ids in the same millisecond differ only in low bits — these would
		// collide if scored as float64.
		s := newStore(10)
		a := int64(333890670651510784)
		b := a + 1
		mustPush(t, s, 1, a)
		mustPush(t, s, 1, b)
		got, _ := s.Range(ctx, 1, 0, 10)
		assertIDs(t, got, []int64{b, a})
	})
}

func TestMemoryStore(t *testing.T) {
	runStoreSuite(t, func(maxLen int) Store { return NewMemory(maxLen) })
}

func TestRedisStore(t *testing.T) {
	rdb := startRedis(t)
	// Each subtest calls the factory once at its start; flush there so subtests
	// that reuse the same user id don't contaminate one another.
	runStoreSuite(t, func(maxLen int) Store {
		if err := rdb.FlushDB(context.Background()).Err(); err != nil {
			t.Fatalf("flushdb: %v", err)
		}
		return NewRedis(rdb, maxLen, 0)
	})
}

// TestRedisPushManyColdScriptCache reproduces the production path where the
// fan-out worker's pipelined PushMany is the first script use (no prior single
// Push preloaded it). With a cold script cache the pipelined EVALSHA would fail
// NOSCRIPT unless PushMany loads-and-retries.
func TestRedisPushManyColdScriptCache(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	if err := rdb.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("script flush: %v", err)
	}
	s := NewRedis(rdb, 10, 0)
	if err := s.PushMany(ctx, []int64{1, 2}, 99); err != nil {
		t.Fatalf("PushMany with cold script cache: %v", err)
	}
	got, err := s.Range(ctx, 1, 0, 10)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	assertIDs(t, got, []int64{99})
}

// TestRedisTTL verifies that a non-zero TTL sets an expiry on the timeline key,
// so inactive users' timelines are eventually evicted (and rebuilt on read).
func TestRedisTTL(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	s := NewRedis(rdb, 10, time.Hour)
	if err := s.Push(ctx, 1, 42); err != nil {
		t.Fatalf("Push: %v", err)
	}
	ttl, err := rdb.PTTL(ctx, "tl:1").Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("PTTL = %v, want a positive expiry", ttl)
	}
}

func mustPush(t *testing.T, s Store, userID, tweetID int64) {
	t.Helper()
	if err := s.Push(context.Background(), userID, tweetID); err != nil {
		t.Fatalf("Push: %v", err)
	}
}

func assertIDs(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// startRedis boots a throwaway Redis, skipping when Docker is unavailable.
func startRedis(t *testing.T) *goredis.Client {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:7-alpine")
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	opt, err := goredis.ParseURL(endpoint)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	rdb := goredis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}
