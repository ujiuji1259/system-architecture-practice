package hometimeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/events"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
)

// BenchmarkApply shows the write amplification of fan-out on write: applying a
// single tweet costs O(followers), which is exactly why celebrities are skipped
// and pulled at read time instead.
func BenchmarkApply(b *testing.B) {
	for _, followers := range []int{100, 10_000, 100_000} {
		b.Run(fmt.Sprintf("followers=%d", followers), func(b *testing.B) {
			ids := make([]int64, followers)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			repo := &fakeRepo{
				authors:   map[int64]repository.TweetAuthor{1: {AuthorID: 1, FollowerCount: int64(followers)}},
				followers: map[int64][]int64{1: ids},
			}
			// A no-op store isolates the fan-out walk from storage cost.
			p := New(repo, noopStore{}, Config{
				Policy:   CelebrityPolicy{Threshold: int64(followers) + 1},
				PageSize: 1000,
			})
			e := events.TweetPosted{TweetID: 1, AuthorID: 1}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				repo.pages = 0
				if err := p.Apply(context.Background(), e); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(followers), "edges/op")
		})
	}
}

type noopStore struct{}

func (noopStore) PushMany(context.Context, []int64, int64) error            { return nil }
func (noopStore) Fill(context.Context, int64, []int64) error                { return nil }
func (noopStore) Range(context.Context, int64, int64, int) ([]int64, error) { return nil, nil }
