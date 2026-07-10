// Package events carries domain events between the command side and the
// projectors that maintain read models.
//
// The command use case (posting a tweet) only records that something happened —
// it publishes a TweetPosted fact and returns. It does not know who consumes the
// event or how many read models exist. Projectors (e.g. the fan-out worker that
// maintains home timelines) subscribe and react. Adding a new read model —
// search, notifications, analytics — means adding a subscriber, not changing the
// command.
package events

import "context"

// TweetPosted is emitted after a tweet is durably stored. It carries the
// immutable facts of the event; consumers re-read any mutable state (such as the
// author's current follower count) themselves.
type TweetPosted struct {
	TweetID  int64 `json:"tweet_id"`
	AuthorID int64 `json:"author_id"`
}

// MemoryBus is an in-process event bus (a buffered channel) used when no Redis
// is configured and in tests. Events still buffered at shutdown are dropped; for
// durability use RedisBus (and, ultimately, a transactional outbox).
type MemoryBus struct {
	ch chan TweetPosted
}

// NewMemoryBus returns a MemoryBus with the given buffer size.
func NewMemoryBus(buffer int) *MemoryBus {
	if buffer <= 0 {
		buffer = 1024
	}
	return &MemoryBus{ch: make(chan TweetPosted, buffer)}
}

// Publish sends an event, blocking if the buffer is full (until ctx is done).
func (b *MemoryBus) Publish(ctx context.Context, e TweetPosted) error {
	select {
	case b.ch <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Poll returns the next event, or ok=false when ctx is done.
func (b *MemoryBus) Poll(ctx context.Context) (TweetPosted, bool, error) {
	select {
	case e := <-b.ch:
		return e, true, nil
	case <-ctx.Done():
		return TweetPosted{}, false, nil
	}
}

// Len reports the number of buffered events (useful in tests).
func (b *MemoryBus) Len() int { return len(b.ch) }
