package repository

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestRepo(t *testing.T) *SQLite {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndGet(t *testing.T) {
	s := newTestRepo(t)
	ctx := context.Background()

	want := Link{Code: "abc123", URL: "https://example.com", CreatedAt: time.Now()}
	if err := s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Code != want.Code || got.URL != want.URL {
		t.Errorf("Get = %+v, want code=%s url=%s", got, want.Code, want.URL)
	}
	if got.VisitCount != 0 {
		t.Errorf("VisitCount = %d, want 0", got.VisitCount)
	}
	if !got.CreatedAt.Equal(want.CreatedAt.UTC().Truncate(time.Nanosecond)) &&
		got.CreatedAt.Sub(want.CreatedAt).Abs() > time.Millisecond {
		t.Errorf("CreatedAt = %v, want ~%v", got.CreatedAt, want.CreatedAt)
	}
}

func TestCreateDuplicateReturnsErrCodeExists(t *testing.T) {
	s := newTestRepo(t)
	ctx := context.Background()

	l := Link{Code: "dup", URL: "https://a.example", CreatedAt: time.Now()}
	if err := s.Create(ctx, l); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := s.Create(ctx, Link{Code: "dup", URL: "https://b.example", CreatedAt: time.Now()})
	if !errors.Is(err, ErrCodeExists) {
		t.Fatalf("second Create err = %v, want ErrCodeExists", err)
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	s := newTestRepo(t)
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
}

func TestRecordVisitAndDerivedCount(t *testing.T) {
	s := newTestRepo(t)
	ctx := context.Background()

	if err := s.Create(ctx, Link{Code: "hit", URL: "https://x.example", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 1; i <= 3; i++ {
		url, err := s.RecordVisit(ctx, "hit", Event{
			Code: "hit", AccessedAt: time.Now(), Referer: "https://ref.example", UserAgent: "curl/8",
		})
		if err != nil {
			t.Fatalf("RecordVisit: %v", err)
		}
		if url != "https://x.example" {
			t.Errorf("url = %q, want target", url)
		}
	}

	// visit_count is derived from the recorded events.
	got, err := s.Get(ctx, "hit")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.VisitCount != 3 {
		t.Errorf("derived VisitCount = %d, want 3", got.VisitCount)
	}

	// Events are listed newest-first with their metadata.
	events, total, err := s.ListEvents(ctx, "hit", 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if total != 3 || len(events) != 3 {
		t.Fatalf("total=%d len=%d, want 3/3", total, len(events))
	}
	if events[0].Referer != "https://ref.example" || events[0].UserAgent != "curl/8" {
		t.Errorf("event metadata = %+v, want referer/user-agent set", events[0])
	}
}

func TestRecordVisitMissing(t *testing.T) {
	s := newTestRepo(t)
	_, err := s.RecordVisit(context.Background(), "nope", Event{Code: "nope", AccessedAt: time.Now()})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteCascadesEvents(t *testing.T) {
	s := newTestRepo(t)
	ctx := context.Background()

	if err := s.Create(ctx, Link{Code: "c", URL: "https://c.example", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.RecordVisit(ctx, "c", Event{Code: "c", AccessedAt: time.Now()}); err != nil {
		t.Fatalf("RecordVisit: %v", err)
	}
	if err := s.Delete(ctx, "c"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// After the link is gone, its events are gone too (ON DELETE CASCADE).
	if _, total, err := s.ListEvents(ctx, "c", 10, 0); err != nil || total != 0 {
		t.Errorf("events after delete: total=%d err=%v, want 0/nil", total, err)
	}
}

func TestListOrderingAndPaging(t *testing.T) {
	s := newTestRepo(t)
	ctx := context.Background()

	base := time.Now()
	// Insert oldest→newest; List must return newest→oldest.
	for i, code := range []string{"old", "mid", "new"} {
		l := Link{Code: code, URL: "https://e.example/" + code, CreatedAt: base.Add(time.Duration(i) * time.Second)}
		if err := s.Create(ctx, l); err != nil {
			t.Fatalf("Create %s: %v", code, err)
		}
	}

	links, total, err := s.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	wantOrder := []string{"new", "mid", "old"}
	for i, l := range links {
		if l.Code != wantOrder[i] {
			t.Errorf("links[%d].Code = %s, want %s", i, l.Code, wantOrder[i])
		}
	}

	// Paging: limit 1, offset 1 -> the middle item.
	page, _, err := s.List(ctx, 1, 1)
	if err != nil {
		t.Fatalf("List page: %v", err)
	}
	if len(page) != 1 || page[0].Code != "mid" {
		t.Errorf("paged list = %+v, want single 'mid'", page)
	}
}

func TestDelete(t *testing.T) {
	s := newTestRepo(t)
	ctx := context.Background()

	if err := s.Create(ctx, Link{Code: "gone", URL: "https://d.example", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, "gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete err = %v, want ErrNotFound", err)
	}
}
