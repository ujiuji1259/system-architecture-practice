// Package fanout is the runtime that drives home-timeline projection updates. It
// is only the delivery mechanism: a pool of worker goroutines that subscribe to
// TweetPosted events and hand each one to an Applier (the hometimeline
// projection). All the logic about what a timeline contains — the celebrity
// policy, fan-out, rebuild — lives in the projection, not here.
//
// Keeping the runtime (event subscription, worker pool, concurrency, lifecycle)
// separate from the projection rule means "how updates are delivered" and "what
// an update does" change in different places.
package fanout

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
)

// Subscriber delivers TweetPosted events. ok is false (with nil error) when the
// context is done and the worker should stop.
type Subscriber interface {
	Poll(ctx context.Context) (e events.TweetPosted, ok bool, err error)
}

// Applier maintains the projection for a single event (implemented by the
// hometimeline projection).
type Applier interface {
	Apply(ctx context.Context, e events.TweetPosted) error
}

// Worker consumes events and applies them. It is stateless apart from its
// failure counter, so a single Worker can be Run from many goroutines.
type Worker struct {
	sub    Subscriber
	apply  Applier
	failed atomic.Int64
}

// NewWorker builds a Worker.
func NewWorker(sub Subscriber, apply Applier) *Worker {
	return &Worker{sub: sub, apply: apply}
}

// Failed returns the number of events whose application errored.
func (w *Worker) Failed() int64 { return w.failed.Load() }

// Run consumes TweetPosted events until the context is done.
func (w *Worker) Run(ctx context.Context) {
	for {
		e, ok, err := w.sub.Poll(ctx)
		if err != nil {
			slog.Error("fanout poll failed", "err", err)
			continue
		}
		if !ok {
			return
		}
		if err := w.apply.Apply(ctx, e); err != nil {
			w.failed.Add(1)
			slog.Error("fanout apply failed", "tweet_id", e.TweetID, "err", err)
		}
	}
}

// Pool runs a fixed number of worker goroutines over a shared Worker.
type Pool struct {
	worker *Worker
	n      int
	wg     sync.WaitGroup
}

// NewPool builds a pool of n goroutines driving worker.
func NewPool(worker *Worker, n int) *Pool {
	if n <= 0 {
		n = 1
	}
	return &Pool{worker: worker, n: n}
}

// Start launches the worker goroutines; they stop when ctx is done.
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.n; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.worker.Run(ctx)
		}()
	}
}

// Wait blocks until all workers have returned (after the context is done).
func (p *Pool) Wait() { p.wg.Wait() }
