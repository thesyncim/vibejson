package durable

// Throwaway profiling benchmarks decomposing the buffered-visible mutation
// path measured by bench/competitive cmd/mixed. Not part of the maintained
// suite; delete before promoting any change they motivated.

import (
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

const zzProfileDocs = 100_000

func zzProfileOptions() Options {
	return Options{
		ResidentBytes:      64 << 20,
		Durability:         DurabilityBufferedVisible,
		Backend:            BackendPortable,
		CheckpointStrength: CheckpointFilesystem,
		MaxBatchDocuments:  1,
		MaxDocumentBytes:   1 << 10,
		BufferCount:        1024,
		QueueSlots:         1024,
		GroupLimit:         64,
	}
}

func zzProfileValue(i, revision int) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"%08d","rev":"%08d","country":"PT","tier":"gold","note":"the quick brown fox jumps over the lazy dog while the benchmark pads this document towards two hundred and fifty bytes of storage","tags":["a","b","c"],"nested":{"score":"%04d"}}`,
		i, revision, i%10_000))
}

func zzProfileOpen(b *testing.B) (*Collection, []string) {
	b.Helper()
	file, err := os.CreateTemp(b.TempDir(), "zz-buffered-*")
	if err != nil {
		b.Fatal(err)
	}
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		b.Fatal(err)
	}
	keys := make([]string, zzProfileDocs)
	for i := range keys {
		keys[i] = fmt.Sprintf("doc:%08d", i)
		if err := builder.Append(keys[i], zzProfileValue(i, 0)); err != nil {
			b.Fatal(err)
		}
	}
	built, err := builder.Build()
	if err != nil {
		b.Fatal(err)
	}
	options := zzProfileOptions()
	if _, err := CreateFrom(built, file, options); err != nil {
		b.Fatal(err)
	}
	collection, err := Open(file, options)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = collection.Close()
		_ = file.Close()
	})
	return collection, keys
}

// BenchmarkZZBufferedAck measures acknowledgement latency alone: same-length
// replacements of random existing keys, with the periodic checkpoint moved
// outside the timer at the harness cadence of 64 mutations.
func BenchmarkZZBufferedAck(b *testing.B) {
	collection, keys := zzProfileOpen(b)
	rng := rand.New(rand.NewSource(1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%64 == 63 {
			b.StopTimer()
			if err := collection.Flush(); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		id := rng.Intn(zzProfileDocs)
		if _, err := collection.Put(keys[id], zzProfileValue(id, i+1)); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := collection.Flush(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkZZBufferedCycle64 measures the complete harness cycle: 64
// same-length replacements plus the CheckpointFilesystem publication, all
// inside the timer. ns/op divided by 64 is the amortized per-mutation cost
// cmd/mixed reports as throughput.
func BenchmarkZZBufferedCycle64(b *testing.B) {
	collection, keys := zzProfileOpen(b)
	rng := rand.New(rand.NewSource(2))
	revision := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 64; j++ {
			revision++
			id := rng.Intn(zzProfileDocs)
			if _, err := collection.Put(keys[id], zzProfileValue(id, revision)); err != nil {
				b.Fatal(err)
			}
		}
		if err := collection.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}
