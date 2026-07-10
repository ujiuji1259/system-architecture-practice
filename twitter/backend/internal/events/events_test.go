package events

import (
	"context"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// bus is the behavior both implementations share.
type bus interface {
	Publish(ctx context.Context, e TweetPosted) error
	Poll(ctx context.Context) (TweetPosted, bool, error)
}

func runBusSuite(t *testing.T, b bus) {
	t.Run("publish then poll preserves the event", func(t *testing.T) {
		ctx := context.Background()
		want := TweetPosted{TweetID: 333890670651510784, AuthorID: 42}
		if err := b.Publish(ctx, want); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		got, ok, err := b.Poll(ctx)
		if err != nil || !ok {
			t.Fatalf("Poll = (ok=%v, err=%v), want an event", ok, err)
		}
		if got != want {
			t.Fatalf("event = %+v, want %+v", got, want)
		}
	})

	t.Run("poll returns ok=false when context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, ok, err := b.Poll(ctx)
		if ok || err != nil {
			t.Fatalf("Poll on done ctx = (ok=%v, err=%v), want (false, nil)", ok, err)
		}
	})

	t.Run("FIFO order", func(t *testing.T) {
		ctx := context.Background()
		for _, id := range []int64{1, 2, 3} {
			if err := b.Publish(ctx, TweetPosted{TweetID: id}); err != nil {
				t.Fatalf("Publish: %v", err)
			}
		}
		for _, want := range []int64{1, 2, 3} {
			got, _, _ := b.Poll(ctx)
			if got.TweetID != want {
				t.Fatalf("polled %d, want %d (FIFO)", got.TweetID, want)
			}
		}
	})
}

func TestMemoryBus(t *testing.T) {
	runBusSuite(t, NewMemoryBus(16))
}

func TestRedisBus(t *testing.T) {
	rdb := startRedis(t)
	runBusSuite(t, NewRedisBus(rdb, 200*time.Millisecond))
}

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
