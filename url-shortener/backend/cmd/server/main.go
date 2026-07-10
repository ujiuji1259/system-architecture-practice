// Command server runs the URL shortener HTTP API.
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
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"

	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/api"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/handler"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/rediscache"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/service"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", envOr("ADDR", ":8080"), "HTTP listen address")
		dbPath    = flag.String("db", envOr("DB_PATH", "shortener.db"), "SQLite database path")
		baseURL   = flag.String("base-url", envOr("BASE_URL", "http://localhost:8080"), "public base URL for short links")
		redisAddr = flag.String("redis-addr", envOr("REDIS_ADDR", ""), "Redis address for the count cache; empty uses an in-memory cache")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The count cache is a repository-layer detail; the service is unaware of it.
	cache, closeCache, err := newCountCache(ctx, *redisAddr)
	if err != nil {
		return err
	}
	defer closeCache()

	store, err := repository.New(ctx, *dbPath, cache)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	svc := service.New(store)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newRouter(svc, *baseURL),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", *addr, "base_url", *baseURL, "db", *dbPath)
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
		return srv.Shutdown(shutdownCtx)
	}
}

// newRouter assembles the full HTTP handler: the generated API under /api/v1,
// the public redirect at the root, and a health check. baseURL is used to build
// short URLs in API responses.
func newRouter(svc *service.LinkService, baseURL string) http.Handler {
	h := handler.New(svc, baseURL)
	strict := api.NewStrictHandler(h, nil)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Public redirect endpoint, kept off the /api/v1 namespace.
	r.Get("/{code}", redirectHandler(svc))

	// Mount the generated API under /api/v1.
	api.HandlerWithOptions(strict, api.ChiServerOptions{
		BaseURL:    "/api/v1",
		BaseRouter: r,
	})
	return r
}

// redirectHandler resolves a short code to its target URL and 302-redirects,
// recording an access event (referer + user-agent) as a side effect.
func redirectHandler(svc *service.LinkService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		url, err := svc.Resolve(r.Context(), code, r.Referer(), r.UserAgent())
		if errors.Is(err, service.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			slog.Error("redirect lookup failed", "code", code, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, url, http.StatusFound)
	}
}

// cacheTTL bounds how long a Redis count entry lives before it is rebuilt from
// the event log, giving a coarse periodic reconciliation.
const cacheTTL = time.Hour

// newCountCache builds the repository count cache. With an empty redisAddr it
// uses an in-process cache; otherwise it connects to Redis (failing fast if it
// is unreachable). The returned func releases any resources.
func newCountCache(ctx context.Context, redisAddr string) (repository.CountCache, func(), error) {
	if redisAddr == "" {
		slog.Info("using in-memory count cache")
		return repository.NewMemoryCache(), func() {}, nil
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("connect redis at %s: %w", redisAddr, err)
	}
	slog.Info("using redis count cache", "addr", redisAddr)
	return rediscache.New(rdb, cacheTTL), func() { _ = rdb.Close() }, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
