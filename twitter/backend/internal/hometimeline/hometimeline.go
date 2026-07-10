// Package hometimeline owns the maintenance of the home-timeline projection:
// what belongs in a user's materialized timeline and how it is kept up to date.
//
// It gathers every "how the timeline is updated" concern in one place, so a
// change to the timeline's definition (the celebrity cut, what gets fanned out,
// how a stale timeline is rebuilt) is a change to this package alone:
//
//   - Apply   — incremental maintenance: fan a single new tweet out to the
//     author's followers (the write-time projector step).
//   - Rebuild — bulk maintenance: reconstruct a user's timeline from the source
//     of truth (used on a read miss / after eviction / to heal a lost event).
//
// Both obey the same CelebrityPolicy, so the write path and the rebuild path can
// never disagree about which authors are materialized. It sits on top of the
// dumb timeline store (which knows only how to hold ids) and reads the source of
// truth through the Repo port. It does not run itself — the fanout package
// drives Apply, and the read path triggers Rebuild.
package hometimeline

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync/atomic"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
)

// CelebrityPolicy is the single source of truth for who is fanned out on write
// versus pulled at read time. It is shared by the write path (Apply), the
// rebuild path, and the read-time celebrity pull, so all three partition
// followees at exactly the same cut point.
type CelebrityPolicy struct {
	// Threshold: authors with strictly more followers than this are celebrities.
	Threshold int64
}

// IsCelebrity reports whether an author with the given follower count is a
// celebrity (skipped by fan-out, pulled at read time).
func (p CelebrityPolicy) IsCelebrity(followerCount int64) bool {
	return followerCount > p.Threshold
}

// Repo is the source-of-truth access the projection needs.
type Repo interface {
	GetTweetAuthor(ctx context.Context, tweetID int64) (repository.TweetAuthor, error)
	FollowersPage(ctx context.Context, followeeID, afterID int64, limit int) ([]int64, error)
	// NonCelebrityTimeline returns recent tweet ids from followerID's
	// non-celebrity followees — exactly what the materialized timeline holds.
	NonCelebrityTimeline(ctx context.Context, followerID, threshold, beforeID int64, limit int) ([]int64, error)
	// CelebrityTimeline returns recent tweet ids from followerID's celebrity
	// followees — the ones deliberately not materialized, pulled at read time.
	CelebrityTimeline(ctx context.Context, followerID, threshold, beforeID int64, limit int) ([]int64, error)
}

// Store is the materialized-timeline store the projection reads and writes.
type Store interface {
	PushMany(ctx context.Context, userIDs []int64, tweetID int64) error
	Fill(ctx context.Context, userID int64, tweetIDs []int64) error
	Range(ctx context.Context, userID, beforeID int64, limit int) ([]int64, error)
}

// Metrics expose what the projection did, so the celebrity split, write
// amplification, and rebuild rate are observable.
type Metrics struct {
	Fanned  atomic.Int64 // tweets fanned out on write
	Skipped atomic.Int64 // tweets skipped (celebrity, pulled at read)
	Edges   atomic.Int64 // total per-follower timeline writes
	Rebuilt atomic.Int64 // timelines rebuilt from the source of truth
}

// Snapshot is a plain-value copy of the counters.
type Snapshot struct {
	Fanned, Skipped, Edges, Rebuilt int64
}

// Config tunes the projection.
type Config struct {
	Policy CelebrityPolicy
	// PageSize is how many followers are loaded (and pushed) per fan-out batch.
	PageSize int
	// MaxLen bounds how many ids a rebuild materializes (matches the store cap).
	MaxLen int
}

// Projection maintains the home-timeline projection. It is stateless apart from
// its counters, so Apply may be called concurrently from many workers.
type Projection struct {
	repo    Repo
	store   Store
	cfg     Config
	metrics Metrics
}

// New builds a Projection.
func New(repo Repo, store Store, cfg Config) *Projection {
	if cfg.PageSize <= 0 {
		cfg.PageSize = 1000
	}
	if cfg.MaxLen <= 0 {
		cfg.MaxLen = 800
	}
	return &Projection{repo: repo, store: store, cfg: cfg}
}

// Metrics returns a snapshot of the counters.
func (p *Projection) Metrics() Snapshot {
	return Snapshot{
		Fanned:  p.metrics.Fanned.Load(),
		Skipped: p.metrics.Skipped.Load(),
		Edges:   p.metrics.Edges.Load(),
		Rebuilt: p.metrics.Rebuilt.Load(),
	}
}

// Apply maintains the projection for one new tweet: it fans the tweet out to the
// author's followers, unless the author is a celebrity (skipped here and pulled
// at read time instead).
func (p *Projection) Apply(ctx context.Context, e events.TweetPosted) error {
	author, err := p.repo.GetTweetAuthor(ctx, e.TweetID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil // tweet gone (e.g. author deleted); nothing to do
	}
	if err != nil {
		return err
	}

	if p.cfg.Policy.IsCelebrity(author.FollowerCount) {
		p.metrics.Skipped.Add(1)
		slog.Debug("hometimeline skip (celebrity)", "author_id", author.AuthorID,
			"followers", author.FollowerCount, "tweet_id", e.TweetID)
		return nil
	}

	var after int64
	for {
		followers, err := p.repo.FollowersPage(ctx, author.AuthorID, after, p.cfg.PageSize)
		if err != nil {
			return err
		}
		if len(followers) == 0 {
			break
		}
		if err := p.store.PushMany(ctx, followers, e.TweetID); err != nil {
			return err
		}
		p.metrics.Edges.Add(int64(len(followers)))
		after = followers[len(followers)-1]
		if len(followers) < p.cfg.PageSize {
			break
		}
	}
	p.metrics.Fanned.Add(1)
	return nil
}

// TimelineIDs returns the ids of the tweets in userID's home timeline from the
// accounts they follow — the projection's content. It reads the materialized
// (non-celebrity) part from the store and pulls the celebrity part at read time,
// then merges them. This is where all three faces of the celebrity policy meet:
// materialize on write (Apply), pull on read (here), reconstruct on rebuild.
//
// On the first page an empty materialized timeline is treated as a miss (never
// built, evicted, or a lost event) and rebuilt from the source of truth first.
// A deeper empty page is just the end of the timeline, so it is not rebuilt.
func (p *Projection) TimelineIDs(ctx context.Context, userID, beforeID int64, limit int) ([]int64, error) {
	materialized, err := p.store.Range(ctx, userID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	if len(materialized) == 0 && beforeID == 0 {
		if err := p.Rebuild(ctx, userID); err != nil {
			return nil, err
		}
		if materialized, err = p.store.Range(ctx, userID, beforeID, limit); err != nil {
			return nil, err
		}
	}

	celebrities, err := p.repo.CelebrityTimeline(ctx, userID, p.cfg.Policy.Threshold, beforeID, limit)
	if err != nil {
		return nil, err
	}

	ids := mergeDescDedup(materialized, celebrities)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// mergeDescDedup merges id lists that are each sorted descending into one
// descending list with duplicates removed.
func mergeDescDedup(lists ...[]int64) []int64 {
	var all []int64
	for _, l := range lists {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] > all[j] })

	out := all[:0] // dedup in place: writes never outrun reads
	var last int64 = -1
	for _, v := range all {
		if v == last {
			continue
		}
		out = append(out, v)
		last = v
	}
	return out
}

// Rebuild reconstructs userID's materialized timeline from the source of truth:
// the newest tweets from their non-celebrity followees, written into the store.
// It is the inverse of Apply (forward, per user) and obeys the same policy, so
// the rebuilt timeline matches what fan-out would have produced.
func (p *Projection) Rebuild(ctx context.Context, userID int64) error {
	ids, err := p.repo.NonCelebrityTimeline(ctx, userID, p.cfg.Policy.Threshold, 0, p.cfg.MaxLen)
	if err != nil {
		return err
	}
	if err := p.store.Fill(ctx, userID, ids); err != nil {
		return err
	}
	p.metrics.Rebuilt.Add(1)
	return nil
}
