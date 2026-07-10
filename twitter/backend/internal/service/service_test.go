package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/snowflake"
)

// --- fakes ---------------------------------------------------------------

type fakeRepo struct {
	users      map[int64]repository.User
	created    []repository.Tweet
	handleTake bool    // next CreateUser returns ErrHandleExists
	own        []int64 // ids returned by UserTweets (read-your-writes)
}

func (r *fakeRepo) CreateUser(_ context.Context, u repository.User) error {
	if r.handleTake {
		return repository.ErrHandleExists
	}
	r.users[u.ID] = u
	return nil
}
func (r *fakeRepo) GetUser(_ context.Context, id int64) (repository.User, error) {
	u, ok := r.users[id]
	if !ok {
		return repository.User{}, repository.ErrNotFound
	}
	return u, nil
}
func (r *fakeRepo) Follow(_ context.Context, _, _ int64) error   { return nil }
func (r *fakeRepo) Unfollow(_ context.Context, _, _ int64) error { return nil }
func (r *fakeRepo) CreateTweet(_ context.Context, t repository.Tweet) error {
	r.created = append(r.created, t)
	return nil
}
func (r *fakeRepo) UserTweets(_ context.Context, _, _ int64, _ int) ([]repository.Tweet, error) {
	out := make([]repository.Tweet, len(r.own))
	for i, id := range r.own {
		out[i] = repository.Tweet{ID: id}
	}
	return out, nil
}
func (r *fakeRepo) GetTweetsByIDs(_ context.Context, ids []int64) ([]repository.Tweet, error) {
	out := make([]repository.Tweet, len(ids))
	for i, id := range ids {
		out[i] = repository.Tweet{ID: id, AuthorID: 1, AuthorHandle: "a", Text: "t"}
	}
	return out, nil
}

// fakeHome stands in for the projection: it returns the ids of tweets from the
// accounts the user follows. All timeline-projection concerns live behind it.
type fakeHome struct{ ids []int64 }

func (h *fakeHome) TimelineIDs(_ context.Context, _, _ int64, _ int) ([]int64, error) {
	return h.ids, nil
}

type fakePublisher struct{ published []events.TweetPosted }

func (p *fakePublisher) Publish(_ context.Context, e events.TweetPosted) error {
	p.published = append(p.published, e)
	return nil
}

func newService(repo *fakeRepo, home *fakeHome, p *fakePublisher) *Service {
	return New(repo, home, p, snowflake.New(1))
}

// --- tests ---------------------------------------------------------------

func TestCreateUserValidatesHandle(t *testing.T) {
	svc := newService(&fakeRepo{users: map[int64]repository.User{}}, &fakeHome{}, &fakePublisher{})
	for _, bad := range []string{"", "with space", "waytoolonghandle_x", "bad!"} {
		if _, err := svc.CreateUser(context.Background(), bad, ""); !errors.Is(err, ErrInvalidHandle) {
			t.Errorf("CreateUser(%q) err = %v, want ErrInvalidHandle", bad, err)
		}
	}
}

func TestCreateUserHandleTaken(t *testing.T) {
	svc := newService(&fakeRepo{users: map[int64]repository.User{}, handleTake: true}, &fakeHome{}, &fakePublisher{})
	if _, err := svc.CreateUser(context.Background(), "alice", ""); !errors.Is(err, ErrHandleTaken) {
		t.Fatalf("err = %v, want ErrHandleTaken", err)
	}
}

func TestFollowRejectsSelf(t *testing.T) {
	svc := newService(&fakeRepo{users: map[int64]repository.User{}}, &fakeHome{}, &fakePublisher{})
	if err := svc.Follow(context.Background(), 7, 7); !errors.Is(err, ErrSelfFollow) {
		t.Fatalf("err = %v, want ErrSelfFollow", err)
	}
}

func TestPostTweetPersistsAndPublishes(t *testing.T) {
	repo := &fakeRepo{users: map[int64]repository.User{1: {ID: 1, Handle: "alice"}}}
	pub := &fakePublisher{}
	svc := newService(repo, &fakeHome{}, pub)

	tw, err := svc.PostTweet(context.Background(), 1, "  hello  ")
	if err != nil {
		t.Fatalf("PostTweet: %v", err)
	}
	if tw.AuthorHandle != "alice" || tw.Text != "hello" {
		t.Errorf("tweet = %+v, want handle=alice text=hello (trimmed)", tw)
	}
	// The tweet is persisted...
	if len(repo.created) != 1 || repo.created[0].ID != tw.ID {
		t.Errorf("created = %v, want the tweet persisted", repo.created)
	}
	// ...and a TweetPosted fact is published (no direct timeline write).
	if len(pub.published) != 1 || pub.published[0] != (events.TweetPosted{TweetID: tw.ID, AuthorID: 1}) {
		t.Errorf("published = %v, want one TweetPosted{%d, 1}", pub.published, tw.ID)
	}
}

func TestHomeTimelineIncludesOwnTweetsReadYourWrites(t *testing.T) {
	// Nothing materialized and no celebrities, but the user's own tweets must
	// still appear immediately, pulled from the source of truth.
	repo := &fakeRepo{users: map[int64]repository.User{1: {ID: 1}}, own: []int64{7, 5}}
	svc := newService(repo, &fakeHome{}, &fakePublisher{})

	tweets, _, err := svc.HomeTimeline(context.Background(), 1, 0, 20)
	if err != nil {
		t.Fatalf("HomeTimeline: %v", err)
	}
	if len(tweets) != 2 || tweets[0].ID != 7 || tweets[1].ID != 5 {
		t.Fatalf("home = %v, want own tweets [7 5]", tweets)
	}
}

func TestPostTweetInvalidText(t *testing.T) {
	repo := &fakeRepo{users: map[int64]repository.User{1: {ID: 1, Handle: "a"}}}
	svc := newService(repo, &fakeHome{}, &fakePublisher{})
	if _, err := svc.PostTweet(context.Background(), 1, "   "); !errors.Is(err, ErrInvalidText) {
		t.Errorf("empty text err = %v, want ErrInvalidText", err)
	}
	long := make([]rune, maxTweetLen+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := svc.PostTweet(context.Background(), 1, string(long)); !errors.Is(err, ErrInvalidText) {
		t.Errorf("long text err = %v, want ErrInvalidText", err)
	}
}

func TestPostTweetMissingAuthor(t *testing.T) {
	svc := newService(&fakeRepo{users: map[int64]repository.User{}}, &fakeHome{}, &fakePublisher{})
	if _, err := svc.PostTweet(context.Background(), 99, "hi"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHomeTimelineUnionsFollowedAndOwn(t *testing.T) {
	// The projection returns tweets from accounts the user follows; the service
	// unions the user's own tweets and caps the page.
	repo := &fakeRepo{users: map[int64]repository.User{1: {ID: 1, Handle: "me"}}, own: []int64{40, 20}}
	home := &fakeHome{ids: []int64{50, 30, 10}}
	svc := newService(repo, home, &fakePublisher{})

	tweets, next, err := svc.HomeTimeline(context.Background(), 1, 0, 4)
	if err != nil {
		t.Fatalf("HomeTimeline: %v", err)
	}
	// {50,30,10} (followed) ∪ {40,20} (own), descending, capped at 4.
	want := []int64{50, 40, 30, 20}
	if len(tweets) != len(want) {
		t.Fatalf("got %d tweets, want %d (%v)", len(tweets), len(want), want)
	}
	for i, id := range want {
		if tweets[i].ID != id {
			t.Errorf("tweet[%d].ID = %d, want %d (order %v)", i, tweets[i].ID, id, want)
		}
	}
	// Full page (4 == limit) -> cursor is the smallest returned id.
	if next != 20 {
		t.Errorf("next cursor = %d, want 20", next)
	}
}

func TestHomeTimelineShortPageHasNoCursor(t *testing.T) {
	repo := &fakeRepo{users: map[int64]repository.User{1: {ID: 1}}}
	home := &fakeHome{ids: []int64{5, 3}}
	svc := newService(repo, home, &fakePublisher{})

	_, next, err := svc.HomeTimeline(context.Background(), 1, 0, 20)
	if err != nil {
		t.Fatalf("HomeTimeline: %v", err)
	}
	if next != 0 {
		t.Errorf("next cursor = %d, want 0 (no more)", next)
	}
}

func TestHomeTimelineMissingUser(t *testing.T) {
	svc := newService(&fakeRepo{users: map[int64]repository.User{}}, &fakeHome{}, &fakePublisher{})
	if _, _, err := svc.HomeTimeline(context.Background(), 42, 0, 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
