package storeio

import (
	"math/rand"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestFreeExtentIndexCapacityBoundaries(t *testing.T) {
	tests := []struct {
		extents int
		maxima  int
	}{
		{extents: -1, maxima: 0},
		{extents: 0, maxima: 0},
		{extents: 1, maxima: 1},
		{extents: 63, maxima: 1},
		{extents: 64, maxima: 1},
		{extents: 65, maxima: 3},
		{extents: 4_096, maxima: 65},
		{extents: 46_200, maxima: 735},
		{extents: 10_000_000, maxima: 158_732},
		{extents: 100_000_000, maxima: 1_587_304},
	}
	for _, test := range tests {
		if got := FreeExtentIndexCapacity(test.extents); got != test.maxima {
			t.Errorf("FreeExtentIndexCapacity(%d) = %d, want %d", test.extents, got, test.maxima)
		}
	}
}

func TestFreeExtentIndexHundredMillionStorageGate(t *testing.T) {
	const count = 100_000_000
	bytes := FreeExtentIndexCapacity(count) * 8
	bytesPerExtent := float64(bytes) / count
	if bytesPerExtent > 0.130 {
		t.Fatalf("100M hierarchy = %.6f B/extent, want <= 0.130",
			bytesPerExtent)
	}
	t.Logf("100M hierarchy=%d bytes (%.6f B/extent); extent table=%d bytes; total resident=%d bytes",
		bytes, bytesPerExtent, count*24, count*24+bytes)

	if os.Getenv("VIBEJSON_ALLOCATOR_SCALE") != "1" {
		t.Skip("set VIBEJSON_ALLOCATOR_SCALE=1 to build and probe the 100M extent table")
	}
	var previousCoalesce, previousRehydrate time.Duration
	for _, measuredCount := range []int{10_000_000, count} {
		extents := freeExtentIndexTestExtents(measuredCount)
		for i := range extents {
			extents[i].Offset = uint64(2*i+4) * (4 << 10)
		}
		extents[measuredCount-1].Length = 64 << 10
		storage := make([]uint64, FreeExtentIndexCapacity(measuredCount))
		var index FreeExtentIndex
		started := time.Now()
		if !index.Rebuild(extents, storage) {
			t.Fatal("Rebuild failed")
		}
		rehydrate := time.Since(started)
		const lookupSamples = 10_000
		var lookupRank int
		var lookupOK bool
		if lookupRank, lookupOK = index.FirstFit(extents, 64<<10); !lookupOK {
			t.Fatal("FirstFit warm-up failed")
		}
		started = time.Now()
		for range lookupSamples {
			lookupRank, lookupOK = index.FirstFit(extents, 64<<10)
		}
		lookup := time.Since(started) / lookupSamples
		if !lookupOK || lookupRank != measuredCount-1 {
			t.Fatalf("FirstFit = %d/%v, want %d/true",
				lookupRank, lookupOK, measuredCount-1)
		}

		// Bridge two one-page holes in the middle. The flat extent array must
		// shift its suffix and the hierarchy must be rehydrated after the count
		// changes; this deliberately measures the allocator's linear mutation
		// lane rather than presenting index.Update as an insertion.
		middle := measuredCount / 2
		started = time.Now()
		extents[middle].Length =
			extents[middle+1].Offset + extents[middle+1].Length -
				extents[middle].Offset
		copy(extents[middle+1:], extents[middle+2:])
		extents = extents[:len(extents)-1]
		if !index.Rebuild(extents, storage) {
			t.Fatal("post-coalesce Rebuild failed")
		}
		coalesce := time.Since(started)
		t.Logf("%d extents: first-fit=%s insert/coalesce=%s reopen-rehydrate=%s hierarchy=%d bytes (%.6f B/extent) total-resident=%d bytes",
			measuredCount, lookup, coalesce, rehydrate, len(storage)*8,
			float64(len(storage)*8)/float64(measuredCount),
			measuredCount*24+len(storage)*8)
		if lookup > time.Microsecond {
			t.Fatalf("%d-extents first-fit = %s, want <= 1µs",
				measuredCount, lookup)
		}
		if previousCoalesce != 0 {
			// The second point contains ten times as many extents. A 20x
			// ceiling leaves machine noise while rejecting superlinear growth.
			if coalesce > 20*previousCoalesce {
				t.Fatalf("insert/coalesce grew from %s to %s across 10x extents",
					previousCoalesce, coalesce)
			}
			if rehydrate > 20*previousRehydrate {
				t.Fatalf("rehydration grew from %s to %s across 10x extents",
					previousRehydrate, rehydrate)
			}
		}
		previousCoalesce, previousRehydrate = coalesce, rehydrate
		runtime.KeepAlive(extents)
		runtime.GC()
	}
}

func TestMeanAdjacentExtentDistanceChurnMetric(t *testing.T) {
	const (
		leaves = 4_096
		cycles = 256
		batch  = 128
		page   = uint64(4 << 10)
	)
	lexical := make([]FreeExtent, leaves)
	for i := range lexical {
		lexical[i] = FreeExtent{Offset: uint64(i) * page, Length: page}
	}
	before := MeanAdjacentExtentDistance(lexical)
	random := rand.New(rand.NewSource(0xa11_0ca1))
	selected := make([]int, leaves)
	for cycle := 0; cycle < cycles; cycle++ {
		copy(selected, random.Perm(leaves))
		offsets := make([]uint64, batch)
		for i, rank := range selected[:batch] {
			offsets[i] = lexical[rank].Offset
		}
		// Lowest-offset first-fit assigns the reclaimed physical holes to keys
		// selected in random lexical order.
		for i := 1; i < len(offsets); i++ {
			for j := i; j > 0 && offsets[j] < offsets[j-1]; j-- {
				offsets[j], offsets[j-1] = offsets[j-1], offsets[j]
			}
		}
		for i, rank := range selected[:batch] {
			lexical[rank].Offset = offsets[i]
		}
	}
	after := MeanAdjacentExtentDistance(lexical)
	if before != float64(page) || after <= before {
		t.Fatalf("mean adjacent physical distance before/after = %.1f/%.1f",
			before, after)
	}
	t.Logf("locality after %d random churn cycles: mean adjacent physical distance %.1f -> %.1f bytes (%.2fx)",
		cycles, before, after, after/before)
}

func TestFreeExtentIndexFirstFitBoundaries(t *testing.T) {
	for _, count := range []int{1, 63, 64, 65, 46_200} {
		t.Run(testCountName(count), func(t *testing.T) {
			extents := freeExtentIndexTestExtents(count)
			for i := range extents {
				extents[i].Length = 4 << 10
			}
			extents[count-1].Length = 64 << 10

			var index FreeExtentIndex
			storage := make([]uint64, FreeExtentIndexCapacity(count))
			if !index.Rebuild(extents, storage) {
				t.Fatal("Rebuild failed")
			}
			if got, ok := index.FirstFit(extents, 4<<10); !ok || got != 0 {
				t.Fatalf("4 KiB FirstFit = %d, %v, want 0, true", got, ok)
			}
			if got, ok := index.FirstFit(extents, 64<<10); !ok || got != count-1 {
				t.Fatalf("64 KiB FirstFit = %d, %v, want %d, true", got, ok, count-1)
			}
			if got, ok := index.FirstFit(extents, 128<<10); ok {
				t.Fatalf("128 KiB FirstFit = %d, true, want no match", got)
			}
			if index.Len() != count {
				t.Fatalf("Len = %d, want %d", index.Len(), count)
			}
			if index.StorageBytes() != len(storage)*8 {
				t.Fatalf("StorageBytes = %d, want %d", index.StorageBytes(), len(storage)*8)
			}
		})
	}
}

func TestFreeExtentIndexUpdatePreservesLowestOffsetFirstFit(t *testing.T) {
	extents := freeExtentIndexTestExtents(130)
	for i := range extents {
		extents[i].Length = 4 << 10
	}
	extents[64].Length = 64 << 10
	extents[129].Length = 64 << 10

	var index FreeExtentIndex
	storage := make([]uint64, FreeExtentIndexCapacity(len(extents)))
	if !index.Rebuild(extents, storage) {
		t.Fatal("Rebuild failed")
	}
	assertFreeExtentFirstFit(t, &index, extents, 64<<10, 64, true)

	extents[64].Length = 0
	if !index.Update(extents, 64) {
		t.Fatal("Update(64) failed")
	}
	assertFreeExtentFirstFit(t, &index, extents, 64<<10, 129, true)

	extents[17].Length = 64 << 10
	if !index.Update(extents, 17) {
		t.Fatal("Update(17) failed")
	}
	assertFreeExtentFirstFit(t, &index, extents, 64<<10, 17, true)

	extents[17].Length = 0
	extents[129].Length = 0
	if !index.Update(extents, 17) || !index.Update(extents, 129) {
		t.Fatal("clearing Update failed")
	}
	assertFreeExtentFirstFit(t, &index, extents, 64<<10, 0, false)
}

func TestFreeExtentIndexRandomizedDifferential(t *testing.T) {
	random := rand.New(rand.NewSource(0x7a11_0ca7))
	for _, count := range []int{1, 2, 63, 64, 65, 255, 256, 4_096, 46_200} {
		extents := freeExtentIndexTestExtents(count)
		for i := range extents {
			extents[i].Length = uint64(random.Intn(33)) * (4 << 10)
		}
		storage := make([]uint64, FreeExtentIndexCapacity(count))
		var index FreeExtentIndex
		if !index.Rebuild(extents, storage) {
			t.Fatalf("count %d: Rebuild failed", count)
		}

		iterations := 2_000
		if count == 46_200 {
			iterations = 10_000
		}
		for iteration := 0; iteration < iterations; iteration++ {
			want := uint64(random.Intn(40)+1) * (4 << 10)
			got, gotOK := index.FirstFit(extents, want)
			wantIndex, wantOK := linearFreeExtentFirstFit(extents, want)
			if got != wantIndex || gotOK != wantOK {
				t.Fatalf(
					"count %d iteration %d want %d: indexed = %d, %v; linear = %d, %v",
					count, iteration, want, got, gotOK, wantIndex, wantOK,
				)
			}

			changed := random.Intn(count)
			extents[changed].Length = uint64(random.Intn(33)) * (4 << 10)
			if !index.Update(extents, changed) {
				t.Fatalf("count %d iteration %d: Update(%d) failed", count, iteration, changed)
			}
		}
	}
}

func TestFreeExtentIndexRejectsInvalidUse(t *testing.T) {
	extents := freeExtentIndexTestExtents(65)
	required := FreeExtentIndexCapacity(len(extents))
	storage := make([]uint64, required)
	var index FreeExtentIndex

	if index.Rebuild(extents, storage[:required-1]) {
		t.Fatal("short-storage Rebuild succeeded")
	}
	if index.Len() != 0 || index.StorageBytes() != 0 {
		t.Fatalf("failed Rebuild retained index: len=%d bytes=%d", index.Len(), index.StorageBytes())
	}
	if _, ok := index.FirstFit(extents, 4<<10); ok {
		t.Fatal("unbuilt FirstFit succeeded")
	}
	if !index.Rebuild(extents, storage) {
		t.Fatal("Rebuild failed")
	}
	if _, ok := index.FirstFit(extents[:64], 4<<10); ok {
		t.Fatal("resized FirstFit succeeded")
	}
	if _, ok := index.FirstFit(extents, 0); ok {
		t.Fatal("zero-length FirstFit succeeded")
	}
	if index.Update(extents[:64], 0) || index.Update(extents, -1) ||
		index.Update(extents, len(extents)) {
		t.Fatal("invalid Update succeeded")
	}
}

func TestFreeExtentIndexZeroAllocationOperations(t *testing.T) {
	extents := freeExtentIndexTestExtents(46_200)
	for i := range extents {
		extents[i].Length = uint64(i%32+1) * (4 << 10)
	}
	storage := make([]uint64, FreeExtentIndexCapacity(len(extents)))
	var index FreeExtentIndex

	if allocations := testing.AllocsPerRun(100, func() {
		if !index.Rebuild(extents, storage) {
			panic("Rebuild failed")
		}
	}); allocations != 0 {
		t.Fatalf("Rebuild allocations = %g, want 0", allocations)
	}
	if allocations := testing.AllocsPerRun(1_000, func() {
		if _, ok := index.FirstFit(extents, 64<<10); !ok {
			panic("FirstFit failed")
		}
	}); allocations != 0 {
		t.Fatalf("FirstFit allocations = %g, want 0", allocations)
	}
	if allocations := testing.AllocsPerRun(1_000, func() {
		extents[17].Length ^= 4 << 10
		if !index.Update(extents, 17) {
			panic("Update failed")
		}
	}); allocations != 0 {
		t.Fatalf("Update allocations = %g, want 0", allocations)
	}
}

func assertFreeExtentFirstFit(
	t *testing.T, index *FreeExtentIndex, extents []FreeExtent, want uint64,
	wantIndex int, wantOK bool,
) {
	t.Helper()
	got, gotOK := index.FirstFit(extents, want)
	if got != wantIndex || gotOK != wantOK {
		t.Fatalf("FirstFit(%d) = %d, %v, want %d, %v", want, got, gotOK, wantIndex, wantOK)
	}
}

func linearFreeExtentFirstFit(extents []FreeExtent, want uint64) (int, bool) {
	for i := range extents {
		if extents[i].Length >= want {
			return i, true
		}
	}
	return 0, false
}

func freeExtentIndexTestExtents(count int) []FreeExtent {
	extents := make([]FreeExtent, count)
	for i := range extents {
		extents[i] = FreeExtent{
			Offset:            uint64(i+4) * (4 << 10),
			Length:            4 << 10,
			RetiredGeneration: 1,
		}
	}
	return extents
}

func testCountName(count int) string {
	const digits = "0123456789"
	var buffer [20]byte
	next := len(buffer)
	for {
		next--
		buffer[next] = digits[count%10]
		count /= 10
		if count == 0 {
			return string(buffer[next:])
		}
	}
}
