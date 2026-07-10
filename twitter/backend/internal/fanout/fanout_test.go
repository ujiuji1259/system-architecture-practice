package fanout

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
)

// recordingApplier records the events handed to it.
type recordingApplier struct {
	mu      sync.Mutex
	applied []events.TweetPosted
}

func (a *recordingApplier) Apply(_ context.Context, e events.TweetPosted) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applied = append(a.applied, e)
	return nil
}

func (a *recordingApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.applied)
}

// TestPoolDeliversEventsToApplier verifies the runtime's only job: pull events
// off the subscription and hand each to the Applier.
func TestPoolDeliversEventsToApplier(t *testing.T) {
	bus := events.NewMemoryBus(8)
	applier := &recordingApplier{}
	w := NewWorker(bus, applier)

	ctx, cancel := context.WithCancel(context.Background())
	pool := NewPool(w, 3)
	pool.Start(ctx)

	want := []events.TweetPosted{{TweetID: 1, AuthorID: 10}, {TweetID: 2, AuthorID: 20}}
	for _, e := range want {
		if err := bus.Publish(ctx, e); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	waitFor(t, func() bool { return applier.count() == len(want) })
	cancel()
	pool.Wait()

	if got := applier.count(); got != len(want) {
		t.Fatalf("applied %d events, want %d", got, len(want))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
