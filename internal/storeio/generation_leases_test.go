package storeio

import (
	"errors"
	"sync"
	"testing"
)

func TestGenerationLeasesLifecycleAndMinimum(t *testing.T) {
	leases, err := NewGenerationLeases(GenerationLeaseOptions{MaxLeases: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := leases.Acquire(5)
	if err != nil {
		t.Fatal(err)
	}
	second, err := leases.Acquire(7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leases.Acquire(9); !errors.Is(err, ErrLeaseCapacity) {
		t.Fatalf("third Acquire = %v, want %v", err, ErrLeaseCapacity)
	}
	if got := leases.Minimum(10); got != 5 {
		t.Fatalf("Minimum = %d, want 5", got)
	}
	stats := leases.Stats(10)
	if stats.Capacity != 2 || stats.Active != 2 || stats.MinimumGeneration != 5 {
		t.Fatalf("Stats = %+v", stats)
	}
	if err := leases.Close(); !errors.Is(err, ErrLeasesActive) {
		t.Fatalf("Close active = %v, want %v", err, ErrLeasesActive)
	}
	if _, err := leases.Acquire(11); !errors.Is(err, ErrGenerationLeasesClosed) {
		t.Fatalf("Acquire while closing = %v, want %v", err, ErrGenerationLeasesClosed)
	}
	first.Release()
	first.Release()
	if got := leases.Minimum(10); got != 7 {
		t.Fatalf("Minimum after first release = %d, want 7", got)
	}
	second.Release()
	if got := leases.Minimum(10); got != 11 {
		t.Fatalf("Minimum without readers = %d, want 11", got)
	}
	if err := leases.Close(); err != nil {
		t.Fatal(err)
	}
	if err := leases.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationLeasesConcurrent(t *testing.T) {
	leases, err := NewGenerationLeases(GenerationLeaseOptions{MaxLeases: 64})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := range 16 {
		group.Add(1)
		go func(generation uint64) {
			defer group.Done()
			for range 100 {
				lease, acquireErr := leases.Acquire(generation)
				if acquireErr != nil {
					t.Errorf("Acquire: %v", acquireErr)
					return
				}
				if lease.Generation() != generation {
					t.Errorf("Generation = %d, want %d", lease.Generation(), generation)
				}
				lease.Release()
			}
		}(uint64(worker + 1))
	}
	group.Wait()
	if stats := leases.Stats(20); stats.Active != 0 || stats.MinimumGeneration != 21 {
		t.Fatalf("Stats after workers = %+v", stats)
	}
}

func TestGenerationLeaseStaleCopyCannotReleaseReusedSlot(t *testing.T) {
	leases, err := NewGenerationLeases(GenerationLeaseOptions{MaxLeases: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := leases.Acquire(5)
	stale := first
	first.Release()
	second, err := leases.Acquire(5)
	if err != nil {
		t.Fatal(err)
	}
	stale.Release()
	if stats := leases.Stats(5); stats.Active != 1 || stats.MinimumGeneration != 5 {
		t.Fatalf("stale release changed active lease: %+v", stats)
	}
	second.Release()
}

func TestExtentReclaimerRespectsReadersAndRecoveryRoots(t *testing.T) {
	leases, err := NewGenerationLeases(GenerationLeaseOptions{MaxLeases: 2})
	if err != nil {
		t.Fatal(err)
	}
	reclaimer, err := NewExtentReclaimer(leases, ExtentReclaimerOptions{MaxRetiredExtents: 3})
	if err != nil {
		t.Fatal(err)
	}
	reader5, _ := leases.Acquire(5)
	reader7, _ := leases.Acquire(7)
	for generation := uint64(4); generation <= 6; generation++ {
		if err := reclaimer.Retire(FreeExtent{
			Offset: generation * 4096, Length: 4096, RetiredGeneration: generation,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reclaimer.Retire(FreeExtent{Offset: 10 * 4096, Length: 4096, RetiredGeneration: 8}); !errors.Is(err, ErrRetiredExtentCapacity) {
		t.Fatalf("Retire over capacity = %v, want %v", err, ErrRetiredExtentCapacity)
	}

	reusable := make([]FreeExtent, 0, 3)
	reusable = reclaimer.AppendReusable(reusable, 7, 6)
	if len(reusable) != 1 || reusable[0].RetiredGeneration != 4 {
		t.Fatalf("first reusable = %+v, want generation 4", reusable)
	}
	reader5.Release()
	reusable = reclaimer.AppendReusable(reusable[:0], 7, 6)
	if len(reusable) != 1 || reusable[0].RetiredGeneration != 5 {
		t.Fatalf("second reusable = %+v, want generation 5", reusable)
	}
	reader7.Release()
	reusable = reclaimer.AppendReusable(reusable[:0], 7, 7)
	if len(reusable) != 1 || reusable[0].RetiredGeneration != 6 {
		t.Fatalf("third reusable = %+v, want generation 6", reusable)
	}
	if stats := reclaimer.Stats(); stats.Pending != 0 || stats.PendingBytes != 0 {
		t.Fatalf("final Stats = %+v", stats)
	}
}

func TestExtentReclaimerRejectsOverlap(t *testing.T) {
	leases, _ := NewGenerationLeases(GenerationLeaseOptions{MaxLeases: 1})
	reclaimer, _ := NewExtentReclaimer(leases, ExtentReclaimerOptions{MaxRetiredExtents: 2})
	if err := reclaimer.Retire(FreeExtent{Offset: 4096, Length: 8192, RetiredGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	if err := reclaimer.Retire(FreeExtent{Offset: 8192, Length: 4096, RetiredGeneration: 2}); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("overlap = %v, want %v", err, ErrInvalidWrite)
	}
}

func TestExtentReclaimerBatchIsAtomicAndCancelable(t *testing.T) {
	leases, _ := NewGenerationLeases(GenerationLeaseOptions{MaxLeases: 1})
	reclaimer, _ := NewExtentReclaimer(leases, ExtentReclaimerOptions{MaxRetiredExtents: 3})
	batch := []FreeExtent{
		{Offset: 4096, Length: 4096, RetiredGeneration: 4},
		{Offset: 8192, Length: 4096, RetiredGeneration: 4},
	}
	if err := reclaimer.RetireBatch(batch); err != nil {
		t.Fatal(err)
	}
	if stats := reclaimer.Stats(); stats.Pending != 2 {
		t.Fatalf("Stats after batch = %+v", stats)
	}
	if err := reclaimer.RetireBatch([]FreeExtent{
		{Offset: 12288, Length: 4096, RetiredGeneration: 5},
		{Offset: 16384, Length: 4096, RetiredGeneration: 5},
	}); !errors.Is(err, ErrRetiredExtentCapacity) {
		t.Fatalf("over-capacity batch = %v, want %v", err, ErrRetiredExtentCapacity)
	}
	if stats := reclaimer.Stats(); stats.Pending != 2 {
		t.Fatalf("failed batch changed Stats = %+v", stats)
	}
	if err := reclaimer.CancelRetiredGeneration(4); err != nil {
		t.Fatal(err)
	}
	if stats := reclaimer.Stats(); stats.Pending != 0 {
		t.Fatalf("Stats after cancel = %+v", stats)
	}
}

func TestGenerationLeaseAndReclaimerSteadyAllocation(t *testing.T) {
	leases, _ := NewGenerationLeases(GenerationLeaseOptions{MaxLeases: 1})
	reclaimer, _ := NewExtentReclaimer(leases, ExtentReclaimerOptions{MaxRetiredExtents: 1})
	dst := make([]FreeExtent, 0, 1)
	if allocs := testing.AllocsPerRun(1000, func() {
		lease, err := leases.Acquire(2)
		if err != nil {
			panic(err)
		}
		lease.Release()
		if err := reclaimer.Retire(FreeExtent{Offset: 4096, Length: 4096, RetiredGeneration: 1}); err != nil {
			panic(err)
		}
		dst = reclaimer.AppendReusable(dst[:0], 2, 2)
		if len(dst) != 1 {
			panic("extent not reclaimed")
		}
	}); allocs != 0 {
		t.Fatalf("lease/reclaimer steady allocations = %g, want 0", allocs)
	}
}
