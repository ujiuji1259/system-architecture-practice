package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/api"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/fanout"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/hometimeline"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/service"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/snowflake"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/timeline"
)

// newTestServer wires the full stack (SQLite + in-memory timeline/queue/cache +
// a running fan-out worker pool) behind an httptest server, exactly as the
// production binary but with the given celebrity threshold.
func newTestServer(t *testing.T, threshold int64) *httptest.Server {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "it.db")
	repo, err := repository.New(context.Background(), dsn, repository.NewMemoryCache())
	if err != nil {
		t.Fatalf("repository.New: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	tl := timeline.NewMemory(800)
	bus := events.NewMemoryBus(0)
	policy := hometimeline.CelebrityPolicy{Threshold: threshold}
	projection := hometimeline.New(repo, tl, hometimeline.Config{Policy: policy, MaxLen: 800})
	svc := service.New(repo, projection, bus, snowflake.New(1))

	worker := fanout.NewWorker(bus, projection)
	pool := fanout.NewPool(worker, 2)
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	t.Cleanup(func() { cancel(); pool.Wait() })

	srv := httptest.NewServer(newRouter(svc))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func createUser(t *testing.T, base, handle string) api.User {
	t.Helper()
	resp := do(t, http.MethodPost, base+"/users", `{"handle":"`+handle+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s status = %d, want 201", handle, resp.StatusCode)
	}
	return decode[api.User](t, resp)
}

func postTweet(t *testing.T, base, authorID, text string) api.Tweet {
	t.Helper()
	resp := do(t, http.MethodPost, base+"/tweets", `{"author_id":"`+authorID+`","text":"`+text+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post tweet status = %d, want 201", resp.StatusCode)
	}
	return decode[api.Tweet](t, resp)
}

// homeContains polls the home timeline until it contains tweetID (fan-out is
// asynchronous) or times out.
func homeContains(t *testing.T, base, userID, tweetID string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := do(t, http.MethodGet, base+"/timelines/home?user_id="+userID, "")
		if resp.StatusCode == http.StatusOK {
			list := decode[api.TweetList](t, resp)
			for _, tw := range list.Items {
				if tw.Id == tweetID {
					return true
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestFanOutOnWrite: an ordinary author's tweet is pushed to a follower's home
// timeline by the worker, and non-followers do not see it.
func TestFanOutOnWrite(t *testing.T) {
	srv := newTestServer(t, 10000)
	base := srv.URL + "/api/v1"

	alice := createUser(t, base, "alice")
	bob := createUser(t, base, "bob")
	carol := createUser(t, base, "carol")

	if resp := do(t, http.MethodPut, base+"/users/"+alice.Id+"/following/"+bob.Id, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("follow status = %d, want 204", resp.StatusCode)
	}

	tw := postTweet(t, base, bob.Id, "hello followers")

	if !homeContains(t, base, alice.Id, tw.Id) {
		t.Fatal("alice's home timeline never received bob's tweet")
	}
	// Carol follows nobody: her home stays empty.
	resp := do(t, http.MethodGet, base+"/timelines/home?user_id="+carol.Id, "")
	if list := decode[api.TweetList](t, resp); len(list.Items) != 0 {
		t.Errorf("carol home = %d items, want 0", len(list.Items))
	}
}

// TestCelebrityPullOnRead: with a low threshold the author is a celebrity, so
// the worker skips fan-out, yet the tweet still appears in a follower's home
// timeline via the read-time pull.
func TestCelebrityPullOnRead(t *testing.T) {
	srv := newTestServer(t, 0) // everyone with >=1 follower is a "celebrity"
	base := srv.URL + "/api/v1"

	fan := createUser(t, base, "fan")
	star := createUser(t, base, "star")

	if resp := do(t, http.MethodPut, base+"/users/"+fan.Id+"/following/"+star.Id, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("follow status = %d, want 204", resp.StatusCode)
	}
	tw := postTweet(t, base, star.Id, "from the star")

	// No fan-out happens, but the pull path must still surface the tweet
	// immediately (no polling needed).
	resp := do(t, http.MethodGet, base+"/timelines/home?user_id="+fan.Id, "")
	list := decode[api.TweetList](t, resp)
	if len(list.Items) != 1 || list.Items[0].Id != tw.Id {
		t.Fatalf("fan home = %v, want the star's tweet %s via pull", list.Items, tw.Id)
	}
}

func TestValidationAndErrors(t *testing.T) {
	srv := newTestServer(t, 10000)
	base := srv.URL + "/api/v1"

	// bad handle -> 400
	if resp := do(t, http.MethodPost, base+"/users", `{"handle":"bad handle"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad handle status = %d, want 400", resp.StatusCode)
	}
	// duplicate handle -> 409
	createUser(t, base, "dup")
	if resp := do(t, http.MethodPost, base+"/users", `{"handle":"dup"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("dup handle status = %d, want 409", resp.StatusCode)
	}
	// self-follow -> 400
	u := createUser(t, base, "solo")
	if resp := do(t, http.MethodPut, base+"/users/"+u.Id+"/following/"+u.Id, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("self-follow status = %d, want 400", resp.StatusCode)
	}
	// follow a missing user -> 404
	if resp := do(t, http.MethodPut, base+"/users/"+u.Id+"/following/9999999", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("follow missing status = %d, want 404", resp.StatusCode)
	}
	// home for a missing user -> 404
	if resp := do(t, http.MethodGet, base+"/timelines/home?user_id=9999999", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("home missing user status = %d, want 404", resp.StatusCode)
	}
}

func TestHomeTimelinePagination(t *testing.T) {
	srv := newTestServer(t, 10000)
	base := srv.URL + "/api/v1"

	me := createUser(t, base, "me")
	author := createUser(t, base, "author")
	if resp := do(t, http.MethodPut, base+"/users/"+me.Id+"/following/"+author.Id, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("follow status = %d", resp.StatusCode)
	}

	var last string
	for i := 0; i < 5; i++ {
		last = postTweet(t, base, author.Id, "tweet").Id
	}
	if !homeContains(t, base, me.Id, last) {
		t.Fatal("newest tweet never fanned out")
	}

	// First page of 2, then follow the cursor.
	resp := do(t, http.MethodGet, base+"/timelines/home?user_id="+me.Id+"&limit=2", "")
	page1 := decode[api.TweetList](t, resp)
	if len(page1.Items) != 2 || page1.NextCursor == nil {
		t.Fatalf("page1 = %d items, cursor=%v, want 2 items and a cursor", len(page1.Items), page1.NextCursor)
	}
	resp = do(t, http.MethodGet, base+"/timelines/home?user_id="+me.Id+"&limit=2&before_id="+*page1.NextCursor, "")
	page2 := decode[api.TweetList](t, resp)
	if len(page2.Items) != 2 {
		t.Fatalf("page2 = %d items, want 2", len(page2.Items))
	}
	// Pages must not overlap and must descend.
	if page2.Items[0].Id >= page1.Items[1].Id {
		t.Errorf("page2 head %s should be older than page1 tail %s", page2.Items[0].Id, page1.Items[1].Id)
	}
}
