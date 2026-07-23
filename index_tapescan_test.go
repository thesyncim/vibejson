package slopjson

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/thesyncim/slopjson/document"
)

// The tape scan promises Node.Get semantics — last duplicate wins, escaped
// keys match their decoded spelling, hash collisions never mislead — while
// testing several members per iteration and stopping at the first verified
// match from the end. These tests hold it to that contract from four
// directions: a differential over the adversarial key-hash corpus for the
// kernel on every flat enriched object, a differential for AppendColumn over
// arrays mixing flat, irregular, and non-object elements on enriched and
// plain tapes, poisoned-entry bounds proofs for objects ending exactly at the
// tape's end, and the standing GOGC corruption gate for the kernel's unsafe
// entry walks.

// refAppendColumn is the gate-free reference for AppendColumn: iterate the
// array and resolve each object element with the linear byte-comparison scan.
func refAppendColumn(v Node, key string) ([]RawValue, bool) {
	iter, ok := v.ArrayIter()
	if !ok {
		return nil, false
	}
	var out []RawValue
	for {
		elem, ok := iter.Next()
		if !ok {
			return out, true
		}
		if elem.Kind() != document.Object {
			out = append(out, RawValue{})
			continue
		}
		value, found := refObjectGetLast(elem, key)
		if !found {
			out = append(out, RawValue{})
			continue
		}
		out = append(out, value.Raw())
	}
}

// sameRawValue reports whether two RawValues alias the identical source
// range: same data pointer and length, or both zero.
func sameRawValue(a, b RawValue) bool {
	return len(a.src) == len(b.src) && unsafe.SliceData(a.src) == unsafe.SliceData(b.src)
}

// tapeScanTestDocs is the shared document set: the adversarial corpus plus
// wide flat objects crossing the kernel's four-member block boundaries.
func tapeScanTestDocs() []string {
	docs := append([]string{}, keyHashCorpus...)
	for _, width := range []int{1, 2, 3, 4, 5, 7, 8, 9, 31, 32, 33, 100} {
		docs = append(docs, keyHashWideDoc(width, ""))
	}
	return docs
}

// TestTapeScanFlatDifferential is the kernel's zero-regression gate: on every
// flat object of every enriched corpus tape, tapeScanFlatHash returns the
// entry-identical member the gate-free linear reference finds, across
// escaped, unicode, duplicate, prefix, collision-shaped, and absent queries.
func TestTapeScanFlatDifferential(t *testing.T) {
	for _, doc := range tapeScanTestDocs() {
		src := []byte(doc)
		need, err := RequiredIndexEntries(src)
		if err != nil {
			t.Fatalf("RequiredIndexEntries(%.60q): %v", doc, err)
		}
		tape, err := BuildIndexOptions(src, make([]IndexEntry, need), document.IndexOptions{HashKeys: true})
		if err != nil {
			t.Fatalf("BuildIndexOptions(%.60q): %v", doc, err)
		}
		for i := range tape.entries {
			e := &tape.entries[i]
			count := int(e.Count())
			if e.Kind() != document.Object || count == 0 || e.next != 2*uint32(count)+1 {
				continue
			}
			node := Node{src: unsafe.SliceData(tape.src), entry: e}
			for _, q := range keyHashQuerySet(node) {
				value := tapeScanFlatHash(node.src, e, count, q, hashKeyString(q))
				want, wantOK := refObjectGetLast(node, q)
				if (value != nil) != wantOK || (value != nil && value != want.entry) {
					t.Fatalf("%.40q entry %d: tapeScanFlatHash(%q) = %p, reference (%p, %v)",
						doc, i, q, value, want.entry, wantOK)
				}
			}
		}
	}
}

// columnCorpus is the AppendColumn document set: arrays mixing flat objects,
// span-chased objects with container values, empty objects, duplicate and
// escaped keys whose decoded spellings collide across elements, and
// non-object elements that must yield the zero RawValue.
var columnCorpus = []string{
	`[]`,
	`[1,2,3]`,
	`[{"a":1}]`,
	`[{"a":1},{"a":2,"b":3},{},{"b":4}]`,
	`[{"a":1,"a":2},{"dup":1,"other":2,"dup":3}]`,
	`[{"k\n":1,"k\u000a":2,"kn":3},{"kn":4}]`,
	`[{"abc":1},{"abc":2},{"héllo":3,"héllo":4}]`,
	`[{"a":{"x":1}},{"a":[1,2]},{"a":1},{"x":{"a":9}}]`,
	`[{"a":1},5,"s",null,true,[{"a":9}],{"a":2},{"b":{"a":3}}]`,
	`[{"f00":0,"f01":1,"f02":2,"f03":3,"f04":4,"f05":5,"f06":6,"f07":7,"f08":8}]`,
}

// columnQuerySet unions the per-element query batteries so every element's
// hit, near-miss, and absent shapes are exercised against every element.
func columnQuerySet(v Node) []string {
	set := map[string]struct{}{}
	iter, ok := v.ArrayIter()
	if ok {
		for {
			elem, ok := iter.Next()
			if !ok {
				break
			}
			if elem.Kind() != document.Object {
				continue
			}
			for _, q := range keyHashQuerySet(elem) {
				set[q] = struct{}{}
			}
		}
	}
	set["\x00 definitely absent"] = struct{}{}
	queries := make([]string, 0, len(set))
	for q := range set {
		queries = append(queries, q)
	}
	return queries
}

// TestAppendColumnDifferential is AppendColumn's zero-regression gate: over
// the column corpus and generated arrays of adversarial wide objects, on both
// enriched and plain tapes, every gathered element aliases the identical
// source range the per-element linear reference resolves.
func TestAppendColumnDifferential(t *testing.T) {
	docs := append([]string{}, columnCorpus...)
	for _, width := range []int{3, 8, 33} {
		elem := keyHashWideDoc(width, "")
		docs = append(docs, "["+elem+","+elem+","+elem+"]")
	}
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
			root := tape.Root()
			for _, q := range columnQuerySet(root) {
				got, ok := root.AppendColumn(nil, q)
				want, wantOK := refAppendColumn(root, q)
				if ok != wantOK || len(got) != len(want) {
					t.Fatalf("hashKeys=%v %.60q: AppendColumn(%q) = (%d, %v), reference (%d, %v)",
						hashKeys, doc, q, len(got), ok, len(want), wantOK)
				}
				for i := range got {
					if !sameRawValue(got[i], want[i]) {
						t.Fatalf("hashKeys=%v %.60q: AppendColumn(%q)[%d] = %q, reference %q",
							hashKeys, doc, q, i, got[i].Bytes(), want[i].Bytes())
					}
				}
			}
		}
	}
}

// TestAppendColumnNonArray pins the edge verdicts: non-arrays decline and
// leave dst unchanged, and an empty array extends dst by nothing.
func TestAppendColumnNonArray(t *testing.T) {
	src := []byte(`{"obj":{"a":1},"num":5,"str":"s","empty":[]}`)
	tape, err := BuildIndexOptions(src, make([]IndexEntry, len(src)), document.IndexOptions{HashKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	root := tape.Root()
	prior := []RawValue{{src: src[:1]}}
	for _, key := range []string{"obj", "num", "str"} {
		v, _ := root.Get(key)
		got, ok := v.AppendColumn(prior, "a")
		if ok || len(got) != len(prior) || !sameRawValue(got[0], prior[0]) {
			t.Fatalf("AppendColumn on %s = (%d values, %v), want declined with dst unchanged", key, len(got), ok)
		}
	}
	if _, ok := (Node{}).AppendColumn(nil, "a"); ok {
		t.Fatal("AppendColumn accepted the zero Node")
	}
	empty, _ := root.Get("empty")
	got, ok := empty.AppendColumn(prior, "a")
	if !ok || len(got) != len(prior) {
		t.Fatalf("AppendColumn on empty array = (%d values, %v), want (1, true)", len(got), ok)
	}
}

// TestAppendColumnNoAlloc proves the gather's only allocation is dst growth:
// with capacity in place, hit and miss columns over flat and irregular
// elements allocate nothing.
func TestAppendColumnNoAlloc(t *testing.T) {
	src := []byte(`[{"a":1,"b":2},{"a":{"x":1},"c":3},{"b":4},7,{"a":"s"}]`)
	tape, err := BuildIndexOptions(src, make([]IndexEntry, len(src)), document.IndexOptions{HashKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	root := tape.Root()
	dst := make([]RawValue, 0, 32)
	if allocs := testing.AllocsPerRun(20, func() {
		for _, q := range []string{"a", "b", "absent"} {
			if _, ok := root.AppendColumn(dst, q); !ok {
				t.Fatal("AppendColumn declined an array")
			}
		}
	}); allocs != 0 {
		t.Fatalf("AppendColumn allocated %.1f times with dst capacity in place", allocs)
	}
}

// poisonTapeTail fills the backing entries beyond the built tape with entries
// that would byte-verify as a matching key: their next word holds the query's
// hash, their flags spell an unescaped key, and their source span covers a
// real spelling of the query in src. A scan that reads even one entry past
// its object's extent resolves a phantom later duplicate and diverges from
// the reference, so the differential below doubles as an out-of-bounds proof.
func poisonTapeTail(backing []IndexEntry, used int, keySpan IndexEntry, queryHash uint32) {
	poison := IndexEntry{
		start: keySpan.start,
		end:   keySpan.end,
		next:  queryHash,
		info:  packInfo(0, document.String, tapeFlagKey),
	}
	for i := used; i < len(backing); i++ {
		backing[i] = poison
	}
}

// TestTapeScanEndOfTape proves the scans never read past the entries built
// for the document. Each fixture places a flat enriched object at the very
// end of the tape; the backing array continues with poisoned phantom matches
// that a single out-of-bounds entry read would resolve.
func TestTapeScanEndOfTape(t *testing.T) {
	const key = "q"
	docs := []string{
		`{"q":1}`,
		`{"a":1,"q":2}`,
		`{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"q":8}`,
		`[{"q":1},{"q":2}]`,
		`[7,{"a":1},{"a":2,"q":3}]`,
	}
	const slack = 8
	for _, doc := range docs {
		src := []byte(doc)
		need, err := RequiredIndexEntries(src)
		if err != nil {
			t.Fatalf("RequiredIndexEntries(%q): %v", doc, err)
		}
		backing := make([]IndexEntry, need+slack)
		tape, err := BuildIndexOptions(src, backing[:need:need], document.IndexOptions{HashKeys: true})
		if err != nil {
			t.Fatalf("BuildIndexOptions(%q): %v", doc, err)
		}
		if len(tape.entries) != need {
			t.Fatalf("%q: built %d entries, expected %d", doc, len(tape.entries), need)
		}
		// The poison spans a real "q" key spelling so a phantom read would
		// byte-verify; find one on the tape.
		var keySpan IndexEntry
		for i := range tape.entries {
			e := &tape.entries[i]
			if e.flags()&tapeFlagKey != 0 && string(src[e.start+1:e.end-1]) == key {
				keySpan = *e
				break
			}
		}
		poisonTapeTail(backing, need, keySpan, hashKeyString(key))
		root := tape.Root()
		if root.Kind() == document.Object {
			got, gotOK := root.Get(key)
			want, wantOK := refObjectGetLast(root, key)
			if gotOK != wantOK || got.entry != want.entry {
				t.Fatalf("%q: Get(%q) diverged beside a poisoned tape tail", doc, key)
			}
			continue
		}
		got, ok := root.AppendColumn(nil, key)
		want, wantOK := refAppendColumn(root, key)
		if ok != wantOK || len(got) != len(want) {
			t.Fatalf("%q: AppendColumn(%q) diverged beside a poisoned tape tail", doc, key)
		}
		for i := range got {
			if !sameRawValue(got[i], want[i]) {
				t.Fatalf("%q: AppendColumn(%q)[%d] resolved a phantom entry past the tape", doc, key, i)
			}
		}
	}
}

// TestGCCorruptionTapeScan is the standing corruption gate for the tape scan,
// which walks entries with unsafe pointer arithmetic and loads key words
// while the garbage collector may move stacks. Concurrent column gathers and
// scans under forced stack movement and GC, over tapes whose backing arrays
// end in poisoned phantom matches, prove the kernel never reads past the
// tape, never resolves a stale pointer, and that retained gathered columns
// stay stable across collections. Stress:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionTapeScan -count=5 -cpu=1,4,8 ./
func TestGCCorruptionTapeScan(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 64; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"c00":%d,"c01":%d,"q":%d,"c03":%d}`, i, i, i, i)
	}
	sb.WriteString("]")
	src := []byte(sb.String())
	need, err := RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	opts := document.IndexOptions{HashKeys: true}
	ref, err := BuildIndexOptions(src, make([]IndexEntry, need), opts)
	if err != nil {
		t.Fatal(err)
	}
	queries := []string{"q", "c01", "absent"}
	wants := map[string][]RawValue{}
	for _, q := range queries {
		wants[q], _ = refAppendColumn(ref.Root(), q)
	}
	var qSpan IndexEntry
	for i := range ref.entries {
		e := &ref.entries[i]
		if e.flags()&tapeFlagKey != 0 && string(src[e.start+1:e.end-1]) == "q" {
			qSpan = *e
			break
		}
	}

	const slack = 8
	workers := runtime.GOMAXPROCS(0) * 2
	const iters = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			backing := make([]IndexEntry, need+slack)
			var retained [][]RawValue
			dst := make([]RawValue, 0, 64*len(queries))
			for it := 0; it < iters; it++ {
				forceStackMovement(48+id, it)
				tape, err := BuildIndexOptions(src, backing[:need:need], opts)
				if err != nil {
					errs <- fmt.Errorf("worker %d iter %d: %v", id, it, err)
					return
				}
				poisonTapeTail(backing, need, qSpan, hashKeyString("q"))
				root := tape.Root()
				dst = dst[:0]
				for _, q := range queries {
					mark := len(dst)
					dst, _ = root.AppendColumn(dst, q)
					want := wants[q]
					if len(dst)-mark != len(want) {
						errs <- fmt.Errorf("worker %d iter %d: column %q length %d, want %d", id, it, q, len(dst)-mark, len(want))
						return
					}
					for i, got := range dst[mark:] {
						if !sameRawValue(got, want[i]) {
							errs <- fmt.Errorf("worker %d iter %d: column %q element %d diverged", id, it, q, i)
							return
						}
					}
				}
				column, _ := root.AppendColumn(nil, "q")
				retained = append(retained, column)
				if len(retained) > 3 {
					retained = retained[1:]
				}
				if it%8 == 0 {
					runtime.GC()
				}
				want := wants["q"]
				for _, r := range retained {
					for i := range r {
						if !sameRawValue(r[i], want[i]) {
							errs <- fmt.Errorf("worker %d iter %d: retained column element %d corrupted", id, it, i)
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
