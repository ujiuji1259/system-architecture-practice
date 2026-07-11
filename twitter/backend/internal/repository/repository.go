// Package repository provides SQLite-backed persistence for users, the follow
// graph, and tweets — the source of truth. SQL is written inline with sqlx
// (this service is small enough that a codegen step would not pay for itself).
//
// The repository owns two derived-data conveniences: a denormalized
// users.follower_count (so the celebrity check is O(1)) and an injected tweet
// cache for read-side body hydration. The cache is taken through a structural
// interface (tweetCache) so the concrete cache implementations live in
// internal/tweetcache and this package stays independent of them.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jmoiron/sqlx"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	dbpkg "github.com/ujiuji1259/system-architecture-practice/twitter/backend/db"
)

func init() {
	// modernc registers its driver as "sqlite"; teach sqlx to emit "?" bindvars
	// for it so sqlx.In / Rebind work.
	sqlx.BindDriver("sqlite", sqlx.QUESTION)
}

// Sentinel errors returned by the repository.
var (
	// ErrNotFound is returned when a referenced row (user or tweet) is absent.
	ErrNotFound = errors.New("not found")
	// ErrHandleExists is returned when a user handle is already taken.
	ErrHandleExists = errors.New("handle already exists")
)

// timeLayout is how timestamps are stored in TEXT columns.
const timeLayout = time.RFC3339Nano

// User is the domain representation of an account.
type User struct {
	ID            int64
	Handle        string
	DisplayName   string
	FollowerCount int64
	CreatedAt     time.Time
}

// Tweet is the domain representation of a tweet (author handle denormalized in
// for cheap rendering).
type Tweet struct {
	ID           int64
	AuthorID     int64
	AuthorHandle string
	Text         string
	CreatedAt    time.Time
}

// tweetCache is the read-side cache repository consults before falling back to
// SQLite. It is a structural interface so callers can pass any implementation
// from internal/tweetcache without this package importing it.
type tweetCache interface {
	GetMany(ctx context.Context, ids []int64) (map[int64]Tweet, error)
	SetMany(ctx context.Context, tweets map[int64]Tweet) error
}

// SQLite is a sqlx-backed repository. cache hydrates tweet bodies on the read
// path.
type SQLite struct {
	db    *sqlx.DB
	cache tweetCache
}

// New opens (or creates) a SQLite database at dsn and applies the schema. The
// caller supplies the tweet cache (see internal/tweetcache).
func New(ctx context.Context, dsn string, cache tweetCache) (*SQLite, error) {
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite handles concurrent writes poorly; keep a single connection.
	raw.SetMaxOpenConns(1)
	if _, err := raw.ExecContext(ctx, `PRAGMA foreign_keys = ON;`); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	if _, err := raw.ExecContext(ctx, dbpkg.Schema); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: sqlx.NewDb(raw, "sqlite"), cache: cache}, nil
}

// Close releases the underlying database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// --- users ---------------------------------------------------------------

// CreateUser inserts a new user. It returns ErrHandleExists if the handle is
// taken (the caller supplies the Snowflake id and creation time).
func (s *SQLite) CreateUser(ctx context.Context, u User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, handle, display_name, follower_count, created_at)
		 VALUES (?, ?, ?, 0, ?)`,
		u.ID, u.Handle, u.DisplayName, u.CreatedAt.UTC().Format(timeLayout))
	if isConstraint(err, sqlite3.SQLITE_CONSTRAINT_UNIQUE) {
		return ErrHandleExists
	}
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// GetUser returns the user for an id, or ErrNotFound.
func (s *SQLite) GetUser(ctx context.Context, id int64) (User, error) {
	var r userRow
	err := s.db.GetContext(ctx, &r,
		`SELECT id, handle, display_name, follower_count, created_at
		 FROM users WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return r.toUser()
}

// --- follow graph --------------------------------------------------------

// Follow makes follower follow followee. It is idempotent (a duplicate is a
// no-op) and bumps the followee's follower_count only on a genuine new edge, in
// one transaction. Returns ErrNotFound if either user does not exist.
func (s *SQLite) Follow(ctx context.Context, followerID, followeeID int64) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO follows (follower_id, followee_id, created_at) VALUES (?, ?, ?)`,
		followerID, followeeID, time.Now().UTC().Format(timeLayout))
	switch {
	case isConstraint(err, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY):
		return nil // already following
	case isConstraint(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY):
		return ErrNotFound // one of the users is missing
	case err != nil:
		return fmt.Errorf("insert follow: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET follower_count = follower_count + 1 WHERE id = ?`, followeeID); err != nil {
		return fmt.Errorf("bump follower_count: %w", err)
	}
	return tx.Commit()
}

// Unfollow removes the edge. It is idempotent; if the edge was absent it
// returns nil when the followee exists and ErrNotFound when it does not (so a
// missing user is still reportable). Decrements follower_count on a real
// removal, in one transaction.
func (s *SQLite) Unfollow(ctx context.Context, followerID, followeeID int64) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM follows WHERE follower_id = ? AND followee_id = ?`, followerID, followeeID)
	if err != nil {
		return fmt.Errorf("delete follow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Nothing removed: distinguish "not following" from "no such user".
		var exists int
		if err := tx.GetContext(ctx, &exists, `SELECT 1 FROM users WHERE id = ?`, followeeID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("check followee: %w", err)
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET follower_count = follower_count - 1 WHERE id = ?`, followeeID); err != nil {
		return fmt.Errorf("drop follower_count: %w", err)
	}
	return tx.Commit()
}

// FollowersPage returns up to limit follower ids of followeeID whose id is
// greater than afterID, ascending. Keyset paging (by follower id) lets the
// fan-out worker walk millions of followers with stable, index-only pages.
func (s *SQLite) FollowersPage(ctx context.Context, followeeID, afterID int64, limit int) ([]int64, error) {
	var ids []int64
	err := s.db.SelectContext(ctx, &ids,
		`SELECT follower_id FROM follows
		 WHERE followee_id = ? AND follower_id > ?
		 ORDER BY follower_id ASC LIMIT ?`,
		followeeID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("followers page: %w", err)
	}
	return ids, nil
}

// --- tweets --------------------------------------------------------------

// CreateTweet inserts a tweet. Returns ErrNotFound if the author is missing.
func (s *SQLite) CreateTweet(ctx context.Context, t Tweet) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tweets (id, author_id, text, created_at) VALUES (?, ?, ?, ?)`,
		t.ID, t.AuthorID, t.Text, t.CreatedAt.UTC().Format(timeLayout))
	if isConstraint(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("insert tweet: %w", err)
	}
	return nil
}

// TweetAuthor is the minimal author info the fan-out worker needs to decide
// push-vs-pull for a tweet.
type TweetAuthor struct {
	AuthorID      int64
	FollowerCount int64
}

// GetTweetAuthor returns the author id and their follower_count for a tweet.
func (s *SQLite) GetTweetAuthor(ctx context.Context, tweetID int64) (TweetAuthor, error) {
	var a struct {
		AuthorID      int64 `db:"author_id"`
		FollowerCount int64 `db:"follower_count"`
	}
	err := s.db.GetContext(ctx, &a,
		`SELECT t.author_id, u.follower_count
		 FROM tweets t JOIN users u ON u.id = t.author_id
		 WHERE t.id = ?`, tweetID)
	if errors.Is(err, sql.ErrNoRows) {
		return TweetAuthor{}, ErrNotFound
	}
	if err != nil {
		return TweetAuthor{}, fmt.Errorf("get tweet author: %w", err)
	}
	return TweetAuthor{AuthorID: a.AuthorID, FollowerCount: a.FollowerCount}, nil
}

// UserTweets returns up to limit of a user's own tweets with id < beforeID,
// newest first (the pull-based user timeline). beforeID <= 0 means "from newest".
func (s *SQLite) UserTweets(ctx context.Context, authorID, beforeID int64, limit int) ([]Tweet, error) {
	beforeID = normalizeCursor(beforeID)
	var rows []tweetRow
	err := s.db.SelectContext(ctx, &rows,
		`SELECT t.id, t.author_id, u.handle AS author_handle, t.text, t.created_at
		 FROM tweets t JOIN users u ON u.id = t.author_id
		 WHERE t.author_id = ? AND t.id < ?
		 ORDER BY t.id DESC LIMIT ?`,
		authorID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("user tweets: %w", err)
	}
	return toTweets(rows)
}

// CelebrityTimeline returns ids of recent tweets (id < beforeID, newest first)
// from the followees of followerID whose follower_count exceeds threshold. These
// authors are skipped by write-time fan-out, so they are pulled and merged at
// read time.
func (s *SQLite) CelebrityTimeline(ctx context.Context, followerID, threshold, beforeID int64, limit int) ([]int64, error) {
	return s.timelineByCelebrity(ctx, followerID, threshold, beforeID, limit, ">")
}

// NonCelebrityTimeline is the complement of CelebrityTimeline: recent tweet ids
// from the followees whose follower_count is within threshold — i.e. exactly the
// authors that write-time fan-out materializes. A rebuild uses it to reconstruct
// a user's materialized timeline from the source of truth.
func (s *SQLite) NonCelebrityTimeline(ctx context.Context, followerID, threshold, beforeID int64, limit int) ([]int64, error) {
	return s.timelineByCelebrity(ctx, followerID, threshold, beforeID, limit, "<=")
}

// timelineByCelebrity pulls followees' recent tweets, partitioning by the
// celebrity threshold with the given comparator (">" for celebrities, "<=" for
// the rest). The comparator is a fixed literal, never user input.
func (s *SQLite) timelineByCelebrity(ctx context.Context, followerID, threshold, beforeID int64, limit int, cmp string) ([]int64, error) {
	beforeID = normalizeCursor(beforeID)
	var ids []int64
	query := fmt.Sprintf(
		`SELECT t.id
		 FROM follows f
		 JOIN users u ON u.id = f.followee_id
		 JOIN tweets t ON t.author_id = f.followee_id
		 WHERE f.follower_id = ? AND u.follower_count %s ? AND t.id < ?
		 ORDER BY t.id DESC LIMIT ?`, cmp)
	if err := s.db.SelectContext(ctx, &ids, query, followerID, threshold, beforeID, limit); err != nil {
		return nil, fmt.Errorf("timeline by celebrity (%s): %w", cmp, err)
	}
	return ids, nil
}

// GetTweetsByIDs hydrates tweet bodies for ids, preserving that order and
// dropping ids that no longer exist. It serves hits from the cache and reads
// only the misses from SQLite, warming the cache with what it fetched.
func (s *SQLite) GetTweetsByIDs(ctx context.Context, ids []int64) ([]Tweet, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := s.cache.GetMany(ctx, ids)
	if err != nil {
		slog.Warn("tweet cache get failed", "err", err)
		found = map[int64]Tweet{}
	}

	var missing []int64
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fetched, err := s.tweetsFromDB(ctx, missing)
		if err != nil {
			return nil, err
		}
		if len(fetched) > 0 {
			if err := s.cache.SetMany(ctx, fetched); err != nil {
				slog.Warn("tweet cache set failed", "err", err)
			}
		}
		for id, t := range fetched {
			found[id] = t
		}
	}

	out := make([]Tweet, 0, len(ids))
	for _, id := range ids {
		if t, ok := found[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// tweetsFromDB reads the given tweets from SQLite into a map keyed by id.
func (s *SQLite) tweetsFromDB(ctx context.Context, ids []int64) (map[int64]Tweet, error) {
	query, args, err := sqlx.In(
		`SELECT t.id, t.author_id, u.handle AS author_handle, t.text, t.created_at
		 FROM tweets t JOIN users u ON u.id = t.author_id
		 WHERE t.id IN (?)`, ids)
	if err != nil {
		return nil, fmt.Errorf("build in-query: %w", err)
	}
	var rows []tweetRow
	if err := s.db.SelectContext(ctx, &rows, s.db.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("get tweets by ids: %w", err)
	}
	tweets, err := toTweets(rows)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]Tweet, len(tweets))
	for _, t := range tweets {
		m[t.ID] = t
	}
	return m, nil
}

// --- row mapping ---------------------------------------------------------

type userRow struct {
	ID            int64  `db:"id"`
	Handle        string `db:"handle"`
	DisplayName   string `db:"display_name"`
	FollowerCount int64  `db:"follower_count"`
	CreatedAt     string `db:"created_at"`
}

func (r userRow) toUser() (User, error) {
	t, err := time.Parse(timeLayout, r.CreatedAt)
	if err != nil {
		return User{}, fmt.Errorf("parse created_at: %w", err)
	}
	return User{
		ID:            r.ID,
		Handle:        r.Handle,
		DisplayName:   r.DisplayName,
		FollowerCount: r.FollowerCount,
		CreatedAt:     t,
	}, nil
}

type tweetRow struct {
	ID           int64  `db:"id"`
	AuthorID     int64  `db:"author_id"`
	AuthorHandle string `db:"author_handle"`
	Text         string `db:"text"`
	CreatedAt    string `db:"created_at"`
}

func toTweets(rows []tweetRow) ([]Tweet, error) {
	out := make([]Tweet, len(rows))
	for i, r := range rows {
		t, err := time.Parse(timeLayout, r.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
		out[i] = Tweet{
			ID:           r.ID,
			AuthorID:     r.AuthorID,
			AuthorHandle: r.AuthorHandle,
			Text:         r.Text,
			CreatedAt:    t,
		}
	}
	return out, nil
}

// normalizeCursor maps a "no cursor" sentinel (<= 0) to the maximum id so that
// "id < cursor" returns the newest tweets.
func normalizeCursor(beforeID int64) int64 {
	if beforeID <= 0 {
		return math.MaxInt64
	}
	return beforeID
}

// isConstraint reports whether err is a modernc SQLite constraint violation of
// the given extended code.
func isConstraint(err error, code int) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == code
}
