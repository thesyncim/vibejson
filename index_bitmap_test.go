package simdjson

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
	simdkernels "github.com/thesyncim/simdjson/internal/kernels"
)

// The index engine's oracle is the portable builder itself: the engine
// may only shortcut acceptance, so on every input where it produces a
// tape, the builder must accept too and every IndexEntry must be
// byte-identical — start, end, next link, and the packed info word with
// its count, kind, and flags. Where the engine declines, the builder is
// authoritative by construction (BuildIndexOptions falls through to it),
// so declines need no comparison, only coverage assertions on documents
// the engine must take.

// buildIndexReference is BuildIndexOptions' portable section, without
// the engine gate: the fast walk, then the diagnostic parser.
func buildIndexReference(src []byte, storage []IndexEntry) (Index, error) {
	b := tapeBuilder{
		src:      src,
		base:     unsafe.Pointer(unsafe.SliceData(src)),
		entries:  storage[:0],
		parent:   noTapeParent,
		maxDepth: defaultMaxDepth,
	}
	status := b.parseFast()
	switch status {
	case tapeParseOK:
	case tapeParseFull:
		return Index{}, document.ErrIndexFull
	default:
		b.entries = storage[:0]
		b.i = 0
		b.sp = 0
		b.parent = noTapeParent
		if err := b.parse(); err != nil {
			return Index{}, err
		}
	}
	return Index{src: src, entries: b.entries}, nil
}

func TestIndexPositionsFallbackNumberMode(t *testing.T) {
	sample := func(long int) ([]byte, []uint32) {
		var src []byte
		var positions []uint32
		for i := 0; i < 16; i++ {
			positions = append(positions, uint32(len(src)))
			if i < long {
				src = append(src, "12345678"...)
			} else {
				src = append(src, '1')
			}
			src = append(src, ',')
		}
		return src, positions
	}
	src, positions := sample(8)
	meta := simdkernels.Stage1IndexMeta{EmitCount: 512, InStrCount: 512}
	if got := indexPositionsFallbackNumberMode(src, positions, &meta); got != tapeNumberSWAR {
		t.Fatalf("half long heads selected mode %d", got)
	}

	short, shortPositions := sample(0)
	if got := indexPositionsFallbackNumberMode(short, shortPositions, &meta); got != tapeNumberScalar {
		t.Fatalf("short heads selected mode %d", got)
	}

	meta.EmitCount = 511
	if got := indexPositionsFallbackNumberMode(src, positions, &meta); got != tapeNumberScalar {
		t.Fatalf("sparse sample selected mode %d", got)
	}
}

// indexOracleBufs hold reusable generous storage so the mutation battery
// does not allocate per mutant. Generous capacity matters: an engine
// starved of storage aborts Full, which would mask a wrong-accept.
type indexOracleBufs struct {
	mach []IndexEntry
	ref  []IndexEntry
}

func (b *indexOracleBufs) grow(src []byte) {
	need := len(src) + 2
	if cap(b.mach) < need {
		b.mach = make([]IndexEntry, 0, need)
		b.ref = make([]IndexEntry, 0, need)
	}
}

// indexBitmapOracle compares one input. mustAccept additionally requires
// the engine to take the document (coverage: without it, a machine that
// declined everything would pass the differential vacuously).
func indexBitmapOracle(t *testing.T, src []byte, bufs *indexOracleBufs, mustAccept bool, label string) {
	t.Helper()
	bufs.grow(src)
	entries, ok := buildIndexBitmap(src, bufs.mach[:0])
	if !ok {
		if mustAccept {
			t.Fatalf("%s: engine declined a document it must take (len %d)", label, len(src))
		}
		return
	}
	ref, refErr := buildIndexReference(src, bufs.ref[:0])
	if refErr != nil {
		t.Fatalf("%s: engine accepted, builder rejects: %v\n%.200q", label, refErr, src)
	}
	if len(entries) != len(ref.entries) {
		t.Fatalf("%s: %d entries, builder %d\n%.200q", label, len(entries), len(ref.entries), src)
	}
	for i := range entries {
		if entries[i] != ref.entries[i] {
			g, w := entries[i], ref.entries[i]
			t.Fatalf("%s: entry %d = {start %d end %d next %d info %#x}, builder {start %d end %d next %d info %#x}\n%.200q",
				label, i, g.start, g.end, g.next, g.info, w.start, w.end, w.next, w.info, src)
		}
	}
}

// TestIndexBitmapCases pins the tape shape on targeted inputs: member
// counts and next links for nested and empty containers, key and escaped
// flags, integer tagging, literal bodies, and the scalar terminator rule.
func TestIndexBitmapCases(t *testing.T) {
	var bufs indexOracleBufs
	accepted := []string{
		`{}`, `[]`, `5`, `"a"`, `true`, `false`, `null`, `-0.5e+7`,
		`{"a":1}`, `[1,2,3]`, `{"a":{"b":[1,{"c":"d"}]},"e":[]}`,
		`[[[[[]]]]]`, `[{},{}]`, `{"a":[1,2],"b":{"c":3}}`, ` [ 1 , "x" ] `,
		`[["a"],["b"]]`, `[true,false,null]`, `{"k":"v","n":[1,2,3]}`,
		`{"esc":"a\nb","clean":"cd"}`, `["é", "😀", "\\"]`,
		`[0, -1, 1.5, 1e9, -0.25E-3, 12345678901234567890]`,
		`{"":{"":[]}}`, `[[],[],{}]`, `{"a":"` + strings.Repeat("x", 500) + `"}`,
		`["` + strings.Repeat("y", 63) + `\n"]`, `[";"]`,
	}
	for _, src := range accepted {
		indexBitmapOracle(t, []byte(src), &bufs, true, "accept "+src[:min(len(src), 40)])
	}
	rejected := []string{
		``, ` `, `{,}`, `[1 2]`, `{"a" "b"}`, `{"a":}`, `{:1}`, `[,1]`,
		`{"a":1,}`, `[1,]`, `{"a"}`, `"a" "b"`, `1 2`, `{} {}`, `[}`, `{]`,
		`[{]}`, `]`, `}`, `[[]`, `{"a":1`, `[1]]`, `{1:2}`,
		`x`, `[x]`, `{"k":x}`, `[01]`, `[1.]`, `[.5]`, `[-]`, `[+1]`,
		`[1e]`, `[1e+]`, `tru`, `truex`, `[nul]`, `[nullx]`, `12x`, `1x2`,
		`["a]`, `["a\`, `["a\q"]`, `["` + "\x01" + `"]`, `[true false]`,
	}
	for _, src := range rejected {
		indexBitmapOracle(t, []byte(src), &bufs, false, "reject "+src[:min(len(src), 40)])
		// The engine must actually decline these: a tape for an invalid
		// document would be a wrong-accept even if the differential above
		// caught it first.
		bufs.grow([]byte(src))
		if _, ok := buildIndexBitmap([]byte(src), bufs.mach[:0]); ok {
			t.Fatalf("engine accepted invalid %q", src)
		}
	}
}

// TestIndexBitmapDepthCases pins the machine's nesting cap against the
// fast walk's: identical tapes through depth 64, a clean decline past it
// (the fallback diverts to the diagnostic parser, as it always has), and
// kind-mismatched closers.
func TestIndexBitmapDepthCases(t *testing.T) {
	var bufs indexOracleBufs
	nest := func(depth int) string {
		var b strings.Builder
		for i := 0; i < depth; i++ {
			if i%2 == 0 {
				b.WriteString(`[`)
			} else {
				b.WriteString(`{"k":`)
			}
		}
		b.WriteString("0")
		for i := depth - 1; i >= 0; i-- {
			if i%2 == 0 {
				b.WriteString("]")
			} else {
				b.WriteString("}")
			}
		}
		return b.String()
	}
	for _, depth := range []int{1, 2, 62, 63, fastWalkMaxDepth} {
		indexBitmapOracle(t, []byte(nest(depth)), &bufs, true, fmt.Sprintf("depth %d", depth))
	}
	for _, depth := range []int{fastWalkMaxDepth + 1, 200} {
		src := []byte(nest(depth))
		bufs.grow(src)
		if _, ok := buildIndexBitmap(src, bufs.mach[:0]); ok {
			t.Fatalf("engine accepted depth %d past its cap", depth)
		}
	}
	for _, src := range []string{
		strings.Repeat("[", 30) + "}" + strings.Repeat("]", 29),
		strings.Repeat(`{"k":[`, 10) + strings.Repeat("]}", 9) + "]",
		strings.Repeat("]", 300), strings.Repeat("[", 300),
	} {
		indexBitmapOracle(t, []byte(src), &bufs, false, "mismatch")
	}
}

// TestIndexBitmapTestSuite runs the whole JSONTestSuite corpus, plain
// and indentation-wrapped, through the differential.
func TestIndexBitmapTestSuite(t *testing.T) {
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Skip("JSONTestSuite corpus not present")
	}
	var bufs indexOracleBufs
	indent := "\n" + strings.Repeat(" ", 10)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		indexBitmapOracle(t, data, &bufs, false, entry.Name())

		var wrapped bytes.Buffer
		wrapped.WriteString("[")
		for range 8 {
			wrapped.WriteString(indent)
			wrapped.Write(data)
			wrapped.WriteString(",")
		}
		wrapped.WriteString(indent)
		wrapped.Write(data)
		wrapped.WriteString("\n]")
		indexBitmapOracle(t, wrapped.Bytes(), &bufs, false, "wrapped "+entry.Name())
	}
}

// TestIndexBitmapTruncations cuts a mid-size document at every engine
// chunk boundary and a small prefix at every byte.
func TestIndexBitmapTruncations(t *testing.T) {
	doc := buildBitmapTestDocument(t)
	var bufs indexOracleBufs
	for cut := 0; cut <= len(doc); cut += simdkernels.Stage1ChunkBlocks * 64 {
		indexBitmapOracle(t, doc[:cut], &bufs, false, fmt.Sprintf("chunk cut %d", cut))
	}
	small := doc[:2048]
	for cut := 0; cut <= len(small); cut++ {
		indexBitmapOracle(t, small[:cut], &bufs, false, fmt.Sprintf("byte cut %d", cut))
	}
}

// TestIndexBitmapChunkResume carries machine state, the scope slab, and
// the entry cursor across randomized split points: any chunking of the
// same masks must produce the identical tape.
// TestIndexPositionChunkResume carries grammar, quote, scope, and entry state
// across arbitrary position-stream splits. Splits deliberately land between
// opening and closing quotes as well as between ordinary grammar tokens.
func TestIndexPositionChunkResume(t *testing.T) {
	doc := buildBitmapTestDocument(t)
	n := len(doc)
	base := unsafe.Pointer(unsafe.SliceData(doc))
	fullBlocks := n / 64
	positions := make([]uint32, n+128)
	written := 0
	var stream simdkernels.Stage1IndexStream
	for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
		count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
		written += simdkernels.Stage1IndexBlocks(
			(*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &stream, positions[written:],
		)
	}
	if fullBlocks*64 != n {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], doc[fullBlocks*64:])
		written += simdkernels.Stage1IndexBlocks(
			&tail[0], 1, uint32(fullBlocks*64), &stream, positions[written:],
		)
	}
	if stream.Bad || stream.Carry.InString != 0 {
		t.Fatal("stage 1 rejected the resume document")
	}
	positions = positions[:written]

	var refBuf indexOracleBufs
	refBuf.grow(doc)
	ref, err := buildIndexReference(doc, refBuf.ref[:0])
	if err != nil {
		t.Fatal(err)
	}
	for i := range ref.entries {
		ref.entries[i].info &^= uint32(tapeFlagEscaped) << infoFlagsShift
	}

	run := func(splits []int, storage []IndexEntry) ([]IndexEntry, bool) {
		full := storage[:cap(storage)]
		entBase := (*byte)(unsafe.Pointer(unsafe.SliceData(full)))
		var grammar simdkernels.Stage2IndexState
		simdkernels.Stage2IndexReset(&grammar)
		var slab [simdkernels.Stage2IndexSlabLen]uint64
		start := 0
		for _, end := range splits {
			simdkernels.Stage2IndexPositionsFused(
				unsafe.SliceData(doc), n, positions[start:end], &slab, entBase, cap(storage), &grammar,
			)
			if grammar.Bad != 0 {
				return nil, false
			}
			start = end
		}
		if !simdkernels.Stage2IndexFinish(&grammar) {
			return nil, false
		}
		return full[:grammar.EntryOff/16], true
	}

	storage := make([]IndexEntry, 0, len(ref.entries)+8)
	rng := rand.New(rand.NewPCG(47, 53))
	for round := 0; round < 30; round++ {
		var splits []int
		for p := 0; p < len(positions); {
			p = min(p+1+rng.IntN(257), len(positions))
			splits = append(splits, p)
		}
		entries, ok := run(splits, storage)
		if !ok {
			t.Fatalf("round %d: machine declined a valid document", round)
		}
		if len(entries) != len(ref.entries) {
			t.Fatalf("round %d: %d entries, builder %d", round, len(entries), len(ref.entries))
		}
		for i := range entries {
			if entries[i] != ref.entries[i] {
				t.Fatalf("round %d: entry %d differs", round, i)
			}
		}
	}
}

// TestIndexBitmapStorageBounds pins the fail-closed storage contract:
// exactly-sized storage succeeds, one short declines with the Full flag
// before any out-of-bounds write, and the public path maps it to
// document.ErrIndexFull through the fallback.
func TestIndexBitmapStorageBounds(t *testing.T) {
	doc := buildBitmapTestDocument(t)
	need, err := RequiredIndexEntries(doc)
	if err != nil {
		t.Fatal(err)
	}

	exact := make([]IndexEntry, 0, need)
	entries, ok := buildIndexBitmap(doc, exact)
	if !ok || len(entries) != need {
		t.Fatalf("exact storage: ok=%v len=%d want %d", ok, len(entries), need)
	}

	const sentinel = ^uint32(0)
	short := make([]IndexEntry, need-1)
	for i := range short {
		short[i] = IndexEntry{start: sentinel, end: sentinel, next: sentinel, info: sentinel}
	}
	if _, ok := buildIndexBitmap(doc, short[:0]); ok {
		t.Fatal("short storage did not decline")
	}
	if _, err := BuildIndex(doc, make([]IndexEntry, 0, need-1)); err != document.ErrIndexFull {
		t.Fatalf("public short storage: %v, want document.ErrIndexFull", err)
	}

	// Zero-capacity storage must decline without dereferencing anything.
	if _, ok := buildIndexBitmap(doc, nil); ok {
		t.Fatal("nil storage did not decline")
	}
}

// TestIndexBitmapPublicWiring proves the public entry point takes the
// engine on a large committed document and produces the identical index.
func TestIndexBitmapPublicWiring(t *testing.T) {
	doc := buildBitmapTestDocument(t)
	need, err := RequiredIndexEntries(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := buildIndexBitmap(doc, make([]IndexEntry, 0, need)); !ok {
		t.Fatal("engine declined the wiring document")
	}
	idx, err := BuildIndex(doc, make([]IndexEntry, need))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := buildIndexReference(doc, make([]IndexEntry, need))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.entries) != len(ref.entries) {
		t.Fatalf("public %d entries, builder %d", len(idx.entries), len(ref.entries))
	}
	for i := range idx.entries {
		if idx.entries[i] != ref.entries[i] {
			t.Fatalf("public entry %d differs", i)
		}
	}
	// Depth options below the machine's cap must keep the fallback.
	if _, err := BuildIndexOptions(doc, make([]IndexEntry, need), document.IndexOptions{MaxDepth: 8}); err != nil {
		t.Fatal(err)
	}
}

// TestGCCorruptionStage2Index is the standing corruption gate for the
// index machine: concurrent builds under forced stack movement and GC,
// sentinel entries proving the machine never writes past its cursor or
// storage, and retained tapes re-verified after collections. Stress:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionStage2Index -count=5 -cpu=1,4,8 ./
func TestGCCorruptionStage2Index(t *testing.T) {
	doc := buildBitmapTestDocument(t)
	need, err := RequiredIndexEntries(doc)
	if err != nil {
		t.Fatal(err)
	}
	want, err := buildIndexReference(doc, make([]IndexEntry, 0, need))
	if err != nil {
		t.Fatal(err)
	}

	bad := append([]byte(nil), doc...)
	bad[bytes.IndexByte(bad, ':')] = ' '

	const slack = 8
	sentinel := IndexEntry{start: ^uint32(0), end: ^uint32(0), next: ^uint32(0), info: ^uint32(0)}
	workers := runtime.GOMAXPROCS(0) * 2
	iters := 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			storage := make([]IndexEntry, 0, need+slack)
			var retained [][]IndexEntry
			for it := 0; it < iters; it++ {
				forceStackMovement(64+id, it)

				full := storage[:cap(storage)]
				for i := need; i < cap(storage); i++ {
					full[i] = sentinel
				}
				entries, ok := buildIndexBitmap(doc, storage)
				if !ok || len(entries) != len(want.entries) {
					errs <- fmt.Errorf("goroutine %d iter %d: ok=%v len=%d want %d", id, it, ok, len(entries), len(want.entries))
					return
				}
				for i := need; i < cap(storage); i++ {
					if full[i] != sentinel {
						errs <- fmt.Errorf("goroutine %d iter %d: sentinel entry %d overwritten", id, it, i)
						return
					}
				}
				snap := append([]IndexEntry(nil), entries...)
				retained = append(retained, snap)
				if len(retained) > 3 {
					retained = retained[1:]
				}

				if _, ok := buildIndexBitmap(bad, storage); ok {
					errs <- fmt.Errorf("goroutine %d iter %d: invalid document accepted", id, it)
					return
				}

				if it%8 == 0 {
					runtime.GC()
				}
				for _, r := range retained {
					for i := range r {
						if r[i] != want.entries[i] {
							errs <- fmt.Errorf("goroutine %d iter %d: retained entry %d corrupted", id, it, i)
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

// benchmarkIndexEngines interleaves the portable builder and the engine
// on one committed document.
func benchmarkIndexEngines(b *testing.B, doc []byte) {
	need, err := RequiredIndexEntries(doc)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]IndexEntry, 0, need)
	if _, ok := buildIndexBitmap(doc, storage); !ok {
		b.Fatal("engine declined the benchmark document")
	}
	b.Run("fallback", func(b *testing.B) {
		b.SetBytes(int64(len(doc)))
		for i := 0; i < b.N; i++ {
			if _, err := buildIndexReference(doc, storage); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("machine", func(b *testing.B) {
		b.SetBytes(int64(len(doc)))
		for i := 0; i < b.N; i++ {
			if _, ok := buildIndexBitmap(doc, storage); !ok {
				b.Fatal("declined")
			}
		}
	})
}

func BenchmarkBuildIndexBitmapIndent4(b *testing.B) {
	benchmarkIndexEngines(b, buildWhitespaceHeavyDoc(b, "    "))
}

func BenchmarkBuildIndexBitmapNested2(b *testing.B) {
	benchmarkIndexEngines(b, buildNestedTwoSpaceDoc(b))
}
