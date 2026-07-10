// Package service holds the domain logic of the mini-Twitter, independent of
// HTTP and of the generated API types.
//
// The interesting use case is HomeTimeline, which implements the read half of
// hybrid fan-out: it merges the user's materialized timeline (written by the
// fan-out worker for ordinary authors) with a pull of recent tweets from the
// celebrities they follow. From here, the timeline store and the repository are
// simply two data sources returning tweet ids.
package service

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/snowflake"
)

// Domain errors. The transport layer maps these to status codes.
var (
	ErrInvalidHandle = errors.New("invalid handle")
	ErrInvalidText   = errors.New("invalid tweet text")
	ErrHandleTaken   = errors.New("handle already in use")
	ErrSelfFollow    = errors.New("cannot follow yourself")
	ErrNotFound      = errors.New("not found")
)

// handlePattern mirrors the handle schema in openapi.yaml.
var handlePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

// maxTweetLen mirrors the text maxLength in openapi.yaml.
const maxTweetLen = 280

// Repository is the persistence behavior the service depends on.
type Repository interface {
	CreateUser(ctx context.Context, u repository.User) error
	GetUser(ctx context.Context, id int64) (repository.User, error)
	Follow(ctx context.Context, followerID, followeeID int64) error
	Unfollow(ctx context.Context, followerID, followeeID int64) error
	CreateTweet(ctx context.Context, t repository.Tweet) error
	UserTweets(ctx context.Context, authorID, beforeID int64, limit int) ([]repository.Tweet, error)
	GetTweetsByIDs(ctx context.Context, ids []int64) ([]repository.Tweet, error)
}

// HomeReader returns the ids of the tweets in a user's home timeline from the
// accounts they follow. All the timeline-projection concerns — the materialized
// read, the celebrity pull, and rebuild-on-miss — live behind this one call
// (internal/hometimeline); the service knows none of them.
type HomeReader interface {
	TimelineIDs(ctx context.Context, userID, beforeID int64, limit int) ([]int64, error)
}

// EventPublisher announces domain events. The command depends only on this: it
// states that a tweet was posted and does not know which read models react.
type EventPublisher interface {
	Publish(ctx context.Context, e events.TweetPosted) error
}

// Service implements the mini-Twitter use cases.
type Service struct {
	repo   Repository
	home   HomeReader
	events EventPublisher
	ids    *snowflake.Generator
	now    func() time.Time
}

// New builds a Service.
func New(repo Repository, home HomeReader, publisher EventPublisher, ids *snowflake.Generator) *Service {
	return &Service{repo: repo, home: home, events: publisher, ids: ids, now: time.Now}
}

// CreateUser validates the handle and persists a new user.
func (s *Service) CreateUser(ctx context.Context, handle, displayName string) (repository.User, error) {
	handle = strings.TrimSpace(handle)
	if !handlePattern.MatchString(handle) {
		return repository.User{}, ErrInvalidHandle
	}
	u := repository.User{
		ID:          s.ids.Next(),
		Handle:      handle,
		DisplayName: displayName,
		CreatedAt:   s.now(),
	}
	switch err := s.repo.CreateUser(ctx, u); {
	case errors.Is(err, repository.ErrHandleExists):
		return repository.User{}, ErrHandleTaken
	case err != nil:
		return repository.User{}, err
	}
	return u, nil
}

// GetUser returns a user by id, or ErrNotFound.
func (s *Service) GetUser(ctx context.Context, id int64) (repository.User, error) {
	u, err := s.repo.GetUser(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return repository.User{}, ErrNotFound
	}
	return u, err
}

// Follow makes follower follow followee (idempotent).
func (s *Service) Follow(ctx context.Context, followerID, followeeID int64) error {
	if followerID == followeeID {
		return ErrSelfFollow
	}
	err := s.repo.Follow(ctx, followerID, followeeID)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Unfollow removes the follow edge (idempotent).
func (s *Service) Unfollow(ctx context.Context, followerID, followeeID int64) error {
	err := s.repo.Unfollow(ctx, followerID, followeeID)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// PostTweet stores a tweet and announces that it happened. It does not touch any
// timeline or fan-out itself: it persists the write model and publishes a
// TweetPosted event; the fan-out projector reacts. Returns ErrNotFound if the
// author does not exist.
func (s *Service) PostTweet(ctx context.Context, authorID int64, text string) (repository.Tweet, error) {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > maxTweetLen {
		return repository.Tweet{}, ErrInvalidText
	}

	// Look the author up first: it validates existence with a clear error and
	// gives us the handle to denormalize onto the tweet.
	author, err := s.repo.GetUser(ctx, authorID)
	if errors.Is(err, repository.ErrNotFound) {
		return repository.Tweet{}, ErrNotFound
	}
	if err != nil {
		return repository.Tweet{}, err
	}

	t := repository.Tweet{
		ID:           s.ids.Next(),
		AuthorID:     authorID,
		AuthorHandle: author.Handle,
		Text:         text,
		CreatedAt:    s.now(),
	}
	if err := s.repo.CreateTweet(ctx, t); err != nil {
		return repository.Tweet{}, err
	}

	// The tweet is durable. Announce the fact and return; projectors (the
	// fan-out worker) update read models asynchronously. A production system
	// would publish via a transactional outbox so the event cannot be lost
	// after the commit.
	if err := s.events.Publish(ctx, events.TweetPosted{TweetID: t.ID, AuthorID: authorID}); err != nil {
		return repository.Tweet{}, err
	}
	return t, nil
}

// HomeTimeline returns a page of the user's home timeline: the projection's
// content (tweets from the accounts they follow) unioned with their own tweets,
// then hydrated. Returns the page and a cursor (0 when there is no more), or
// ErrNotFound if the user does not exist.
func (s *Service) HomeTimeline(ctx context.Context, userID, beforeID int64, limit int) ([]repository.Tweet, int64, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, 0, err
	}

	// Tweets from the accounts the user follows. How that timeline is assembled
	// (materialized read, celebrity pull, rebuild-on-miss) is the projection's
	// concern, not the service's.
	followed, err := s.home.TimelineIDs(ctx, userID, beforeID, limit)
	if err != nil {
		return nil, 0, err
	}
	// Read-your-writes: the user's own tweets come straight from the source of
	// truth, so a just-posted tweet appears immediately — no waiting for the
	// projector, and the command never had to write a read model.
	own, err := s.repo.UserTweets(ctx, userID, beforeID, limit)
	if err != nil {
		return nil, 0, err
	}

	ids := mergeDescDedup(followed, tweetIDs(own))
	if len(ids) > limit {
		ids = ids[:limit]
	}

	tweets, err := s.repo.GetTweetsByIDs(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	return tweets, nextCursor(ids, limit), nil
}

// UserTimeline returns a page of a user's own tweets (pull). Returns the page
// and a cursor (0 when there is no more), or ErrNotFound if the user is absent.
func (s *Service) UserTimeline(ctx context.Context, userID, beforeID int64, limit int) ([]repository.Tweet, int64, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, 0, err
	}
	tweets, err := s.repo.UserTweets(ctx, userID, beforeID, limit)
	if err != nil {
		return nil, 0, err
	}
	return tweets, nextCursor(tweetIDs(tweets), limit), nil
}

// tweetIDs extracts the ids from a slice of tweets, preserving order.
func tweetIDs(tweets []repository.Tweet) []int64 {
	ids := make([]int64, len(tweets))
	for i, t := range tweets {
		ids[i] = t.ID
	}
	return ids
}

// mergeDescDedup merges any number of id lists that are each sorted descending
// into one descending list with duplicates removed.
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

// nextCursor returns the keyset cursor for the next (older) page: the smallest
// id in a full page, or 0 when the page was not full (no more to fetch).
func nextCursor(ids []int64, limit int) int64 {
	if len(ids) < limit || len(ids) == 0 {
		return 0
	}
	return ids[len(ids)-1]
}
