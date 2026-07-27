package durable

// Throwaway diagnostic: why does the buffered-visible Put path miss the page
// cache continuously? Delete together with zz_profile_buffered_bench_test.go.

import (
	"math/rand"
	"os"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

func TestZZCacheDiag(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "zz-cache-diag-*")
	if err != nil {
		t.Fatal(err)
	}
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, zzProfileDocs)
	for i := range keys {
		keys[i] = zzKey(i)
		if err := builder.Append(keys[i], zzProfileValue(i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	options := zzProfileOptions()
	if _, err := CreateFrom(built, file, options); err != nil {
		t.Fatal(err)
	}
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = collection.Close()
		_ = file.Close()
	}()

	rng := rand.New(rand.NewSource(7))
	report := func(label string) {
		s := collection.Stats()
		t.Logf("%-18s misses=%d evictions=%d readBytes=%d resident=%d RESERVED=%d dirty=%d activeSnaps=%d pendingRetired=%d/%dB",
			label, s.CacheMisses, s.Evictions, s.ReadBytes,
			s.ResidentBytes, s.ReservedBytes, s.DirtyBytes,
			s.ActiveSnapshots, s.PendingRetiredExtents, s.PendingRetiredBytes)
	}
	report("after open")
	phase := func(label string, count int) {
		for i := 0; i < count; i++ {
			if i%64 == 63 {
				if err := collection.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			id := rng.Intn(zzProfileDocs)
			if _, err := collection.Put(keys[id], zzProfileValue(id, i+1)); err != nil {
				t.Fatal(err)
			}
		}
		report(label)
	}
	phase("warm 20k", 20_000)
	phase("steady 10k", 10_000)
	phase("steady 10k more", 10_000)
}

func zzKey(i int) string {
	const digits = "0123456789"
	buf := []byte("doc:00000000")
	for p := len(buf) - 1; i > 0; p-- {
		buf[p] = digits[i%10]
		i /= 10
	}
	return string(buf)
}
