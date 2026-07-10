// Package repository provides persistence for shortened links.
//
// SQL lives in internal/repository/queries and is compiled into type-safe Go by
// sqlc (see sqlc.yaml). This file adapts that generated code to the domain
// model, mapping driver errors to sentinel errors and converting the TEXT
// created_at column to time.Time.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// timeLayout is how created_at is stored in the TEXT column.
const timeLayout = time.RFC3339Nano

// Link is the domain representation of a shortened URL.
type Link struct {
	Code       string
	URL        string
	VisitCount int64
	CreatedAt  time.Time
}

// SQLite is a SQLite-backed link repository built on sqlc-generated queries.
type SQLite struct {
	db *sql.DB
	q  *dbgen.Queries
}

// New opens (or creates) a SQLite database at dsn and applies the schema.
func New(ctx context.Context, dsn string) (*SQLite, error) {
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite handles concurrent writes poorly; keep a single connection.
	database.SetMaxOpenConns(1)
	if _, err := database.ExecContext(ctx, dbpkg.Schema); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: database, q: dbgen.New(database)}, nil
}

// Close releases the underlying database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// Create inserts a new link. It returns ErrCodeExists if the code is taken.
func (s *SQLite) Create(ctx context.Context, l Link) error {
	err := s.q.CreateLink(ctx, dbgen.CreateLinkParams{
		Code:       l.Code,
		Url:        l.URL,
		VisitCount: l.VisitCount,
		CreatedAt:  l.CreatedAt.UTC().Format(timeLayout),
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

// Get returns the link for a code, or ErrNotFound.
func (s *SQLite) Get(ctx context.Context, code string) (Link, error) {
	row, err := s.q.GetLink(ctx, code)
	return toDomain(row, err)
}

// GetAndCountVisit atomically increments the visit count and returns the link.
// It returns ErrNotFound if the code does not exist.
func (s *SQLite) GetAndCountVisit(ctx context.Context, code string) (Link, error) {
	row, err := s.q.CountVisit(ctx, code)
	return toDomain(row, err)
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
		l, err := fromRow(r)
		if err != nil {
			return nil, 0, err
		}
		links[i] = l
	}
	return links, total, nil
}

// Delete removes a link by code. It returns ErrNotFound if it did not exist.
func (s *SQLite) Delete(ctx context.Context, code string) error {
	n, err := s.q.DeleteLink(ctx, code)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// toDomain maps a single-row query result (and its error) to the domain model.
func toDomain(r dbgen.Link, err error) (Link, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("query link: %w", err)
	}
	return fromRow(r)
}

// fromRow converts a generated row into a domain Link, parsing created_at.
func fromRow(r dbgen.Link) (Link, error) {
	t, err := time.Parse(timeLayout, r.CreatedAt)
	if err != nil {
		return Link{}, fmt.Errorf("parse created_at: %w", err)
	}
	return Link{
		Code:       r.Code,
		URL:        r.Url,
		VisitCount: r.VisitCount,
		CreatedAt:  t,
	}, nil
}
