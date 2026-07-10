// Command server runs the mini-Twitter HTTP API and its fan-out workers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/api"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/fanout"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/handler"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/hometimeline"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/hometimeline/store"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/rediscache"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/service"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/snowflake"
)

// tweetCacheTTL bounds how long a hydrated tweet body lives in Redis.
const tweetCacheTTL = time.Hour

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", envOr("ADDR", ":8080"), "HTTP listen address")
		dbPath    = flag.String("db", envOr("DB_PATH", "twitter.db"), "SQLite database path")
		redisAddr = flag.String("redis-addr", envOr("REDIS_ADDR", ""), "Redis address; empty uses in-memory timeline/bus/cache")
		threshold = flag.Int64("celebrity-threshold", envInt("FANOUT_CELEBRITY_THRESHOLD", 10000), "authors with more followers than this are pulled at read time, not fanned out")
		workers   = flag.Int("workers", int(envInt("FANOUT_WORKERS", 4)), "number of fan-out worker goroutines")
		maxLen    = flag.Int("timeline-max", int(envInt("TIMELINE_MAX_LEN", 800)), "max entries kept per materialized timeline")
		ttl       = flag.Duration("timeline-ttl", envDuration("TIMELINE_TTL", 0), "materialized timeline TTL (0 = persistent); expiry evicts inactive users, rebuilt on next read")
		machineID = flag.Int64("machine-id", envInt("MACHINE_ID", 1), "snowflake machine id (0..1023)")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Choose Redis-backed or in-memory implementations for every derived-data
	// component in one place.
	deps, closeDeps, err := newDeps(ctx, *redisAddr, *maxLen, *ttl)
	if err != nil {
		return err
	}
	defer closeDeps()

	repo, err := repository.New(ctx, *dbPath, deps.tweetCache)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()

	// One CelebrityPolicy, shared by the projection (write + rebuild) and the
	// service (read-time celebrity pull), so all three agree on the cut point.
	policy := hometimeline.CelebrityPolicy{Threshold: *threshold}
	projection := hometimeline.New(repo, deps.store, hometimeline.Config{Policy: policy, MaxLen: *maxLen})

	gen := snowflake.New(*machineID)
	svc := service.New(repo, projection, deps.bus, gen)

	// Fan-out runtime: a worker pool that delivers TweetPosted events to the
	// projection's Apply.
	worker := fanout.NewWorker(deps.bus, projection)
	pool := fanout.NewPool(worker, *workers)
	pool.Start(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newRouter(svc),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", *addr, "db", *dbPath, "redis", *redisAddr,
			"workers", *workers, "celebrity_threshold", *threshold, "timeline_max", *maxLen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		pool.Wait() // workers stop on ctx cancellation; wait for them to drain
		m := projection.Metrics()
		slog.Info("projection stats", "fanned", m.Fanned, "skipped", m.Skipped, "edges", m.Edges, "rebuilt", m.Rebuilt)
		return err
	}
}

// eventBus is the union of event behaviors: the command publishes (Publish) and
// the projector subscribes (Poll). One instance serves both.
type eventBus interface {
	Publish(ctx context.Context, e events.TweetPosted) error
	Poll(ctx context.Context) (events.TweetPosted, bool, error)
}

// deps bundles the swappable derived-data backends (Redis or in-memory).
type deps struct {
	tweetCache repository.TweetCache
	store      store.Store
	bus        eventBus
}

// newDeps builds the derived-data components. With an empty redisAddr it uses
// in-process implementations (no external dependency); otherwise it connects to
// Redis, failing fast if it is unreachable.
func newDeps(ctx context.Context, redisAddr string, maxLen int, ttl time.Duration) (deps, func(), error) {
	if redisAddr == "" {
		slog.Info("using in-memory timeline, event bus and tweet cache")
		return deps{
			tweetCache: repository.NewMemoryCache(),
			store:      store.NewMemory(maxLen),
			bus:        events.NewMemoryBus(0),
		}, func() {}, nil
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return deps{}, nil, fmt.Errorf("connect redis at %s: %w", redisAddr, err)
	}
	slog.Info("using redis timeline, event bus and tweet cache", "addr", redisAddr)
	return deps{
		tweetCache: rediscache.New(rdb, tweetCacheTTL),
		store:      store.NewRedis(rdb, maxLen, ttl),
		bus:        events.NewRedisBus(rdb, 0),
	}, func() { _ = rdb.Close() }, nil
}

// newRouter assembles the HTTP handler: the generated API under /api/v1 plus a
// health check.
func newRouter(svc *service.Service) http.Handler {
	h := handler.New(svc)
	strict := api.NewStrictHandler(h, nil)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	api.HandlerWithOptions(strict, api.ChiServerOptions{
		BaseURL:    "/api/v1",
		BaseRouter: r,
	})
	return r
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
