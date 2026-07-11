package repository

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// stubCache is a trivial tweetCache used in tests. The real in-memory and Redis
// caches live in internal/tweetcache; importing them here would cycle.
type stubCache struct{ m map[int64]Tweet }

func (c *stubCache) GetMany(_ context.Context, ids []int64) (map[int64]Tweet, error) {
	out := make(map[int64]Tweet, len(ids))
	for _, id := range ids {
		if t, ok := c.m[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

func (c *stubCache) SetMany(_ context.Context, tweets map[int64]Tweet) error {
	for id, t := range tweets {
		c.m[id] = t
	}
	return nil
}

func newRepo(t *testing.T) *SQLite {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	repo, err := New(context.Background(), dsn, &stubCache{m: map[int64]Tweet{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func mkUser(t *testing.T, r *SQLite, id int64, handle string) {
	t.Helper()
	if err := r.CreateUser(context.Background(), User{ID: id, Handle: handle, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateUser(%s): %v", handle, err)
	}
}

func mkTweet(t *testing.T, r *SQLite, id, author int64, text string) {
	t.Helper()
	if err := r.CreateTweet(context.Background(), Tweet{ID: id, AuthorID: author, Text: text, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateTweet(%d): %v", id, err)
	}
}

func TestUserCreateAndGet(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, 1, "alice")

	u, err := r.GetUser(ctx, 1)
	if err != nil || u.Handle != "alice" {
		t.Fatalf("GetUser = (%+v, %v), want alice", u, err)
	}
	if _, err := r.GetUser(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUser(missing) = %v, want ErrNotFound", err)
	}
	// Duplicate handle.
	if err := r.CreateUser(ctx, User{ID: 2, Handle: "alice", CreatedAt: time.Now()}); !errors.Is(err, ErrHandleExists) {
		t.Errorf("duplicate handle = %v, want ErrHandleExists", err)
	}
}

func TestFollowMaintainsCounterAndIsIdempotent(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, 1, "alice")
	mkUser(t, r, 2, "bob")

	if err := r.Follow(ctx, 1, 2); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	// Duplicate follow is a no-op and must not double-count.
	if err := r.Follow(ctx, 1, 2); err != nil {
		t.Fatalf("Follow (dup): %v", err)
	}
	bob, _ := r.GetUser(ctx, 2)
	if bob.FollowerCount != 1 {
		t.Errorf("follower_count = %d, want 1", bob.FollowerCount)
	}

	if err := r.Unfollow(ctx, 1, 2); err != nil {
		t.Fatalf("Unfollow: %v", err)
	}
	// Idempotent unfollow.
	if err := r.Unfollow(ctx, 1, 2); err != nil {
		t.Fatalf("Unfollow (again): %v", err)
	}
	bob, _ = r.GetUser(ctx, 2)
	if bob.FollowerCount != 0 {
		t.Errorf("follower_count after unfollow = %d, want 0", bob.FollowerCount)
	}
}

func TestFollowMissingUser(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, 1, "alice")
	if err := r.Follow(ctx, 1, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Follow missing followee = %v, want ErrNotFound", err)
	}
	if err := r.Unfollow(ctx, 1, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Unfollow missing followee = %v, want ErrNotFound", err)
	}
}

func TestFollowersPageKeyset(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, 100, "celeb")
	for id := int64(1); id <= 5; id++ {
		mkUser(t, r, id, string(rune('a'+id)))
		if err := r.Follow(ctx, id, 100); err != nil {
			t.Fatalf("Follow: %v", err)
		}
	}
	// Walk in pages of 2 by keyset.
	var got []int64
	var after int64
	for {
		page, err := r.FollowersPage(ctx, 100, after, 2)
		if err != nil {
			t.Fatalf("FollowersPage: %v", err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		after = page[len(page)-1]
	}
	want := []int64{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("followers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("followers = %v, want %v", got, want)
		}
	}
}

func TestCreateTweetMissingAuthor(t *testing.T) {
	r := newRepo(t)
	if err := r.CreateTweet(context.Background(), Tweet{ID: 1, AuthorID: 999, Text: "x", CreatedAt: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateTweet missing author = %v, want ErrNotFound", err)
	}
}

func TestUserTweetsOrderAndCursor(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, 1, "alice")
	for _, id := range []int64{10, 20, 30, 40} {
		mkTweet(t, r, id, 1, "t")
	}
	// Newest first.
	all, err := r.UserTweets(ctx, 1, 0, 10)
	if err != nil {
		t.Fatalf("UserTweets: %v", err)
	}
	if len(all) != 4 || all[0].ID != 40 || all[3].ID != 10 {
		t.Fatalf("order = %v, want 40..10", ids(all))
	}
	// Cursor: id < 30.
	page, _ := r.UserTweets(ctx, 1, 30, 10)
	if len(page) != 2 || page[0].ID != 20 {
		t.Fatalf("cursor page = %v, want [20 10]", ids(page))
	}
}

func TestCelebrityTimelineFiltersByThreshold(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, 1, "me")
	mkUser(t, r, 2, "celeb")
	mkUser(t, r, 3, "normal")
	// me follows both; celeb also gets two more followers to exceed threshold 1.
	mkUser(t, r, 4, "f4")
	mkUser(t, r, 5, "f5")
	for _, f := range []int64{1, 4, 5} {
		if err := r.Follow(ctx, f, 2); err != nil { // followers of celeb
			t.Fatalf("Follow celeb: %v", err)
		}
	}
	if err := r.Follow(ctx, 1, 3); err != nil { // me -> normal
		t.Fatalf("Follow normal: %v", err)
	}
	mkTweet(t, r, 200, 2, "celeb tweet")
	mkTweet(t, r, 300, 3, "normal tweet")

	// Threshold 1: celeb has 3 followers (> 1), normal has 1 (not > 1).
	got, err := r.CelebrityTimeline(ctx, 1, 1, 0, 10)
	if err != nil {
		t.Fatalf("CelebrityTimeline: %v", err)
	}
	if len(got) != 1 || got[0] != 200 {
		t.Fatalf("celebrity ids = %v, want [200]", got)
	}
}

func TestGetTweetsByIDsHydratesInOrderAndDropsMissing(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, 1, "alice")
	mkTweet(t, r, 10, 1, "ten")
	mkTweet(t, r, 30, 1, "thirty")

	got, err := r.GetTweetsByIDs(ctx, []int64{30, 20, 10}) // 20 does not exist
	if err != nil {
		t.Fatalf("GetTweetsByIDs: %v", err)
	}
	if len(got) != 2 || got[0].ID != 30 || got[1].ID != 10 {
		t.Fatalf("hydrated = %v, want [30 10] preserving order", ids(got))
	}
	if got[0].AuthorHandle != "alice" {
		t.Errorf("author handle = %q, want alice", got[0].AuthorHandle)
	}
	// Cache is warm now: a repeat returns the same result.
	again, _ := r.GetTweetsByIDs(ctx, []int64{30, 10})
	if len(again) != 2 {
		t.Fatalf("second call = %v, want 2", ids(again))
	}
}

func ids(ts []Tweet) []int64 {
	out := make([]int64, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
