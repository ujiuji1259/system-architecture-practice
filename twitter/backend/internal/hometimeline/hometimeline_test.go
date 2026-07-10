package hometimeline

import (
	"context"
	"sort"
	"testing"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
)

// fakeRepo serves a fixed follow graph, tweet-author map, and per-user
// non-celebrity timeline.
type fakeRepo struct {
	authors   map[int64]repository.TweetAuthor // tweetID -> author
	followers map[int64][]int64                // authorID -> sorted follower ids
	nonCeleb  map[int64][]int64                // followerID -> materializable tweet ids
	celeb     map[int64][]int64                // followerID -> celebrity-followee tweet ids
	pages     int                              // FollowersPage call count
}

func (r *fakeRepo) GetTweetAuthor(_ context.Context, tweetID int64) (repository.TweetAuthor, error) {
	a, ok := r.authors[tweetID]
	if !ok {
		return repository.TweetAuthor{}, repository.ErrNotFound
	}
	return a, nil
}

func (r *fakeRepo) FollowersPage(_ context.Context, followeeID, afterID int64, limit int) ([]int64, error) {
	r.pages++
	all := r.followers[followeeID] // assumed sorted ascending
	i := sort.Search(len(all), func(i int) bool { return all[i] > afterID })
	end := i + limit
	if end > len(all) {
		end = len(all)
	}
	return append([]int64(nil), all[i:end]...), nil
}

func (r *fakeRepo) NonCelebrityTimeline(_ context.Context, followerID, _, _ int64, limit int) ([]int64, error) {
	return page(r.nonCeleb[followerID], limit), nil
}

func (r *fakeRepo) CelebrityTimeline(_ context.Context, followerID, _, _ int64, limit int) ([]int64, error) {
	return page(r.celeb[followerID], limit), nil
}

func page(ids []int64, limit int) []int64 {
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return append([]int64(nil), ids...)
}

// fakeStore records what was written to each user's timeline and serves a
// settable materialized content per user for Range.
type fakeStore struct {
	pushed  map[int64][]int64
	filled  map[int64][]int64
	content map[int64][]int64 // what Range returns (descending)
}

func newFakeStore() *fakeStore {
	return &fakeStore{pushed: map[int64][]int64{}, filled: map[int64][]int64{}, content: map[int64][]int64{}}
}

func (s *fakeStore) PushMany(_ context.Context, userIDs []int64, tweetID int64) error {
	for _, u := range userIDs {
		s.pushed[u] = append(s.pushed[u], tweetID)
	}
	return nil
}

func (s *fakeStore) Fill(_ context.Context, userID int64, tweetIDs []int64) error {
	s.filled[userID] = append([]int64(nil), tweetIDs...)
	s.content[userID] = append([]int64(nil), tweetIDs...) // a rebuild makes them readable
	return nil
}

func (s *fakeStore) Range(_ context.Context, userID, _ int64, limit int) ([]int64, error) {
	return page(s.content[userID], limit), nil
}

func newProjection(repo Repo, store Store, threshold int64, pageSize int) *Projection {
	return New(repo, store, Config{Policy: CelebrityPolicy{Threshold: threshold}, PageSize: pageSize, MaxLen: 800})
}

func TestApplyFansOutToFollowers(t *testing.T) {
	repo := &fakeRepo{
		authors:   map[int64]repository.TweetAuthor{100: {AuthorID: 1, FollowerCount: 3}},
		followers: map[int64][]int64{1: {10, 20, 30}},
	}
	store := newFakeStore()
	p := newProjection(repo, store, 1000, 2)

	if err := p.Apply(context.Background(), events.TweetPosted{TweetID: 100, AuthorID: 1}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, u := range []int64{10, 20, 30} {
		if got := store.pushed[u]; len(got) != 1 || got[0] != 100 {
			t.Errorf("user %d pushed = %v, want [100]", u, got)
		}
	}
	if m := p.Metrics(); m.Fanned != 1 || m.Skipped != 0 || m.Edges != 3 {
		t.Errorf("metrics = %+v, want fanned=1 skipped=0 edges=3", m)
	}
	if repo.pages != 2 { // 3 followers, page size 2 -> pages of 2 and 1
		t.Errorf("FollowersPage calls = %d, want 2 (paginated)", repo.pages)
	}
}

func TestApplySkipsCelebrity(t *testing.T) {
	repo := &fakeRepo{
		authors:   map[int64]repository.TweetAuthor{100: {AuthorID: 1, FollowerCount: 5000}},
		followers: map[int64][]int64{1: {10, 20, 30}},
	}
	store := newFakeStore()
	p := newProjection(repo, store, 1000, 2)

	if err := p.Apply(context.Background(), events.TweetPosted{TweetID: 100, AuthorID: 1}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(store.pushed) != 0 {
		t.Errorf("celebrity fan-out pushed %v, want nothing (pulled at read time)", store.pushed)
	}
	if m := p.Metrics(); m.Skipped != 1 || m.Fanned != 0 || m.Edges != 0 {
		t.Errorf("metrics = %+v, want skipped=1 fanned=0 edges=0", m)
	}
	if repo.pages != 0 {
		t.Errorf("FollowersPage calls = %d, want 0 for a celebrity", repo.pages)
	}
}

func TestApplyMissingTweetIsNoOp(t *testing.T) {
	repo := &fakeRepo{authors: map[int64]repository.TweetAuthor{}}
	store := newFakeStore()
	p := newProjection(repo, store, 1000, 100)

	if err := p.Apply(context.Background(), events.TweetPosted{TweetID: 404}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if m := p.Metrics(); m.Fanned != 0 || m.Skipped != 0 {
		t.Errorf("metrics = %+v, want all zero", m)
	}
}

func TestTimelineIDsMergesMaterializedAndCelebrityPull(t *testing.T) {
	repo := &fakeRepo{celeb: map[int64][]int64{1: {40, 30, 20}}} // celebrity pull
	store := newFakeStore()
	store.content[1] = []int64{50, 30, 10} // materialized
	p := newProjection(repo, store, 1000, 100)

	ids, err := p.TimelineIDs(context.Background(), 1, 0, 4)
	if err != nil {
		t.Fatalf("TimelineIDs: %v", err)
	}
	// {50,30,10} ∪ {40,30,20}, deduped descending, capped at 4.
	want := []int64{50, 40, 30, 20}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestTimelineIDsRebuildsOnEmptyFirstPage(t *testing.T) {
	// Materialized is empty; the source of truth has non-celebrity tweets. A
	// first-page read must rebuild, then serve them.
	repo := &fakeRepo{nonCeleb: map[int64][]int64{1: {9, 7}}}
	store := newFakeStore() // content[1] empty
	p := newProjection(repo, store, 1000, 100)

	ids, err := p.TimelineIDs(context.Background(), 1, 0, 20)
	if err != nil {
		t.Fatalf("TimelineIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != 9 || ids[1] != 7 {
		t.Fatalf("ids = %v, want [9 7] after rebuild", ids)
	}
	if m := p.Metrics(); m.Rebuilt != 1 {
		t.Errorf("rebuilt = %d, want 1", m.Rebuilt)
	}
}

func TestTimelineIDsDoesNotRebuildOnDeepPage(t *testing.T) {
	repo := &fakeRepo{nonCeleb: map[int64][]int64{1: {9, 7}}}
	store := newFakeStore() // content empty
	p := newProjection(repo, store, 1000, 100)

	// A cursor (before_id > 0) means paging; an empty page is the end, not a miss.
	if _, err := p.TimelineIDs(context.Background(), 1, 100, 20); err != nil {
		t.Fatalf("TimelineIDs: %v", err)
	}
	if m := p.Metrics(); m.Rebuilt != 0 {
		t.Errorf("rebuilt = %d, want 0 on a deep page", m.Rebuilt)
	}
}

func TestRebuildFillsFromNonCelebrityFollowees(t *testing.T) {
	repo := &fakeRepo{nonCeleb: map[int64][]int64{7: {50, 40, 30}}}
	store := newFakeStore()
	p := newProjection(repo, store, 1000, 100)

	if err := p.Rebuild(context.Background(), 7); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got := store.filled[7]; len(got) != 3 || got[0] != 50 || got[2] != 30 {
		t.Fatalf("filled = %v, want [50 40 30] from the source of truth", got)
	}
	if m := p.Metrics(); m.Rebuilt != 1 {
		t.Errorf("rebuilt = %d, want 1", m.Rebuilt)
	}
}
