// Package timeline stores materialized home timelines: for each user, a bounded
// list of the newest tweet ids from the accounts they follow (celebrities
// excluded — those are pulled at read time). It is the write-time fan-out target
// and the read-time fast path.
//
// From the service's point of view a Store is just another data source that
// returns tweet ids; whether it is Redis or in-memory is not its concern.
//
// # Why lexicographic ordering, not scores
//
// A natural design is a Redis ZSET scored by tweet id. But ZSET scores are
// float64 (53-bit mantissa) while Snowflake ids are 63-bit, so two ids in the
// same millisecond can collapse to the same score and mis-order. Instead we
// give every member the same score and order by the member itself: a
// zero-padded, fixed-width decimal id, whose lexicographic order equals numeric
// order. Ranges then use ZRANGEBYLEX and stay exact.
package timeline

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// idWidth is the fixed member width: 19 digits holds math.MaxInt64
// (9223372036854775807) so all positive ids zero-pad to the same length and
// sort lexicographically the same as numerically.
const idWidth = 19

// Store is a materialized-timeline store. It is a dumb id container: it knows
// nothing about tweets, the follow graph, or how a timeline is defined — that is
// the projection's concern (see internal/hometimeline).
type Store interface {
	// Push adds tweetID to userID's timeline, trimming to the max length.
	Push(ctx context.Context, userID, tweetID int64) error
	// PushMany adds tweetID to every user's timeline (the fan-out write).
	PushMany(ctx context.Context, userIDs []int64, tweetID int64) error
	// Fill adds many tweet ids to a single user's timeline, trimming to the max
	// length. It is how a rebuild repopulates one timeline from scratch.
	Fill(ctx context.Context, userID int64, tweetIDs []int64) error
	// Range returns up to limit tweet ids from userID's timeline with id <
	// beforeID, newest first. beforeID <= 0 means "from newest".
	Range(ctx context.Context, userID, beforeID int64, limit int) ([]int64, error)
}

func pad(id int64) string {
	return fmt.Sprintf("%0*d", idWidth, id)
}

func unpad(member string) (int64, error) {
	return strconv.ParseInt(strings.TrimLeft(member, "0"), 10, 64)
}

// Memory is an in-process Store used when no Redis is configured and in tests.
type Memory struct {
	mu  sync.Mutex
	max int
	tls map[int64][]int64 // per user, sorted descending
}

// NewMemory returns an in-process Store capping each timeline at maxLen entries.
func NewMemory(maxLen int) *Memory {
	return &Memory{max: maxLen, tls: make(map[int64][]int64)}
}

// Push adds tweetID to userID's timeline.
func (m *Memory) Push(_ context.Context, userID, tweetID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.push(userID, tweetID)
	return nil
}

// PushMany adds tweetID to each user's timeline.
func (m *Memory) PushMany(_ context.Context, userIDs []int64, tweetID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range userIDs {
		m.push(u, tweetID)
	}
	return nil
}

// Fill adds many tweet ids to a single user's timeline.
func (m *Memory) Fill(_ context.Context, userID int64, tweetIDs []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range tweetIDs {
		m.push(userID, id)
	}
	return nil
}

func (m *Memory) push(userID, tweetID int64) {
	ids := m.tls[userID]
	// Insert keeping descending order; ignore an exact duplicate.
	i := sort.Search(len(ids), func(i int) bool { return ids[i] <= tweetID })
	if i < len(ids) && ids[i] == tweetID {
		return
	}
	ids = append(ids, 0)
	copy(ids[i+1:], ids[i:])
	ids[i] = tweetID
	if len(ids) > m.max {
		ids = ids[:m.max]
	}
	m.tls[userID] = ids
}

// Range returns up to limit ids with id < beforeID, newest first.
func (m *Memory) Range(_ context.Context, userID, beforeID int64, limit int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.tls[userID]
	out := make([]int64, 0, limit)
	for _, id := range ids { // already descending
		if beforeID > 0 && id >= beforeID {
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
