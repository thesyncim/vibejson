package storeio

import (
	"fmt"
	"testing"
)

// BenchmarkPageCacheDirtyBudget measures the per-mutation dirty-budget check
// against cache size. The writer runs this once per Put, so anything that
// scales with the frame count here is a size-dependent tax on a bound that is
// O(1) by construction. Stats is measured beside it because that is what the
// check used to call.
func BenchmarkPageCacheDirtyBudget(b *testing.B) {
	for _, megabytes := range []int64{1, 16, 64} {
		file, storeID, _ := newPageCacheFixture(b, 1)
		cache, err := NewPageCache(file, PageCacheOptions{
			PageSize: pageCacheTestPageSize, ResidentBytes: megabytes << 20,
			StoreID: storeID, PrefetchQueue: 4,
		})
		if err != nil {
			b.Fatal(err)
		}
		name := fmt.Sprintf("resident=%dMiB", megabytes)
		b.Run(name+"/DirtyCapacityAvailable", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sink += cache.DirtyCapacityAvailable()
			}
		})
		b.Run(name+"/Stats", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sink += cache.Stats().DirtyBytes
			}
		})
		if err := cache.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

var sink uint64
