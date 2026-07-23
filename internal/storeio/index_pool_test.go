package storeio

import (
	"runtime"
	"sync"
	"testing"
)

func TestIndexPoolConcurrentReuse(t *testing.T) {
	const (
		indexes = 64
		workers = 8
		loops   = 10_000
	)
	pool := newIndexPool(indexes)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for range loops {
				var index uint32
				for {
					var ok bool
					index, ok = pool.pop()
					if ok {
						break
					}
					runtime.Gosched()
				}
				pool.push(index)
			}
		}()
	}
	group.Wait()

	seen := make([]bool, indexes)
	for range indexes {
		index, ok := pool.pop()
		if !ok {
			t.Fatal("pool lost an index")
		}
		if index >= indexes || seen[index] {
			t.Fatalf("invalid or duplicate index %d", index)
		}
		seen[index] = true
	}
	if _, ok := pool.pop(); ok {
		t.Fatal("pool returned more indexes than initialized")
	}
}

func TestIndexPoolMaximumDeviceIndex(t *testing.T) {
	const count = 1 << 16
	pool := newIndexPool(count)
	for want := uint32(0); want < count; want++ {
		got, ok := pool.pop()
		if !ok || got != want {
			t.Fatalf("pop = (%d, %v), want (%d, true)", got, ok, want)
		}
	}
	if _, ok := pool.pop(); ok {
		t.Fatal("maximum-sized pool returned an extra index")
	}
}
