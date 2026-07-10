package repository

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// newBenchRepo builds a store seeded with n links. When withIndex is false the
// created_at index is dropped, so ORDER BY must fall back to a full scan + sort.
func newBenchRepo(b *testing.B, n int, withIndex bool) *SQLite {
	b.Helper()
	dsn := filepath.Join(b.TempDir(), "bench.db")
	s, err := New(context.Background(), dsn)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	if !withIndex {
		if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_links_created_at`); err != nil {
			b.Fatalf("drop index: %v", err)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO links (code, url, created_at) VALUES (?, ?, ?)`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	base := time.Now()
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("code%08d", i)
		created := base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano)
		if _, err := stmt.Exec(code, "https://example.com/"+code, created); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
	return s
}

// benchmark parameters: deep pagination amplifies the ordering cost.
const (
	benchRows   = 100_000
	benchLimit  = 20
	benchOffset = benchRows / 2
)

func benchmarkList(b *testing.B, withIndex bool) {
	s := newBenchRepo(b, benchRows, withIndex)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		links, total, err := s.List(ctx, benchLimit, benchOffset)
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		if total != benchRows || len(links) != benchLimit {
			b.Fatalf("unexpected result: total=%d len=%d", total, len(links))
		}
	}
}

func BenchmarkListWithIndex(b *testing.B)    { benchmarkList(b, true) }
func BenchmarkListWithoutIndex(b *testing.B) { benchmarkList(b, false) }
