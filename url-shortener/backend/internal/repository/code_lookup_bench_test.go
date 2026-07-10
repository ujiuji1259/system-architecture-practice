package repository

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// newCodeBenchDB builds a table whose primary key is an autoincrement integer
// and whose `code` is an ordinary column. When withCodeIndex is true a secondary
// index is added on `code`; otherwise a lookup by code requires a full scan.
func newCodeBenchDB(b *testing.B, n int, withCodeIndex bool) *sql.DB {
	b.Helper()
	dsn := filepath.Join(b.TempDir(), "code_bench.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	b.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE links (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			code        TEXT    NOT NULL,
			url         TEXT    NOT NULL,
			visit_count INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT    NOT NULL
		)`); err != nil {
		b.Fatalf("create table: %v", err)
	}
	if withCodeIndex {
		if _, err := db.Exec(`CREATE UNIQUE INDEX idx_links_code ON links(code)`); err != nil {
			b.Fatalf("create index: %v", err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO links (code, url, visit_count, created_at) VALUES (?, ?, 0, ?)`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("code%08d", i)
		if _, err := stmt.Exec(code, "https://example.com/"+code, "2026-01-01T00:00:00Z"); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
	return db
}

func benchmarkCodeLookup(b *testing.B, withCodeIndex bool) {
	db := newCodeBenchDB(b, benchRows, withCodeIndex)

	// Precompute random target codes so we don't repeatedly hit one cached row.
	rng := rand.New(rand.NewSource(1))
	targets := make([]string, 1000)
	for i := range targets {
		targets[i] = fmt.Sprintf("code%08d", rng.Intn(benchRows))
	}

	stmt, err := db.Prepare(`SELECT id, code, url, visit_count, created_at FROM links WHERE code = ?`)
	if err != nil {
		b.Fatalf("prepare select: %v", err)
	}
	defer func() { _ = stmt.Close() }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var (
			id                   int64
			code, url, createdAt string
			visitCount           int64
		)
		err := stmt.QueryRow(targets[i%len(targets)]).Scan(&id, &code, &url, &visitCount, &createdAt)
		if err != nil {
			b.Fatalf("lookup: %v", err)
		}
	}
}

func BenchmarkCodeLookupWithIndex(b *testing.B)    { benchmarkCodeLookup(b, true) }
func BenchmarkCodeLookupWithoutIndex(b *testing.B) { benchmarkCodeLookup(b, false) }
