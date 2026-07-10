package rediscache

import (
	"context"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// startRedis boots a throwaway Redis in a container. It skips the test when no
// container runtime (Docker) is available.
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

func TestRedisCacheIncrOnlyWhenPresent(t *testing.T) {
	rdb := startRedis(t)
	c := New(rdb, time.Minute)
	ctx := context.Background()

	// Cold key: Incr must be a no-op, leaving the key absent.
	if err := c.Incr(ctx, "cold"); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if _, ok, err := c.Get(ctx, "cold"); err != nil || ok {
		t.Fatalf("Get cold = ok=%v err=%v, want absent", ok, err)
	}

	// Warmed key: Set then Incr increments from the seeded value.
	if err := c.Set(ctx, "warm", 10); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Incr(ctx, "warm"); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	n, ok, err := c.Get(ctx, "warm")
	if err != nil || !ok || n != 11 {
		t.Fatalf("Get warm = (%d,%v,%v), want (11,true,nil)", n, ok, err)
	}

	// Del removes the key.
	if err := c.Del(ctx, "warm"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, ok, _ := c.Get(ctx, "warm"); ok {
		t.Fatal("want absent after Del")
	}
}
