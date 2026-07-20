package simdjson

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
)

// ObjectProbe promises Node.Get semantics — last duplicate wins, escaped keys
// match their decoded spelling — at constant expected cost. These tests hold
// the probe to that contract from three directions: a differential over the
// adversarial key-hash corpus and wide objects on both enriched and plain
// tapes, exact-storage accounting with allocation-free builds and queries,
// and the standing GOGC corruption gate for the offset-derived entry
// pointers.

// checkObjectProbeDifferential builds probes for one object node — with
// exactly sized storage and through the undersized-allocation path — and
// drives both through the query battery against Node.Get, which the key-hash
// differential already holds to the gate-free linear reference.
func checkObjectProbeDifferential(t *testing.T, v Node, label string) {
	t.Helper()
	need := RequiredProbeSlots(v)
	exact, ok := BuildObjectProbe(v, make([]ProbeSlot, need))
	if !ok {
		t.Fatalf("%s: BuildObjectProbe declined an object", label)
	}
	if used := len(exact.table) + len(exact.escaped); used != need {
		t.Fatalf("%s: probe uses %d slots, RequiredProbeSlots said %d", label, used, need)
	}
	grown, ok := BuildObjectProbe(v, nil)
	if !ok {
		t.Fatalf("%s: BuildObjectProbe declined with nil storage", label)
	}
	for _, q := range keyHashQuerySet(v) {
		want, wantOK := v.Get(q)
		for name, probe := range map[string]*ObjectProbe{"exact": &exact, "grown": &grown} {
			got, gotOK := probe.Get(q)
			if gotOK != wantOK || got.entry != want.entry {
				t.Fatalf("%s: %s probe Get(%q) = (%p, %v), Node.Get (%p, %v)",
					label, name, q, got.entry, gotOK, want.entry, wantOK)
			}
		}
	}
}

// TestObjectProbeDifferential is the zero-regression gate for probe lookups:
// on every object of every corpus document, at widths through the wide and
// machine-built ranges, a probe's Get returns entry-identical results to
// Node.Get on both enriched and unenriched tapes.
func TestObjectProbeDifferential(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	for _, width := range []int{1, 2, 8, 32, 128, 512} {
		docs = append(docs, keyHashWideDoc(width, ""))
	}
	// Large enough to take the production stage-1/stage-2 machine route.
	docs = append(docs, keyHashWideDoc(2000, strings.Repeat("pad", 12)))
	for _, hashKeys := range []bool{false, true} {
		for _, doc := range docs {
			src := []byte(doc)
			need, err := RequiredIndexEntries(src)
			if err != nil {
				t.Fatalf("RequiredIndexEntries(%.60q): %v", doc, err)
			}
			tape, err := BuildIndexOptions(src, make([]IndexEntry, need), document.IndexOptions{HashKeys: hashKeys})
			if err != nil {
				t.Fatalf("BuildIndexOptions(%.60q): %v", doc, err)
			}
			for i := range tape.entries {
				if tape.entries[i].Kind() != document.Object {
					continue
				}
				node := Node{src: unsafe.SliceData(tape.src), entry: &tape.entries[i]}
				label := fmt.Sprintf("hashKeys=%v %.40q entry %d", hashKeys, doc, i)
				checkObjectProbeDifferential(t, node, label)
			}
		}
	}
}

// TestObjectProbeNonObject pins the edge verdicts: non-objects decline the
// build and need no storage, an empty object builds a probe that resolves
// nothing, and the zero probe resolves nothing.
func TestObjectProbeNonObject(t *testing.T) {
	src := []byte(`{"arr":[1,2],"str":"s","obj":{}}`)
	tape, err := BuildIndex(src, make([]IndexEntry, len(src)))
	if err != nil {
		t.Fatal(err)
	}
	root := tape.Root()
	for _, key := range []string{"arr", "str"} {
		v, _ := root.Get(key)
		if need := RequiredProbeSlots(v); need != 0 {
			t.Fatalf("RequiredProbeSlots(%s) = %d, want 0", key, need)
		}
		if _, ok := BuildObjectProbe(v, nil); ok {
			t.Fatalf("BuildObjectProbe accepted the %s value", key)
		}
	}
	if _, ok := BuildObjectProbe(Node{}, nil); ok {
		t.Fatal("BuildObjectProbe accepted the zero Node")
	}
	empty, _ := root.Get("obj")
	if need := RequiredProbeSlots(empty); need != 0 {
		t.Fatalf("RequiredProbeSlots(empty object) = %d, want 0", need)
	}
	probe, ok := BuildObjectProbe(empty, nil)
	if !ok {
		t.Fatal("BuildObjectProbe declined the empty object")
	}
	for _, q := range []string{"", "a"} {
		if _, found := probe.Get(q); found {
			t.Fatalf("empty-object probe resolved %q", q)
		}
	}
	var zero ObjectProbe
	if _, found := zero.Get("a"); found {
		t.Fatal("zero probe resolved a key")
	}
}

// TestObjectProbeNoAlloc proves the caller-owned-storage promise: with
// exactly RequiredProbeSlots of storage the build allocates nothing, and Get
// never allocates on hits or misses, escaped keys included.
func TestObjectProbeNoAlloc(t *testing.T) {
	src := []byte(keyHashWideDoc(96, "value-"))
	tape, err := BuildIndex(src, make([]IndexEntry, len(src)))
	if err != nil {
		t.Fatal(err)
	}
	root := tape.Root()
	storage := make([]ProbeSlot, RequiredProbeSlots(root))
	if allocs := testing.AllocsPerRun(20, func() {
		if _, ok := BuildObjectProbe(root, storage); !ok {
			t.Fatal("build declined")
		}
	}); allocs != 0 {
		t.Fatalf("BuildObjectProbe allocated %.1f times with exact storage", allocs)
	}
	probe, _ := BuildObjectProbe(root, storage)
	queries := []string{"member0000", "member0094", "m\tember0004", "absent-key", ""}
	if allocs := testing.AllocsPerRun(20, func() {
		for _, q := range queries {
			probe.Get(q)
		}
	}); allocs != 0 {
		t.Fatalf("ObjectProbe.Get allocated %.1f times", allocs)
	}
}

// TestGCCorruptionObjectProbe is the standing corruption gate for the probe,
// whose slots hold entry offsets that Get turns back into interior pointers
// with unsafe arithmetic. Concurrent builds and queries under forced stack
// movement and GC, with sentinel slots past the exact storage need and
// retained probes revalidated across collections, prove the build never
// writes outside its storage and a probe's derived pointers stay on the live
// tape. Stress:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionObjectProbe -count=5 -cpu=1,4,8 ./
func TestGCCorruptionObjectProbe(t *testing.T) {
	src := []byte(keyHashWideDoc(96, "value-"))
	need, err := RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	opts := document.IndexOptions{HashKeys: true}
	ref, err := BuildIndexOptions(src, make([]IndexEntry, need), opts)
	if err != nil {
		t.Fatal(err)
	}
	queries := keyHashQuerySet(ref.Root())
	probeNeed := RequiredProbeSlots(ref.Root())

	const slack = 8
	sentinel := ProbeSlot{hash: ^uint32(0), off: ^uint32(0)}
	workers := runtime.GOMAXPROCS(0) * 2
	const iters = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tape, err := BuildIndexOptions(src, make([]IndexEntry, need), opts)
			if err != nil {
				errs <- fmt.Errorf("worker %d: build index: %v", id, err)
				return
			}
			root := tape.Root()
			storage := make([]ProbeSlot, probeNeed+slack)
			var retained []ObjectProbe
			for it := 0; it < iters; it++ {
				forceStackMovement(48+id, it)
				for i := probeNeed; i < len(storage); i++ {
					storage[i] = sentinel
				}
				probe, ok := BuildObjectProbe(root, storage[:probeNeed])
				if !ok {
					errs <- fmt.Errorf("worker %d iter %d: build declined", id, it)
					return
				}
				for _, q := range queries {
					got, gotOK := probe.Get(q)
					want, wantOK := root.Get(q)
					if gotOK != wantOK || got.entry != want.entry {
						errs <- fmt.Errorf("worker %d iter %d: Get(%q) mismatch", id, it, q)
						return
					}
				}
				for i := probeNeed; i < len(storage); i++ {
					if storage[i] != sentinel {
						errs <- fmt.Errorf("worker %d iter %d: sentinel %d overwritten", id, it, i)
						return
					}
				}
				// Retain probes in fresh storage so later collections must
				// keep their tape and slots reachable and intact.
				snap, _ := BuildObjectProbe(root, make([]ProbeSlot, probeNeed))
				retained = append(retained, snap)
				if len(retained) > 3 {
					retained = retained[1:]
				}
				if it%8 == 0 {
					runtime.GC()
				}
				for _, r := range retained {
					for _, q := range queries {
						got, gotOK := r.Get(q)
						want, wantOK := root.Get(q)
						if gotOK != wantOK || got.entry != want.entry {
							errs <- fmt.Errorf("worker %d iter %d: retained Get(%q) mismatch", id, it, q)
							return
						}
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
