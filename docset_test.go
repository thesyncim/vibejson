package simdjson

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
)

// The DocSet's contract is arena-backed equivalence: every stored document
// must index byte-identically to a standalone BuildIndexOptions of the same
// bytes, every handle must survive later growth because arena chunks never
// move, and a failed Append must be invisible. Extraction adds one more edge:
// the batch pointer walk shares Node.Get's exact match semantics, so it is
// gated differentially against a per-document PointerCompiled loop.

// docSetTestCorpus returns the adversarial document battery: the key-hash
// corpus, scalars, arrays, a deep nest that takes the diagnostic parser, a
// spill-forcing wide object, and a document large enough for the stage-1/2
// machine route.
func docSetTestCorpus() []string {
	docs := append([]string{}, keyHashCorpus...)
	return append(docs,
		`42`, `"scalar"`, `null`, `true`, `[]`,
		`[1,"two",{"three":3},[4,[5]]]`,
		`{"0":"zero","1":"one","-":"dash"}`,
		strings.Repeat("[", 3000)+strings.Repeat("]", 3000),
		keyHashWideDoc(1000, ""),
		keyHashWideDoc(2000, strings.Repeat("pad", 12)),
	)
}

// docSetOptionVariants pairs each option set under test with a label.
func docSetOptionVariants() []struct {
	name string
	opts document.IndexOptions
} {
	return []struct {
		name string
		opts document.IndexOptions
	}{
		{"default", document.IndexOptions{}},
		{"hashKeys", document.IndexOptions{HashKeys: true}},
	}
}

// checkDocSetDifferential asserts every stored document is byte- and
// entry-identical to a fresh standalone build of the same source under the
// same options.
func checkDocSetDifferential(t *testing.T, s *DocSet, docs []string, label string) {
	t.Helper()
	if s.Len() != len(docs) {
		t.Fatalf("%s: Len = %d, want %d", label, s.Len(), len(docs))
	}
	for i, doc := range docs {
		got := s.Doc(i)
		if string(got.src) != doc {
			t.Fatalf("%s: doc %d source = %.60q, want %.60q", label, i, got.src, doc)
		}
		want, err := BuildIndexOptions([]byte(doc), make([]IndexEntry, len(doc)+2), s.Options)
		if err != nil {
			t.Fatalf("%s: standalone build of doc %d: %v", label, i, err)
		}
		if len(got.entries) != len(want.entries) {
			t.Fatalf("%s: doc %d has %d entries, standalone %d", label, i, len(got.entries), len(want.entries))
		}
		for j := range got.entries {
			if got.entries[j] != want.entries[j] {
				t.Fatalf("%s: doc %d entry %d = %+v, standalone %+v", label, i, j, got.entries[j], want.entries[j])
			}
		}
	}
}

// TestDocSetDifferential is the batch-equals-standalone gate over the corpus,
// under both option variants.
func TestDocSetDifferential(t *testing.T) {
	docs := docSetTestCorpus()
	for _, variant := range docSetOptionVariants() {
		var s DocSet
		s.Options = variant.opts
		for i, doc := range docs {
			ordinal, err := s.Append([]byte(doc))
			if err != nil {
				t.Fatalf("%s: Append(%.60q): %v", variant.name, doc, err)
			}
			if ordinal != i {
				t.Fatalf("%s: Append returned ordinal %d, want %d", variant.name, ordinal, i)
			}
		}
		checkDocSetDifferential(t, &s, docs, variant.name)
	}
}

// TestDocSetHandleStability catches arena moves: handles taken from the first
// document — its Index, a Node deep in it, and raw bytes with their exact
// backing address — must survive a thousand later Appends spanning chunk
// turnover and spill-forcing documents.
func TestDocSetHandleStability(t *testing.T) {
	first := `{"id":7,"name":"first","nested":{"deep":[1,2,3]}}`
	var s DocSet
	s.Options = document.IndexOptions{HashKeys: true}
	if _, err := s.Append([]byte(first)); err != nil {
		t.Fatal(err)
	}
	doc0 := s.Doc(0)
	root := doc0.Root()
	raw0 := root.Raw().Bytes()
	base0 := unsafe.SliceData(raw0)
	deep, ok, err := doc0.Pointer("/nested/deep/2")
	if !ok || err != nil {
		t.Fatalf("Pointer(/nested/deep/2) = (%v, %v)", ok, err)
	}

	appended := []string{first}
	for i := 0; i < 1000; i++ {
		doc := fmt.Sprintf(`{"filler":%d,"pad":"%s"}`, i, strings.Repeat("p", i%257))
		if i%97 == 0 {
			doc = keyHashWideDoc(600, "spill-") // outgrows the entry chunk tail
		}
		if _, err := s.Append([]byte(doc)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		appended = append(appended, doc)
	}

	if string(raw0) != first || unsafe.SliceData(raw0) != base0 {
		t.Fatal("first document's raw bytes moved or changed")
	}
	if got := s.Doc(0).Root().Raw().Bytes(); unsafe.SliceData(got) != base0 {
		t.Fatal("re-fetched first document points at different storage")
	}
	if n, ok := deep.Int64(); !ok || n != 3 {
		t.Fatalf("retained node reads (%d, %v), want 3", n, ok)
	}
	if name, ok := s.Doc(0).Root().Get("name"); !ok || name.Raw().String() != `"first"` {
		t.Fatal("first document lookup broke after growth")
	}
	checkDocSetDifferential(t, &s, appended, "after growth")
}

// TestDocSetFailedAppendUnchanged proves failure atomicity: invalid input of
// every stripe — syntax errors, depth violations, oversized invalid bodies
// that force fresh chunks — must leave length, prior documents, and future
// ordinals untouched.
func TestDocSetFailedAppendUnchanged(t *testing.T) {
	valid := []string{`{"a":1}`, `[1,2,3]`, keyHashWideDoc(64, "")}
	invalid := []string{
		``, ` `, `{`, `{"a":`, `[1,2`, `{"a":1,}`, `tru`, `"unterminated`,
		`[1,2,3]]`, `{"a":1}{"b":2}`,
		`[` + strings.Repeat(`"x",`, 8000) + `]`,                // comma before the closing bracket
		strings.Repeat("[", 40000) + strings.Repeat("]", 39999), // deep and unterminated
	}
	for _, variant := range docSetOptionVariants() {
		var s DocSet
		s.Options = variant.opts
		for _, doc := range valid {
			if _, err := s.Append([]byte(doc)); err != nil {
				t.Fatalf("%s: Append(%.40q): %v", variant.name, doc, err)
			}
		}
		for _, doc := range invalid {
			if _, err := s.Append([]byte(doc)); err == nil {
				t.Fatalf("%s: Append(%.40q) succeeded, want error", variant.name, doc)
			}
			checkDocSetDifferential(t, &s, valid, variant.name+" after failed append")
		}
		ordinal, err := s.Append([]byte(`{"after":"failure"}`))
		if err != nil {
			t.Fatalf("%s: Append after failures: %v", variant.name, err)
		}
		if ordinal != len(valid) {
			t.Fatalf("%s: ordinal after failures = %d, want %d", variant.name, ordinal, len(valid))
		}
		checkDocSetDifferential(t, &s, append(append([]string{}, valid...), `{"after":"failure"}`), variant.name)
	}

	// A depth limit tighter than the document must also fail atomically.
	var s DocSet
	s.Options = document.IndexOptions{MaxDepth: 4}
	if _, err := s.Append([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append([]byte(`[[[[[1]]]]]`)); err == nil {
		t.Fatal("Append past MaxDepth succeeded, want error")
	}
	checkDocSetDifferential(t, &s, []string{`{"a":1}`}, "maxDepth")
}

// docSetPointerBattery returns the pointer expressions the extraction
// differential resolves: hits and misses over the corpus, escaped tokens,
// numeric tokens against both arrays and objects, the dash token, and the
// empty pointer.
func docSetPointerBattery() []string {
	return []string{
		"", "/a", "/b", "/absent", "/dup", "/x", "/x/1/y/a", "/x/1/y/b/0",
		"/a~1b", "/a~0b", "/a~11b", "/outer/inner", "/outer/inner/deep",
		"/member0005", "/member0099", "/very_long_member_key_0002_with_tail",
		"/m\tember0004", "/0", "/1", "/-", "/😀", "/héllo", "/k\n",
		"/member0010/1/nested", "/outer/list/1/z",
	}
}

// TestDocSetAppendPointerDifferential gates the batch extraction against a
// per-document PointerCompiled loop. When the reference resolves every
// document, presence, bytes, and the exact backing address must agree per
// document, with dst's prior contents preserved. When the reference errors on
// some document — an invalid array-index token meeting an array — the batch
// must stop with the same error and return dst truncated to its original
// length. The battery is built so both regimes occur: "/a" alone errors on
// the corpus's array documents and "/member0010/y" on its wide objects.
func TestDocSetAppendPointerDifferential(t *testing.T) {
	docs := docSetTestCorpus()
	battery := append(docSetPointerBattery(), "/member0010/y")
	for _, variant := range docSetOptionVariants() {
		var s DocSet
		s.Options = variant.opts
		for _, doc := range docs {
			if _, err := s.Append([]byte(doc)); err != nil {
				t.Fatalf("%s: Append(%.60q): %v", variant.name, doc, err)
			}
		}
		erroring := 0
		for _, expr := range battery {
			pointer := MustCompilePointer(expr)

			// The reference: one PointerCompiled per document, stopping at
			// the first document that errors.
			var wantValues []RawValue
			var wantErr error
			for i := 0; i < s.Len() && wantErr == nil; i++ {
				want, ok, err := s.Doc(i).PointerCompiled(pointer)
				switch {
				case err != nil:
					wantErr = err
				case ok:
					wantValues = append(wantValues, want.Raw())
				default:
					wantValues = append(wantValues, RawValue{})
				}
			}

			prefix := RawValue{src: []byte("sentinel")}
			got, err := s.AppendPointer([]RawValue{prefix}, pointer)
			if wantErr != nil {
				erroring++
				if err == nil || err.Error() != wantErr.Error() {
					t.Fatalf("%s: AppendPointer(%q) error = %v, reference %v", variant.name, expr, err, wantErr)
				}
				if len(got) != 1 || !bytes.Equal(got[0].Bytes(), prefix.Bytes()) {
					t.Fatalf("%s: failed AppendPointer(%q) returned %d values, want the original prefix", variant.name, expr, len(got))
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s: AppendPointer(%q): %v", variant.name, expr, err)
			}
			if len(got) != 1+s.Len() || !bytes.Equal(got[0].Bytes(), prefix.Bytes()) {
				t.Fatalf("%s: AppendPointer(%q) returned %d values for %d docs", variant.name, expr, len(got), s.Len())
			}
			for i, want := range wantValues {
				extracted := got[1+i]
				if want.Kind() == document.Invalid {
					if extracted.Kind() != document.Invalid || len(extracted.Bytes()) != 0 {
						t.Fatalf("%s: pointer %q doc %d = %q, reference absent", variant.name, expr, i, extracted.Bytes())
					}
					continue
				}
				if !bytes.Equal(extracted.Bytes(), want.Bytes()) {
					t.Fatalf("%s: pointer %q doc %d = %q, reference %q", variant.name, expr, i, extracted.Bytes(), want.Bytes())
				}
				if unsafe.SliceData(extracted.Bytes()) != unsafe.SliceData(want.Bytes()) {
					t.Fatalf("%s: pointer %q doc %d aliases foreign storage", variant.name, expr, i)
				}
			}
		}
		if erroring == 0 {
			t.Fatalf("%s: no battery pointer exercised the error path", variant.name)
		}
	}
}

// TestDocSetAppendPointerEmptySet pins the trivial boundary: extraction over
// an empty set returns dst unchanged.
func TestDocSetAppendPointerEmptySet(t *testing.T) {
	var s DocSet
	dst, err := s.AppendPointer(nil, MustCompilePointer("/a"))
	if err != nil || dst != nil {
		t.Fatalf("AppendPointer on empty set = (%v, %v), want (nil, nil)", dst, err)
	}
}

// TestDocSetSparseGatherDifferential proves the caller-supplied row APIs are
// exact gathers of the dense column APIs across classic, hashed,
// shape-deduplicated, and value-dictionary storage. The row list is
// deliberately non-monotonic and contains a duplicate: order and multiplicity
// belong to the caller, while each resolved cell keeps the dense path's bytes,
// kind, absence verdict, and borrowing lifetime.
func TestDocSetSparseGatherDifferential(t *testing.T) {
	docs := []string{
		`{"a":0,"nested":{"x":"n0"}}`,
		`{"a":1,"a":2,"nested":{"x":"n1"}}`,
		`{"b":3}`,
	}
	for i := 0; i < 24; i++ {
		docs = append(docs, fmt.Sprintf(`{"a":%d,"b":"repeat-value-long","empty":[]}`, i+10))
	}
	rows := []int{len(docs) - 1, 2, 7, 7, 0, 19, 1}
	configs := []struct {
		name       string
		hashKeys   bool
		shapeTapes bool
		valueDict  bool
	}{
		{name: "classic"},
		{name: "hashed", hashKeys: true},
		{name: "shaped", hashKeys: true, shapeTapes: true},
		{name: "shaped+dict", hashKeys: true, shapeTapes: true, valueDict: true},
	}

	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			set := &DocSet{
				Options:    document.IndexOptions{HashKeys: cfg.hashKeys},
				ShapeTapes: cfg.shapeTapes,
				ValueDict:  cfg.valueDict,
			}
			for _, doc := range docs {
				if _, err := set.Append([]byte(doc)); err != nil {
					t.Fatal(err)
				}
			}
			before := set.Stats().Widened

			var denseCache, sparseCache ShapeCache
			for _, field := range []string{"a", "b", "absent"} {
				dense := denseCache.AppendField(nil, set, field)
				sparse := sparseCache.AppendFieldRows(nil, set, rows, field)
				if len(sparse) != len(rows) {
					t.Fatalf("AppendFieldRows(%q) len = %d, want %d", field, len(sparse), len(rows))
				}
				for i, ord := range rows {
					if sparse[i].Kind() != dense[ord].Kind() || !bytes.Equal(sparse[i].Bytes(), dense[ord].Bytes()) {
						t.Fatalf("AppendFieldRows(%q)[%d=>%d] = (%v,%q), dense (%v,%q)",
							field, i, ord, sparse[i].Kind(), sparse[i].Bytes(), dense[ord].Kind(), dense[ord].Bytes())
					}
				}
			}

			for _, expr := range []string{"", "/a", "/b", "/nested/x", "/absent"} {
				pointer := MustCompilePointer(expr)
				dense, err := set.AppendPointer(nil, pointer)
				if err != nil {
					t.Fatalf("AppendPointer(%q): %v", expr, err)
				}
				sparse, err := set.AppendPointerRows(nil, rows, pointer)
				if err != nil {
					t.Fatalf("AppendPointerRows(%q): %v", expr, err)
				}
				for i, ord := range rows {
					if sparse[i].Kind() != dense[ord].Kind() || !bytes.Equal(sparse[i].Bytes(), dense[ord].Bytes()) {
						t.Fatalf("AppendPointerRows(%q)[%d=>%d] = (%v,%q), dense (%v,%q)",
							expr, i, ord, sparse[i].Kind(), sparse[i].Bytes(), dense[ord].Kind(), dense[ord].Bytes())
					}
				}
			}
			if got := set.Stats().Widened; got != before {
				t.Fatalf("sparse gather widened %d shape tapes, want %d", got-before, 0)
			}
		})
	}
}

// TestDocSetSparseGatherSteadyAllocs pins the buffered contract: once the
// caller supplies enough destination capacity, both sparse gather operations
// perform zero steady-state heap allocations, including over narrow shape
// tapes and dictionary-backed values.
func TestDocSetSparseGatherSteadyAllocs(t *testing.T) {
	docs := shapeTapeClusteredDocs(128, 2, 8)
	set := &DocSet{
		Options:    document.IndexOptions{HashKeys: true},
		ShapeTapes: true,
		ValueDict:  true,
	}
	for _, doc := range docs {
		if _, err := set.Append([]byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	rows := make([]int, 0, 16)
	for i := 0; i < set.Len(); i += 8 {
		rows = append(rows, i)
	}
	var cache ShapeCache
	pointer := MustCompilePointer("/s00_f02")
	fields := make([]RawValue, 0, len(rows))
	pointers := make([]RawValue, 0, len(rows))
	fields = cache.AppendFieldRows(fields, set, rows, "s00_f02")
	var err error
	pointers, err = set.AppendPointerRows(pointers, rows, pointer)
	if err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(){
		"AppendFieldRows": func() {
			fields = cache.AppendFieldRows(fields[:0], set, rows, "s00_f02")
		},
		"AppendPointerRows": func() {
			pointers, _ = set.AppendPointerRows(pointers[:0], rows, pointer)
		},
	} {
		if allocs := testing.AllocsPerRun(100, fn); allocs != 0 {
			t.Fatalf("%s with caller-owned destination allocates %v per run", name, allocs)
		}
	}
}

// TestGCCorruptionDocSetMultiDoc is the standing corruption gate for the
// multi-document primitives, whose hot paths read arena bytes through
// byteview views and step tape entries with unsafe offset arithmetic.
// Concurrent workers build sets, intern key identifiers, and extract values
// under forced stack movement and GC while retaining earlier sets, proving
// arena handles never dangle and results stay byte-stable. Stress:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionDocSetMultiDoc -count=5 -cpu=1,4,8 ./
func TestGCCorruptionDocSetMultiDoc(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	docs = append(docs, keyHashWideDoc(96, "value-"), keyHashWideDoc(600, "spill-"))
	pointer := MustCompilePointer("/dup")
	opts := document.IndexOptions{HashKeys: true}

	// The single-threaded expectation every worker must reproduce.
	var wantSet DocSet
	wantSet.Options = opts
	for _, doc := range docs {
		if _, err := wantSet.Append([]byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	var wantInterner KeyInterner
	var wantIDs []uint32
	for i := 0; i < wantSet.Len(); i++ {
		wantIDs = wantInterner.AppendKeyIDs(wantIDs, wantSet.Doc(i))
	}
	wantValues, err := wantSet.AppendPointer(nil, pointer)
	if err != nil {
		t.Fatal(err)
	}

	workers := runtime.GOMAXPROCS(0) * 2
	const iters = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var retained []*DocSet
			ids := make([]uint32, 0, len(wantIDs))
			values := make([]RawValue, 0, len(wantValues))
			for it := 0; it < iters; it++ {
				forceStackMovement(48+id, it)
				set := &DocSet{Options: opts}
				for _, doc := range docs {
					if _, err := set.Append([]byte(doc)); err != nil {
						errs <- fmt.Errorf("worker %d iter %d: Append: %v", id, it, err)
						return
					}
				}
				var interner KeyInterner
				ids = ids[:0]
				for i := 0; i < set.Len(); i++ {
					ids = interner.AppendKeyIDs(ids, set.Doc(i))
				}
				if len(ids) != len(wantIDs) {
					errs <- fmt.Errorf("worker %d iter %d: %d ids, want %d", id, it, len(ids), len(wantIDs))
					return
				}
				for i := range ids {
					if ids[i] != wantIDs[i] {
						errs <- fmt.Errorf("worker %d iter %d: id %d = %d, want %d", id, it, i, ids[i], wantIDs[i])
						return
					}
				}
				var extractErr error
				values, extractErr = set.AppendPointer(values[:0], pointer)
				if extractErr != nil || len(values) != len(wantValues) {
					errs <- fmt.Errorf("worker %d iter %d: extraction (%d values, %v)", id, it, len(values), extractErr)
					return
				}
				for i := range values {
					if !bytes.Equal(values[i].Bytes(), wantValues[i].Bytes()) {
						errs <- fmt.Errorf("worker %d iter %d: value %d = %q, want %q",
							id, it, i, values[i].Bytes(), wantValues[i].Bytes())
						return
					}
				}
				retained = append(retained, set)
				if len(retained) > 3 {
					retained = retained[1:]
				}
				if it%5 == 0 {
					runtime.GC()
				}
				for _, old := range retained {
					for i := 0; i < old.Len(); i++ {
						if string(old.Doc(i).src) != docs[i] {
							errs <- fmt.Errorf("worker %d iter %d: retained doc %d corrupted", id, it, i)
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
