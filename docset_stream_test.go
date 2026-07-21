package simdjson

// ReadFrom's contract is stream-equals-loop equivalence: ingesting a stream
// must leave the set exactly as a per-document Append loop over the same
// documents would — same count, same bytes, same entries, same enrichment —
// regardless of separators, read sizes, or where documents land relative to
// arena chunk boundaries. Failure keeps every prior document and leaves the
// set usable. The checks below lean on checkDocSetDifferential, which gates
// every stored document against a fresh standalone build.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
)

// joinDocs concatenates documents with sep, inserting a single space where
// direct concatenation would merge two bare numbers into one token.
func joinDocs(docs []string, sep string) string {
	var sb strings.Builder
	for i, doc := range docs {
		if i > 0 {
			sb.WriteString(sep)
			if sep == "" {
				prev := docs[i-1]
				last := prev[len(prev)-1]
				first := doc[0]
				if (isDigit(last) || last == '.') && (isDigit(first) || first == '-') {
					sb.WriteString(" ")
				}
			}
		}
		sb.WriteString(doc)
	}
	return sb.String()
}

// checkReadFrom ingests stream through ReadFrom (from the given reader) and
// gates the result: full byte count, expected document list, and per-document
// equivalence with standalone builds.
func checkReadFrom(t *testing.T, r io.Reader, streamLen int, docs []string, opts document.IndexOptions, label string) {
	t.Helper()
	var s DocSet
	s.Options = opts
	n, err := s.ReadFrom(r)
	if err != nil {
		t.Fatalf("%s: ReadFrom: %v", label, err)
	}
	if n != int64(streamLen) {
		t.Fatalf("%s: ReadFrom read %d bytes, want %d", label, n, streamLen)
	}
	checkDocSetDifferential(t, &s, docs, label)
}

// TestDocSetReadFromDifferential is the stream-equals-loop gate over the
// adversarial corpus: every separator style, both option variants, and both
// one-shot and 1..7-byte torn reads must reproduce the Append loop exactly.
func TestDocSetReadFromDifferential(t *testing.T) {
	docs := docSetTestCorpus()
	seps := []struct {
		name string
		sep  string
	}{
		{"ndjson", "\n"},
		{"space", " "},
		{"tabs", "\t\t"},
		{"crlf", "\r\n"},
		{"mixed", " \r\n\t "},
		{"concat", ""},
	}
	for _, variant := range docSetOptionVariants() {
		for _, sep := range seps {
			stream := joinDocs(docs, sep.sep)
			label := variant.name + "/" + sep.name
			checkReadFrom(t, strings.NewReader(stream), len(stream), docs, variant.opts, label)
			for _, size := range []int{1, 2, 3, 7} {
				r := &fixedChunkReader{data: []byte(stream), chunk: size}
				checkReadFrom(t, r, len(stream), docs, variant.opts, fmt.Sprintf("%s/read%d", label, size))
			}
			torn := &tornReader{data: []byte(stream), state: uint64(len(stream))*2654435761 | 1}
			checkReadFrom(t, torn, len(stream), docs, variant.opts, label+"/torn")
		}
	}
}

// TestDocSetReadFromChunkStraddle sweeps document positions across source
// chunk boundaries: a growing pad document shifts a fixed follower through
// every offset phase, so documents straddle the 8K/16K/32K chunk edges (and
// the entry-chunk edges) at every alignment. The stream also ends with a bare
// number so the end-of-input boundary is confirmed without a delimiter.
func TestDocSetReadFromChunkStraddle(t *testing.T) {
	follower := `{"id":123,"tags":["a","b"],"nested":{"deep":true}}`
	for _, variant := range docSetOptionVariants() {
		var docs []string
		for pad := 0; pad < 96; pad++ {
			docs = append(docs, fmt.Sprintf(`{"pad":"%s"}`, strings.Repeat("x", 64+pad*7)))
			docs = append(docs, follower)
		}
		docs = append(docs, `42`)
		stream := joinDocs(docs, "\n")
		checkReadFrom(t, strings.NewReader(stream), len(stream), docs, variant.opts, variant.name+"/one-shot")
		r := &fixedChunkReader{data: []byte(stream), chunk: 512}
		checkReadFrom(t, r, len(stream), docs, variant.opts, variant.name+"/read512")
	}
}

// TestDocSetReadFromGiantDoc proves a document larger than the maximum source
// chunk ingests correctly: the partial rolls through geometrically growing
// chunks and the final build routes through the large-document machinery.
func TestDocSetReadFromGiantDoc(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"blob":"`)
	sb.WriteString(strings.Repeat("abcdefgh", 400_000)) // 3.2 MB > docSetMaxSrcChunk
	sb.WriteString(`","tail":[1,2,3]}`)
	giant := sb.String()
	docs := []string{`{"before":1}`, giant, `{"after":2}`}
	stream := joinDocs(docs, "\n")
	checkReadFrom(t, strings.NewReader(stream), len(stream), docs, document.IndexOptions{}, "one-shot")
	r := &fixedChunkReader{data: []byte(stream), chunk: 64 << 10}
	checkReadFrom(t, r, len(stream), docs, document.IndexOptions{HashKeys: true}, "chunked")
}

// TestDocSetReadFromEdgeStreams pins the trivial and near-trivial inputs:
// empty stream, whitespace-only stream, trailing whitespace, and a scalar
// ending exactly at EOF.
func TestDocSetReadFromEdgeStreams(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		docs   []string
	}{
		{"empty", "", nil},
		{"whitespaceOnly", " \n\t\r\n  ", nil},
		{"trailingWhitespace", "{\"a\":1}\n\n\t ", []string{`{"a":1}`}},
		{"numberAtEOF", "{\"a\":1}\n42", []string{`{"a":1}`, `42`}},
		{"literalAtEOF", "true", []string{`true`}},
		{"singleDoc", `{"only":1}`, []string{`{"only":1}`}},
	}
	for _, tc := range cases {
		checkReadFrom(t, strings.NewReader(tc.stream), len(tc.stream), tc.docs, document.IndexOptions{}, tc.name)
		if len(tc.stream) > 0 {
			r := &fixedChunkReader{data: []byte(tc.stream), chunk: 1}
			checkReadFrom(t, r, len(tc.stream), tc.docs, document.IndexOptions{}, tc.name+"/read1")
		}
	}
}

// TestDocSetReadFromMidStreamError proves failure atomicity: a syntax error
// or truncation mid-stream surfaces an offset-carrying error, keeps every
// prior document, and leaves the set fully usable for Append and ReadFrom.
func TestDocSetReadFromMidStreamError(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		kept   []string
	}{
		{"syntax", "{\"a\":1}\n{\"bad\":}\n{\"c\":3}", []string{`{"a":1}`}},
		{"truncated", "{\"a\":1} [1,2,3] {\"b\":", []string{`{"a":1}`, `[1,2,3]`}},
		{"badLiteral", "null nulx", []string{`null`}},
		{"garbageByte", "{\"a\":1} @", []string{`{"a":1}`}},
		{"unterminatedString", `{"a":1} "open`, []string{`{"a":1}`}},
	}
	for _, variant := range docSetOptionVariants() {
		for _, tc := range cases {
			var s DocSet
			s.Options = variant.opts
			_, err := s.ReadFrom(strings.NewReader(tc.stream))
			label := variant.name + "/" + tc.name
			if err == nil {
				t.Fatalf("%s: ReadFrom succeeded, want error", label)
			}
			if !strings.Contains(err.Error(), "input offset") {
				t.Fatalf("%s: error %q carries no input offset", label, err)
			}
			checkDocSetDifferential(t, &s, tc.kept, label)

			// The set must remain fully usable after the failure.
			if _, err := s.Append([]byte(`{"recovered":true}`)); err != nil {
				t.Fatalf("%s: Append after failure: %v", label, err)
			}
			if _, err := s.ReadFrom(strings.NewReader("{\"more\":1}\n7")); err != nil {
				t.Fatalf("%s: ReadFrom after failure: %v", label, err)
			}
			want := append(append([]string{}, tc.kept...), `{"recovered":true}`, `{"more":1}`, `7`)
			checkDocSetDifferential(t, &s, want, label+" after recovery")
		}
	}
}

// errAfterReader yields its data, then a permanent non-EOF error.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestDocSetReadFromReadError proves read errors follow the Reader's
// data-first convention: documents that arrived before the error are kept,
// then the error surfaces — bare at a document boundary, as the wrapped
// cause when the stream broke mid-document.
func TestDocSetReadFromReadError(t *testing.T) {
	cause := errors.New("connection reset")

	var s DocSet
	_, err := s.ReadFrom(&errAfterReader{data: []byte("{\"a\":1}\n{\"b\":2}\n"), err: cause})
	if !errors.Is(err, cause) {
		t.Fatalf("boundary read error = %v, want %v", err, cause)
	}
	checkDocSetDifferential(t, &s, []string{`{"a":1}`, `{"b":2}`}, "boundary")

	var mid DocSet
	_, err = mid.ReadFrom(&errAfterReader{data: []byte("{\"a\":1}\n{\"broken\":"), err: cause})
	if !errors.Is(err, cause) {
		t.Fatalf("mid-document read error = %v, want wrapped %v", err, cause)
	}
	if !strings.Contains(err.Error(), "input offset") {
		t.Fatalf("mid-document read error %q carries no input offset", err)
	}
	checkDocSetDifferential(t, &mid, []string{`{"a":1}`}, "mid-document")
}

// TestDocSetReadFromHandleStability catches arena moves under bulk load:
// handles taken from the first streamed document must survive a hundred
// thousand later documents arriving through ReadFrom, spanning hundreds of
// chunk rolls.
func TestDocSetReadFromHandleStability(t *testing.T) {
	first := `{"id":7,"name":"first","nested":{"deep":[1,2,3]}}`
	var s DocSet
	s.Options = document.IndexOptions{HashKeys: true}
	if _, err := s.ReadFrom(strings.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	doc0 := s.Doc(0)
	raw0 := doc0.Root().Raw().Bytes()
	base0 := unsafe.SliceData(raw0)
	deep, ok, err := doc0.Pointer("/nested/deep/2")
	if !ok || err != nil {
		t.Fatalf("Pointer(/nested/deep/2) = (%v, %v)", ok, err)
	}

	count := testIterations(100_000, 10_000)
	var sb strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&sb, "{\"seq\":%d,\"pad\":\"%s\"}\n", i, strings.Repeat("p", i%53))
	}
	if _, err := s.ReadFrom(strings.NewReader(sb.String())); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1+count {
		t.Fatalf("Len = %d, want %d", s.Len(), 1+count)
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
	last := s.Doc(s.Len() - 1)
	if v, ok := last.Root().Get("seq"); !ok {
		t.Fatal("last document lost its seq member")
	} else if n, ok := v.Int64(); !ok || n != int64(count-1) {
		t.Fatalf("last seq = (%d, %v), want %d", n, ok, count-1)
	}
}

// TestDocSetReadFromAfterAppend proves the two ingestion paths compose: the
// stream picks up in the same arenas Append was filling, and later Appends
// continue after the streamed documents.
func TestDocSetReadFromAfterAppend(t *testing.T) {
	for _, variant := range docSetOptionVariants() {
		var s DocSet
		s.Options = variant.opts
		docs := []string{`{"appended":1}`, `[1,2]`}
		for _, doc := range docs {
			if _, err := s.Append([]byte(doc)); err != nil {
				t.Fatal(err)
			}
		}
		streamed := []string{`{"streamed":true}`, `"mid"`, `{"n":[4,5,6]}`}
		stream := joinDocs(streamed, "\n")
		if _, err := s.ReadFrom(strings.NewReader(stream)); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, streamed...)
		if _, err := s.Append([]byte(`{"tail":9}`)); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, `{"tail":9}`)
		checkDocSetDifferential(t, &s, docs, variant.name)
	}
}

// TestGCCorruptionDocSetReadFrom is the standing corruption gate for stream
// ingestion, whose fast path hands arena tail storage to the tape walker
// through unsafe base pointers. Concurrent workers ingest torn streams under
// forced stack movement and GC while retaining earlier sets, proving
// committed documents never move and never dangle. Stress:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionDocSetReadFrom -count=5 -cpu=1,4,8 ./
func TestGCCorruptionDocSetReadFrom(t *testing.T) {
	// The corpus is padded with filler documents until the stream spans
	// several source chunks, so every iteration exercises chunk rolls — the
	// partial-document move — under GC pressure, not just in-chunk commits.
	docs := docSetTestCorpus()
	for i := 0; i < 400; i++ {
		docs = append(docs, fmt.Sprintf(`{"filler":%d,"pad":"%s"}`, i, strings.Repeat("f", i%211)))
	}
	stream := []byte(joinDocs(docs, "\n"))
	opts := document.IndexOptions{HashKeys: true}

	var wantSet DocSet
	wantSet.Options = opts
	for _, doc := range docs {
		if _, err := wantSet.Append([]byte(doc)); err != nil {
			t.Fatal(err)
		}
	}

	workers := runtime.GOMAXPROCS(0) * 2
	const iters = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var retained []*DocSet
			for it := 0; it < iters; it++ {
				forceStackMovement(48+id, it)
				set := &DocSet{Options: opts}
				in := &tornReader{data: append([]byte(nil), stream...), state: uint64(id*iters+it)*2654435761 | 1}
				if _, err := set.ReadFrom(in); err != nil {
					errs <- fmt.Errorf("worker %d iter %d: ReadFrom: %v", id, it, err)
					return
				}
				if set.Len() != wantSet.Len() {
					errs <- fmt.Errorf("worker %d iter %d: %d docs, want %d", id, it, set.Len(), wantSet.Len())
					return
				}
				for i := 0; i < set.Len(); i++ {
					got, want := set.Doc(i), wantSet.Doc(i)
					if !bytes.Equal(got.src, want.src) {
						errs <- fmt.Errorf("worker %d iter %d: doc %d bytes diverge", id, it, i)
						return
					}
					if len(got.entries) != len(want.entries) {
						errs <- fmt.Errorf("worker %d iter %d: doc %d has %d entries, want %d", id, it, i, len(got.entries), len(want.entries))
						return
					}
					for j := range got.entries {
						if got.entries[j] != want.entries[j] {
							errs <- fmt.Errorf("worker %d iter %d: doc %d entry %d diverges", id, it, i, j)
							return
						}
					}
				}
				retained = append(retained, set)
				if len(retained) > 3 {
					retained = retained[1:]
				}
				if it%4 == 0 {
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
