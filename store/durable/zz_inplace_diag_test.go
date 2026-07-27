package durable

// Throwaway diagnostic for the phase-A in-place path. Delete with zz_*.go.

import (
	"math/rand"
	"os"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

func TestZZInplaceDiag(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "zz-inplace-*")
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
	// Under the lazy second-touch rule, in-place engagement requires REPEAT
	// touches of one document frame inside one checkpoint window. Pass 1 is
	// uniform random (first touches dominate every window: expect ~0
	// in-place, all ordinary COW). Pass 2 hammers 4 hot keys, so each
	// 64-put window touches each key ~16 times: first touch COWs and
	// registers, the rest go in-place — expect >= 90% updates.
	rng := rand.New(rand.NewSource(9))
	pass := func(label string, next func(i int) int, revisionBase int) {
		before := collection.Stats()
		for i := 0; i < 2_000; i++ {
			if i%64 == 63 {
				if err := collection.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			id := next(i)
			if _, err := collection.Put(keys[id], zzProfileValue(id, revisionBase+i)); err != nil {
				t.Fatal(err)
			}
		}
		s := collection.Stats()
		t.Logf("%s: attempts=%d updates=%d fallbacks=%d", label,
			s.BufferedInplaceAttempts-before.BufferedInplaceAttempts,
			s.BufferedInplaceUpdates-before.BufferedInplaceUpdates,
			s.BufferedInplaceFallbacks-before.BufferedInplaceFallbacks)
	}
	pass("pass1 uniform-cold", func(int) int { return rng.Intn(zzProfileDocs) }, 1)
	hot := []int{17, 4211, 60_003, 99_991}
	pass("pass2 hot-repeat", func(i int) int { return hot[i%len(hot)] }, 100_000)
}
