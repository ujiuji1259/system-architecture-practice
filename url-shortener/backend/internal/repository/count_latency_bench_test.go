package repository

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// benchRepoWithEvents creates a single link "hot" and inserts nEvents access
// events for it, so we can measure how deriving visit_count (a COUNT over
// link_events) scales with the number of events for one code.
func benchRepoWithEvents(b *testing.B, nEvents int) *SQLite {
	b.Helper()
	dsn := filepath.Join(b.TempDir(), "count.db")
	s, err := New(context.Background(), dsn, NewMemoryCache())
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.Create(ctx, Link{Code: "hot", URL: "https://x.example", CreatedAt: time.Now()}); err != nil {
		b.Fatalf("Create: %v", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO link_events (code, accessed_at, referer, user_agent) VALUES ('hot', ?, '', '')`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	base := time.Now()
	for i := 0; i < nEvents; i++ {
		ts := base.Add(time.Duration(i) * time.Millisecond).UTC().Format(time.RFC3339Nano)
		if _, err := stmt.Exec(ts); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
	return s
}

// BenchmarkDerivedCount measures the authoritative O(N) count over the event
// log (the cache-miss rebuild path) as the number of events grows.
func BenchmarkDerivedCount(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			s := benchRepoWithEvents(b, n)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := s.countEvents(ctx, "hot")
				if err != nil {
					b.Fatalf("countEvents: %v", err)
				}
				if got != int64(n) {
					b.Fatalf("count = %d, want %d", got, n)
				}
			}
		})
	}
}
