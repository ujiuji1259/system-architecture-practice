package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/repository"
)

// fakeRepo is an in-memory Repository with programmable failures.
type fakeRepo struct {
	links map[string]repository.Link
	// failCreates causes the next N Create calls to return ErrCodeExists,
	// simulating short-code collisions.
	failCreates int
	createCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{links: make(map[string]repository.Link)}
}

func (f *fakeRepo) Create(_ context.Context, l repository.Link) error {
	f.createCalls++
	if f.failCreates > 0 {
		f.failCreates--
		return repository.ErrCodeExists
	}
	if _, ok := f.links[l.Code]; ok {
		return repository.ErrCodeExists
	}
	f.links[l.Code] = l
	return nil
}

func (f *fakeRepo) Get(_ context.Context, code string) (repository.Link, error) {
	l, ok := f.links[code]
	if !ok {
		return repository.Link{}, repository.ErrNotFound
	}
	return l, nil
}

func (f *fakeRepo) List(_ context.Context, limit, offset int) ([]repository.Link, int64, error) {
	all := make([]repository.Link, 0, len(f.links))
	for _, l := range f.links {
		all = append(all, l)
	}
	total := int64(len(all))
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (f *fakeRepo) Delete(_ context.Context, code string) error {
	if _, ok := f.links[code]; !ok {
		return repository.ErrNotFound
	}
	delete(f.links, code)
	return nil
}

func (f *fakeRepo) GetAndCountVisit(_ context.Context, code string) (repository.Link, error) {
	l, ok := f.links[code]
	if !ok {
		return repository.Link{}, repository.ErrNotFound
	}
	l.VisitCount++
	f.links[code] = l
	return l, nil
}

func ptr[T any](v T) *T { return &v }

func TestCreateGeneratedCode(t *testing.T) {
	svc := New(newFakeRepo())
	link, err := svc.Create(context.Background(), "https://example.com/path", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.Code == "" {
		t.Error("generated code is empty")
	}
	if link.URL != "https://example.com/path" {
		t.Errorf("URL = %q", link.URL)
	}
}

func TestCreateCustomAlias(t *testing.T) {
	svc := New(newFakeRepo())
	link, err := svc.Create(context.Background(), "https://go.dev", ptr("godev"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.Code != "godev" {
		t.Errorf("Code = %q, want godev", link.Code)
	}
}

func TestCreateDuplicateAliasIsErrAliasTaken(t *testing.T) {
	fr := newFakeRepo()
	fr.links["godev"] = repository.Link{Code: "godev", URL: "https://go.dev"}
	svc := New(fr)

	_, err := svc.Create(context.Background(), "https://go.dev", ptr("godev"))
	if !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("err = %v, want ErrAliasTaken", err)
	}
}

// The validation logic is DB-independent, so it is tested as pure functions
// with no repository, mock, or context involved.

func TestValidateURL(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantNorm string
	}{
		{"https://ok.example/path", true, "https://ok.example/path"},
		{"http://ok.example", true, "http://ok.example"},
		{"  https://trim.example  ", true, "https://trim.example"},
		{"ftp://example.com", false, ""},
		{"notaurl", false, ""},
		{"   ", false, ""},
		{"https://", false, ""}, // scheme but no host
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateURL(tc.in)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("validateURL(%q) unexpected error: %v", tc.in, err)
				}
				if got != tc.wantNorm {
					t.Errorf("normalized = %q, want %q", got, tc.wantNorm)
				}
				return
			}
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("validateURL(%q) err = %v, want ErrInvalidURL", tc.in, err)
			}
		})
	}
}

func TestValidateAlias(t *testing.T) {
	valid := []string{
		"abc",
		"my-link",
		"a_b-3",
		"AbC123",
		"abcdefghijklmnopqrstuvwxyzABCDEF", // 32 chars, the max
	}
	for _, a := range valid {
		if err := validateAlias(a); err != nil {
			t.Errorf("validateAlias(%q) = %v, want nil", a, err)
		}
	}

	invalid := []string{
		"",                                  // empty
		"ab",                                // too short (<3)
		"has space",                         // space
		"with/slash",                        // disallowed char
		"abcdefghijklmnopqrstuvwxyzABCDEFG", // 33 chars, too long
		"emoji\U0001F600xx",                 // non-ASCII
	}
	for _, a := range invalid {
		if err := validateAlias(a); !errors.Is(err, ErrInvalidAlias) {
			t.Errorf("validateAlias(%q) = %v, want ErrInvalidAlias", a, err)
		}
	}
}

func TestCreateRetriesOnCollision(t *testing.T) {
	fr := newFakeRepo()
	fr.failCreates = 3 // first 3 attempts collide, 4th succeeds
	svc := New(fr)

	_, err := svc.Create(context.Background(), "https://example.com", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fr.createCalls != 4 {
		t.Errorf("createCalls = %d, want 4", fr.createCalls)
	}
}

func TestCreateExhaustsRetries(t *testing.T) {
	fr := newFakeRepo()
	fr.failCreates = maxGenAttempts + 1
	svc := New(fr)

	_, err := svc.Create(context.Background(), "https://example.com", nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	// It is an internal failure, not one of the input-validation sentinels.
	if errors.Is(err, ErrInvalidURL) || errors.Is(err, ErrAliasTaken) {
		t.Errorf("unexpected sentinel error: %v", err)
	}
}

func TestGet(t *testing.T) {
	fr := newFakeRepo()
	fr.links["abc"] = repository.Link{Code: "abc", URL: "https://example.com", VisitCount: 7}
	svc := New(fr)

	link, err := svc.Get(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if link.VisitCount != 7 {
		t.Errorf("VisitCount = %d, want 7", link.VisitCount)
	}

	if _, err := svc.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	fr := newFakeRepo()
	fr.links["a"] = repository.Link{Code: "a", URL: "https://a.example"}
	fr.links["b"] = repository.Link{Code: "b", URL: "https://b.example"}
	svc := New(fr)

	links, total, err := svc.List(context.Background(), 20, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 || len(links) != 2 {
		t.Errorf("total=%d len=%d, want 2/2", total, len(links))
	}
}

func TestDelete(t *testing.T) {
	fr := newFakeRepo()
	fr.links["gone"] = repository.Link{Code: "gone", URL: "https://d.example"}
	svc := New(fr)

	if err := svc.Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := svc.Delete(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete err = %v, want ErrNotFound", err)
	}
}

func TestResolveCountsVisit(t *testing.T) {
	fr := newFakeRepo()
	fr.links["hit"] = repository.Link{Code: "hit", URL: "https://x.example"}
	svc := New(fr)

	link, err := svc.Resolve(context.Background(), "hit")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if link.URL != "https://x.example" {
		t.Errorf("URL = %q", link.URL)
	}
	if link.VisitCount != 1 {
		t.Errorf("VisitCount = %d, want 1", link.VisitCount)
	}

	if _, err := svc.Resolve(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve missing err = %v, want ErrNotFound", err)
	}
}
