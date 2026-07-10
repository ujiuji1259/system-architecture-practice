// Package repository provides persistence for shortened links and their
// access events.
//
// SQL lives in internal/repository/queries and is compiled into type-safe Go by
// sqlc (see sqlc.yaml). This file adapts that generated code to the domain
// model, mapping driver errors to sentinel errors and converting TEXT timestamp
// columns to time.Time. The visit count is derived by counting access events,
// so there is a single source of truth.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	dbpkg "github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/db"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/repository/dbgen"
)

// Sentinel errors returned by the repository.
var (
	// ErrNotFound is returned when a link does not exist.
	ErrNotFound = errors.New("link not found")
	// ErrCodeExists is returned when a code is already taken.
	ErrCodeExists = errors.New("code already exists")
)

// timeLayout is how timestamps are stored in TEXT columns.
const timeLayout = time.RFC3339Nano

// Link is the domain representation of a shortened URL.
type Link struct {
	Code       string
	URL        string
	VisitCount int64
	CreatedAt  time.Time
}

// Event is a single access to a shortened link.
type Event struct {
	Code       string
	AccessedAt time.Time
	Referer    string
	UserAgent  string
}

// SQLite is a SQLite-backed repository built on sqlc-generated queries. The
// event log is the source of truth; visit counts are served from a CountCache
// (rebuilt from the log on a miss), which is an internal persistence concern.
type SQLite struct {
	db    *sql.DB
	q     *dbgen.Queries
	cache CountCache
}

// New opens (or creates) a SQLite database at dsn and applies the schema. cache
// backs the visit counter; pass nil to use an in-process cache.
func New(ctx context.Context, dsn string, cache CountCache) (*SQLite, error) {
	if cache == nil {
		cache = NewMemoryCache()
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite handles concurrent writes poorly; keep a single connection.
	database.SetMaxOpenConns(1)
	// Enforce foreign keys so ON DELETE CASCADE removes a link's events.
	if _, err := database.ExecContext(ctx, `PRAGMA foreign_keys = ON;`); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	if _, err := database.ExecContext(ctx, dbpkg.Schema); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: database, q: dbgen.New(database), cache: cache}, nil
}

// Close releases the underlying database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// Create inserts a new link. It returns ErrCodeExists if the code is taken.
func (s *SQLite) Create(ctx context.Context, l Link) error {
	err := s.q.CreateLink(ctx, dbgen.CreateLinkParams{
		Code:      l.Code,
		Url:       l.URL,
		CreatedAt: l.CreatedAt.UTC().Format(timeLayout),
	})
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
		return ErrCodeExists
	}
	if err != nil {
		return fmt.Errorf("insert link: %w", err)
	}
	return nil
}

// Get returns the link for a code, or ErrNotFound. The visit count is served
// from the cache (O(1)); on a miss it is rebuilt from the event log and cached.
func (s *SQLite) Get(ctx context.Context, code string) (Link, error) {
	link, err := s.getMeta(ctx, code)
	if err != nil {
		return Link{}, err
	}
	link.VisitCount, err = s.visitCount(ctx, code)
	if err != nil {
		return Link{}, err
	}
	return link, nil
}

// getMeta is an O(1) primary-key lookup of a link's fields (without the count).
func (s *SQLite) getMeta(ctx context.Context, code string) (Link, error) {
	row, err := s.q.GetLinkMeta(ctx, code)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("get link meta: %w", err)
	}
	return toLink(row.Code, row.Url, row.CreatedAt, 0)
}

// visitCount returns the count from the cache, rebuilding it from the event log
// (the source of truth) and warming the cache on a miss.
func (s *SQLite) visitCount(ctx context.Context, code string) (int64, error) {
	if n, ok, err := s.cache.Get(ctx, code); err != nil {
		slog.Warn("count cache get failed", "code", code, "err", err)
	} else if ok {
		return n, nil
	}
	n, err := s.countEvents(ctx, code)
	if err != nil {
		return 0, err
	}
	if err := s.cache.Set(ctx, code, n); err != nil {
		slog.Warn("count cache set failed", "code", code, "err", err)
	}
	return n, nil
}

// countEvents is the authoritative visit count: an O(N) COUNT over the event
// log, used to (re)build the cache.
func (s *SQLite) countEvents(ctx context.Context, code string) (int64, error) {
	n, err := s.q.CountEvents(ctx, code)
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// List returns a page of links ordered by newest first, plus the total count.
func (s *SQLite) List(ctx context.Context, limit, offset int) ([]Link, int64, error) {
	total, err := s.q.CountLinks(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count links: %w", err)
	}
	rows, err := s.q.ListLinks(ctx, dbgen.ListLinksParams{
		Limit:  int64(limit),
		Offset: int64(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list links: %w", err)
	}
	links := make([]Link, len(rows))
	for i, r := range rows {
		l, err := toLink(r.Code, r.Url, r.CreatedAt, r.VisitCount)
		if err != nil {
			return nil, 0, err
		}
		links[i] = l
	}
	return links, total, nil
}

// Delete removes a link by code. It returns ErrNotFound if it did not exist.
// Associated events are removed by the ON DELETE CASCADE constraint, and the
// cached count is evicted.
func (s *SQLite) Delete(ctx context.Context, code string) error {
	n, err := s.q.DeleteLink(ctx, code)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := s.cache.Del(ctx, code); err != nil {
		slog.Warn("count cache del failed", "code", code, "err", err)
	}
	return nil
}

// RecordVisit records an access event for a code and returns the target URL.
// The lookup and insert run in one transaction. Returns ErrNotFound if the
// code does not exist.
func (s *SQLite) RecordVisit(ctx context.Context, code string, ev Event) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	url, err := q.GetLinkURL(ctx, code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get link url: %w", err)
	}
	if err := q.InsertEvent(ctx, dbgen.InsertEventParams{
		Code:       code,
		AccessedAt: ev.AccessedAt.UTC().Format(timeLayout),
		Referer:    ev.Referer,
		UserAgent:  ev.UserAgent,
	}); err != nil {
		return "", fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	// The event is durably recorded; bump the cache best-effort. If the code is
	// not cached this is a no-op and the count is rebuilt on the next read.
	if err := s.cache.Incr(ctx, code); err != nil {
		slog.Warn("count cache incr failed", "code", code, "err", err)
	}
	return url, nil
}

// ListEvents returns a page of access events for a code, newest first, plus the
// total count. It does not distinguish a missing link from one with no events.
func (s *SQLite) ListEvents(ctx context.Context, code string, limit, offset int) ([]Event, int64, error) {
	total, err := s.q.CountEvents(ctx, code)
	if err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}
	rows, err := s.q.ListEvents(ctx, dbgen.ListEventsParams{
		Code:   code,
		Limit:  int64(limit),
		Offset: int64(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list events: %w", err)
	}
	events := make([]Event, len(rows))
	for i, r := range rows {
		t, err := time.Parse(timeLayout, r.AccessedAt)
		if err != nil {
			return nil, 0, fmt.Errorf("parse accessed_at: %w", err)
		}
		events[i] = Event{
			Code:       r.Code,
			AccessedAt: t,
			Referer:    r.Referer,
			UserAgent:  r.UserAgent,
		}
	}
	return events, total, nil
}

// toLink builds a domain Link, parsing the TEXT created_at column.
func toLink(code, url, createdAt string, visitCount int64) (Link, error) {
	t, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return Link{}, fmt.Errorf("parse created_at: %w", err)
	}
	return Link{Code: code, URL: url, VisitCount: visitCount, CreatedAt: t}, nil
}
