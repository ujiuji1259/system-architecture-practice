package service

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/hometimeline"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/hometimeline/store"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/snowflake"
)

// BenchmarkHomeTimelineRead contrasts the two ways to build a home timeline as
// the number of followees grows:
//
//   - hybrid: read the materialized timeline (O(page)) — flat regardless of how
//     many accounts the user follows.
//   - pull:   query each followee's recent tweets and merge (O(followees)) —
//     grows linearly, the design this whole exercise avoids.
func BenchmarkHomeTimelineRead(b *testing.B) {
	const tweetsPer = 20
	for _, followees := range []int{10, 100, 1000} {
		repo, tl, me, followeeIDs := seedHome(b, followees, tweetsPer)
		// Threshold high enough that nobody is a celebrity here: we are measuring
		// the materialized read against the pull baseline directly.
		projection := hometimeline.New(repo, tl, hometimeline.Config{
			Policy: hometimeline.CelebrityPolicy{Threshold: 1 << 62},
			MaxLen: 800,
		})
		svc := New(repo, projection, noopPublisher{}, snowflake.New(1))
		ctx := context.Background()

		b.Run(fmt.Sprintf("hybrid/followees=%d", followees), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := svc.HomeTimeline(ctx, me, 0, 20); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("pull/followees=%d", followees), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				pullHome(b, repo, followeeIDs, 20)
			}
		})
	}
}

// seedHome builds a repository with `followees` authors (each posting
// tweetsPer tweets), a user `me` who follows them all, and a materialized
// timeline already fanned out to `me`.
func seedHome(b *testing.B, followees, tweetsPer int) (*repository.SQLite, *store.Memory, int64, []int64) {
	b.Helper()
	ctx := context.Background()
	dsn := filepath.Join(b.TempDir(), "bench.db")
	repo, err := repository.New(ctx, dsn, repository.NewMemoryCache())
	if err != nil {
		b.Fatalf("repository.New: %v", err)
	}
	b.Cleanup(func() { _ = repo.Close() })

	tl := store.NewMemory(800)
	gen := snowflake.New(1)

	const me = int64(1)
	if err := repo.CreateUser(ctx, repository.User{ID: me, Handle: "me", CreatedAt: time.Now()}); err != nil {
		b.Fatal(err)
	}

	ids := make([]int64, followees)
	for i := 0; i < followees; i++ {
		uid := int64(i + 2)
		ids[i] = uid
		if err := repo.CreateUser(ctx, repository.User{ID: uid, Handle: fmt.Sprintf("u%d", uid), CreatedAt: time.Now()}); err != nil {
			b.Fatal(err)
		}
		if err := repo.Follow(ctx, me, uid); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < tweetsPer; j++ {
			tid := gen.Next()
			if err := repo.CreateTweet(ctx, repository.Tweet{ID: tid, AuthorID: uid, Text: "t", CreatedAt: time.Now()}); err != nil {
				b.Fatal(err)
			}
			// Simulate fan-out on write into me's materialized timeline.
			if err := tl.Push(ctx, me, tid); err != nil {
				b.Fatal(err)
			}
		}
	}
	return repo, tl, me, ids
}

// BenchmarkHomeTimelineReadByCelebrityRatio fixes the number of followees and
// varies how many of them are celebrities. At 0% every followee is materialized
// (an O(page) read); at 100% every followee is pulled at read time (an
// O(celebrity-followees) read). It shows how the hybrid read degrades from
// materialized-fast toward pull-slow as a user follows more celebrities.
func BenchmarkHomeTimelineReadByCelebrityRatio(b *testing.B) {
	const (
		followees = 500
		tweetsPer = 20
		threshold = 1 // a followee is a celebrity once it has >1 follower
	)
	for _, celebPct := range []int{0, 25, 50, 100} {
		repo, tl, me := seedByRatio(b, followees, tweetsPer, celebPct, threshold)
		projection := hometimeline.New(repo, tl, hometimeline.Config{
			Policy: hometimeline.CelebrityPolicy{Threshold: threshold},
			MaxLen: 800,
		})
		svc := New(repo, projection, noopPublisher{}, snowflake.New(1))
		ctx := context.Background()

		b.Run(fmt.Sprintf("celebrities=%d%%", celebPct), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := svc.HomeTimeline(ctx, me, 0, 20); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// seedByRatio builds a graph where the first celebPct% of followees are
// celebrities (materialized nowhere, pulled at read) and the rest are
// materialized into me's timeline. Celebrities are pushed over the threshold by
// a second follower (a dummy user), since me's own follow only counts as one.
func seedByRatio(b *testing.B, followees, tweetsPer, celebPct int, threshold int64) (*repository.SQLite, *store.Memory, int64) {
	b.Helper()
	ctx := context.Background()
	dsn := filepath.Join(b.TempDir(), "ratio.db")
	repo, err := repository.New(ctx, dsn, repository.NewMemoryCache())
	if err != nil {
		b.Fatalf("repository.New: %v", err)
	}
	b.Cleanup(func() { _ = repo.Close() })

	tl := store.NewMemory(800)
	gen := snowflake.New(1)

	const me, dummy = int64(1), int64(2)
	for _, u := range []struct {
		id     int64
		handle string
	}{{me, "me"}, {dummy, "dummy"}} {
		if err := repo.CreateUser(ctx, repository.User{ID: u.id, Handle: u.handle, CreatedAt: time.Now()}); err != nil {
			b.Fatal(err)
		}
	}
	// A sentinel keeps me's materialized timeline non-empty even at 100%
	// celebrities, so we measure the read (materialized + pull) and not a
	// rebuild-on-miss.
	if err := tl.Push(ctx, me, 1); err != nil {
		b.Fatal(err)
	}

	celebCount := followees * celebPct / 100
	for i := 0; i < followees; i++ {
		uid := int64(i + 3)
		celebrity := i < celebCount
		fc := "u"
		if celebrity {
			fc = "c"
		}
		if err := repo.CreateUser(ctx, repository.User{ID: uid, Handle: fmt.Sprintf("%s%d", fc, uid), CreatedAt: time.Now()}); err != nil {
			b.Fatal(err)
		}
		if err := repo.Follow(ctx, me, uid); err != nil {
			b.Fatal(err)
		}
		if celebrity {
			if err := repo.Follow(ctx, dummy, uid); err != nil { // pushes follower_count over threshold
				b.Fatal(err)
			}
		}
		for j := 0; j < tweetsPer; j++ {
			tid := gen.Next()
			if err := repo.CreateTweet(ctx, repository.Tweet{ID: tid, AuthorID: uid, Text: "t", CreatedAt: time.Now()}); err != nil {
				b.Fatal(err)
			}
			if !celebrity { // only non-celebrities are materialized (fanned out)
				if err := tl.Push(ctx, me, tid); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	return repo, tl, me
}

// pullHome is the O(followees) baseline: query each followee's newest tweets and
// merge them, keeping the newest `limit`.
func pullHome(b *testing.B, repo *repository.SQLite, followees []int64, limit int) {
	ctx := context.Background()
	var all []int64
	for _, f := range followees {
		ts, err := repo.UserTweets(ctx, f, 0, limit)
		if err != nil {
			b.Fatal(err)
		}
		for _, t := range ts {
			all = append(all, t.ID)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] > all[j] })
	if len(all) > limit {
		all = all[:limit]
	}
	if _, err := repo.GetTweetsByIDs(ctx, all); err != nil {
		b.Fatal(err)
	}
}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, events.TweetPosted) error { return nil }
