package repository

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkGetCountDerivedVsCached contrasts the O(N) rebuild (counting the
// event log) with the O(1) warm-cache Get as the number of events grows.
func BenchmarkGetCountDerivedVsCached(b *testing.B) {
	for _, n := range []int{100, 10_000, 1_000_000} {
		b.Run(fmt.Sprintf("derived/events=%d", n), func(b *testing.B) {
			s := benchRepoWithEvents(b, n)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.countEvents(ctx, "hot"); err != nil {
					b.Fatalf("countEvents: %v", err)
				}
			}
		})

		b.Run(fmt.Sprintf("cached/events=%d", n), func(b *testing.B) {
			s := benchRepoWithEvents(b, n)
			ctx := context.Background()
			if _, err := s.Get(ctx, "hot"); err != nil { // warm the cache
				b.Fatalf("warm Get: %v", err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Get(ctx, "hot"); err != nil {
					b.Fatalf("Get: %v", err)
				}
			}
		})
	}
}
