package repository

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestRepoWithCache builds a store with a caller-supplied cache so tests can
// inspect what the repository put in it.
func newTestRepoWithCache(t *testing.T, cache CountCache) *SQLite {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := New(context.Background(), dsn, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGetRebuildsCountFromEventLog(t *testing.T) {
	cache := NewMemoryCache()
	s := newTestRepoWithCache(t, cache)
	ctx := context.Background()

	if err := s.Create(ctx, Link{Code: "hot", URL: "https://x.example", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Record two visits; the cache stays cold because Incr is a no-op while absent.
	for i := 0; i < 2; i++ {
		if _, err := s.RecordVisit(ctx, "hot", Event{Code: "hot", AccessedAt: time.Now()}); err != nil {
			t.Fatalf("RecordVisit: %v", err)
		}
	}
	if _, ok, _ := cache.Get(ctx, "hot"); ok {
		t.Fatal("cache should be cold before first Get")
	}

	// First Get rebuilds the count from the event log and warms the cache.
	got, err := s.Get(ctx, "hot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.VisitCount != 2 {
		t.Errorf("VisitCount = %d, want 2", got.VisitCount)
	}
	if n, ok, _ := cache.Get(ctx, "hot"); !ok || n != 2 {
		t.Errorf("cache after Get = (%d,%v), want (2,true)", n, ok)
	}
}

func TestRecordVisitIncrementsWarmCache(t *testing.T) {
	cache := NewMemoryCache()
	s := newTestRepoWithCache(t, cache)
	ctx := context.Background()

	if err := s.Create(ctx, Link{Code: "hot", URL: "https://x.example", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Get(ctx, "hot"); err != nil { // warm to 0
		t.Fatalf("Get: %v", err)
	}
	if _, err := s.RecordVisit(ctx, "hot", Event{Code: "hot", AccessedAt: time.Now()}); err != nil {
		t.Fatalf("RecordVisit: %v", err)
	}
	if n, ok, _ := cache.Get(ctx, "hot"); !ok || n != 1 {
		t.Errorf("cache after RecordVisit = (%d,%v), want (1,true)", n, ok)
	}
}

func TestRecordVisitNoOpWhenCold(t *testing.T) {
	cache := NewMemoryCache()
	s := newTestRepoWithCache(t, cache)
	ctx := context.Background()

	if err := s.Create(ctx, Link{Code: "hot", URL: "https://x.example", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Cold cache: Incr must be a no-op so it does not fabricate a count of 1.
	if _, err := s.RecordVisit(ctx, "hot", Event{Code: "hot", AccessedAt: time.Now()}); err != nil {
		t.Fatalf("RecordVisit: %v", err)
	}
	if _, ok, _ := cache.Get(ctx, "hot"); ok {
		t.Fatal("cold cache must stay absent after Incr no-op")
	}
	// Get rebuilds from the event log to the true value.
	got, err := s.Get(ctx, "hot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.VisitCount != 1 {
		t.Errorf("VisitCount = %d, want 1", got.VisitCount)
	}
}

func TestDeleteEvictsCache(t *testing.T) {
	cache := NewMemoryCache()
	s := newTestRepoWithCache(t, cache)
	ctx := context.Background()

	if err := s.Create(ctx, Link{Code: "gone", URL: "https://d.example", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Get(ctx, "gone"); err != nil { // warm cache
		t.Fatalf("Get: %v", err)
	}
	if err := s.Delete(ctx, "gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := cache.Get(ctx, "gone"); ok {
		t.Error("cache entry should be evicted after Delete")
	}
	if _, err := s.Get(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrNotFound", err)
	}
}
