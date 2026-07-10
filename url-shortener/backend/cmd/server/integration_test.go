package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/api"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/service"
)

const testBaseURL = "http://short.test"

// newTestServer starts an httptest server backed by a real (temp-file) SQLite
// repository, wired exactly as the production binary via newRouter.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "it.db")
	repo, err := repository.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("repository.New: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	srv := httptest.NewServer(newRouter(service.New(repo), testBaseURL))
	t.Cleanup(srv.Close)
	return srv
}

// doJSON performs an HTTP request without following redirects (so a 302 is
// observable) and registers the response body to be closed at test end.
func doJSON(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeLink(t *testing.T, resp *http.Response) api.Link {
	t.Helper()
	var l api.Link
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatalf("decode link: %v", err)
	}
	return l
}

// TestIntegration_Lifecycle walks a link through create → redirect (visit
// counted) → get → list → delete → gone, all over real HTTP.
func TestIntegration_Lifecycle(t *testing.T) {
	srv := newTestServer(t)

	// create with a generated code
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/links", `{"url":"https://example.com/long"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	created := decodeLink(t, resp)
	if created.Code == "" {
		t.Fatal("created code is empty")
	}
	if created.ShortUrl != testBaseURL+"/"+created.Code {
		t.Errorf("short_url = %q, want %s/%s", created.ShortUrl, testBaseURL, created.Code)
	}
	if created.VisitCount != 0 {
		t.Errorf("visit_count = %d, want 0", created.VisitCount)
	}

	// redirect: 302 to the target, counted as a visit
	resp = doJSON(t, http.MethodGet, srv.URL+"/"+created.Code, "")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://example.com/long" {
		t.Errorf("Location = %q, want the target URL", loc)
	}

	// get reflects the incremented visit count
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/links/"+created.Code, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	if got := decodeLink(t, resp); got.VisitCount != 1 {
		t.Errorf("visit_count = %d, want 1", got.VisitCount)
	}

	// list contains the single link
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/links", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var list api.LinkList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Errorf("list total=%d len=%d, want 1/1", list.Total, len(list.Items))
	}

	// delete
	resp = doJSON(t, http.MethodDelete, srv.URL+"/api/v1/links/"+created.Code, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// get after delete → 404
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/links/"+created.Code, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", resp.StatusCode)
	}

	// redirect for the deleted code → 404
	resp = doJSON(t, http.MethodGet, srv.URL+"/"+created.Code, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("redirect missing = %d, want 404", resp.StatusCode)
	}
}

func TestIntegration_CustomAliasAndConflict(t *testing.T) {
	srv := newTestServer(t)

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/links", `{"url":"https://go.dev","custom_alias":"godev"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	if link := decodeLink(t, resp); link.Code != "godev" {
		t.Errorf("code = %q, want godev", link.Code)
	}

	// same alias again → 409
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/links", `{"url":"https://go.dev","custom_alias":"godev"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("conflict status = %d, want 409", resp.StatusCode)
	}
}

func TestIntegration_BadRequests(t *testing.T) {
	srv := newTestServer(t)
	cases := map[string]string{
		"invalid url":   `{"url":"notaurl"}`,
		"ftp scheme":    `{"url":"ftp://example.com"}`,
		"invalid alias": `{"url":"https://ok.example","custom_alias":"a b"}`,
		"missing url":   `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/links", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestIntegration_GetMissing(t *testing.T) {
	srv := newTestServer(t)
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/links/nope", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestIntegration_Health(t *testing.T) {
	srv := newTestServer(t)
	resp := doJSON(t, http.MethodGet, srv.URL+"/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok" {
		t.Errorf("body = %q, want ok", string(b))
	}
}
