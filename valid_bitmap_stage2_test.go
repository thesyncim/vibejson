package simdjson

import (
	"bytes"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

// The stage-2 machine's oracle is the Go walk it replaces: both consume
// identical emit masks in identical chunk runs, so acceptance, rejection,
// AND the chunk at which rejection is decided must all match. The helpers
// below run the two consumers over the same chunking and compare the
// triple (verdict, decided, done-chunk).

// stage2EmitMasks classifies the whole document with the engine's exact
// framing: full blocks straight from the source, the tail block
// space-padded.
func stage2EmitMasks(src []byte) []uint64 {
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	nBlocks := (n + 63) / 64
	fullBlocks := n / 64
	emit := make([]uint64, nBlocks)

	var st simdkernels.Stage1Stream
	var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
	for chunk := 0; chunk < fullBlocks; chunk += simdkernels.Stage1ChunkBlocks {
		cnt := fullBlocks - chunk
		if cnt > simdkernels.Stage1ChunkBlocks {
			cnt = simdkernels.Stage1ChunkBlocks
		}
		simdkernels.Stage1BlocksGP((*byte)(unsafe.Add(base, chunk*64)), cnt, &st, &recs)
		for i := 0; i < cnt; i++ {
			emit[chunk+i] = recs[i].Emit
		}
	}
	if fullBlocks < nBlocks {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		simdkernels.Stage1BlocksGP(&tail[0], 1, &st, &recs)
		emit[fullBlocks] = recs[0].Emit
	}
	return emit
}

// stage2RefRun drives validBitmapWalk over emit in runs of chunkWords.
// doneChunk is the chunk index at which the walk concluded, or -1 when it
// ran to the end; valid is the final verdict either way.
func stage2RefRun(src []byte, emit []uint64, chunkWords int) (valid bool, doneChunk int) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	n := len(src)
	var g vbState
	g.state = vbValue
	for w, ci := 0, 0; w < len(emit); w, ci = w+chunkWords, ci+1 {
		end := min(w+chunkWords, len(emit))
		if v, done := validBitmapWalk(src, base, n, w*64, emit[w:end], nil, &g); done {
			return v, ci
		}
	}
	return g.state == vbDone && g.depth == 0, -1
}

// stage2AsmRunScalars drives the machine over the same chunking with the
// exact validBitmapWalkAsm logic inlined, so recorded scalar positions
// can be captured per chunk before the buffer is reused.
func stage2AsmRunScalars(src []byte, emit []uint64, chunkWords int) (valid bool, doneChunk int, positions []uint32) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	n := len(src)
	var st simdkernels.Stage2State
	simdkernels.Stage2Reset(&st)
	kinds := new([simdkernels.Stage2KindsLen]byte)
	scalars := make([]uint32, 64*chunkWords)
	for w, ci := 0, 0; w < len(emit); w, ci = w+chunkWords, ci+1 {
		end := min(w+chunkWords, len(emit))
		pos := w * 64
		if pos+(end-w)*64 > n {
			for i := end - w - 1; i >= 0; i-- {
				wordBase := pos + i*64
				if wordBase >= n {
					if emit[w+i] != 0 {
						return false, ci, positions
					}
					continue
				}
				if rel := uint(n - wordBase); rel < 64 && emit[w+i]>>rel != 0 {
					return false, ci, positions
				}
				break
			}
		}
		ns := simdkernels.Stage2Walk((*byte)(unsafe.Add(base, pos)), emit[w:end], kinds, scalars, &st)
		for _, rel := range scalars[:ns] {
			positions = append(positions, uint32(pos)+rel)
		}
		if st.Bad != 0 {
			return false, ci, positions
		}
		for _, rel := range scalars[:ns] {
			if !validScalarTokenAt(src, base, n, pos+int(rel)) {
				return false, ci, positions
			}
		}
	}
	return simdkernels.Stage2Finish(&st), -1, positions
}

// stage2ExpectedScalars lists the emit-bit positions whose byte is
// neither structural nor a quote — the scalar starts the machine must
// record, in order.
func stage2ExpectedScalars(src []byte, emit []uint64) []uint32 {
	var out []uint32
	for w, m := range emit {
		for ; m != 0; m &= m - 1 {
			j := w*64 + bits.TrailingZeros64(m)
			switch src[j] {
			case '{', '[', '}', ']', ':', ',', '"':
			default:
				out = append(out, uint32(j))
			}
		}
	}
	return out
}

// stage2Differential is the harness core: identical chunking through both
// consumers, identical (verdict, done-chunk) required, and on acceptance
// the machine's scalar record must equal the classified emit bits.
func stage2Differential(t *testing.T, src []byte, chunkWords int, label string) {
	t.Helper()
	emit := stage2EmitMasks(src)
	refValid, refDone := stage2RefRun(src, emit, chunkWords)
	asmValid, asmDone, positions := stage2AsmRunScalars(src, emit, chunkWords)
	if refValid != asmValid || refDone != asmDone {
		t.Fatalf("%s (chunk %d): machine = (%v, done %d), walk = (%v, done %d)\n%.200q",
			label, chunkWords, asmValid, asmDone, refValid, refDone, src)
	}
	if refValid {
		want := stage2ExpectedScalars(src, emit)
		if len(positions) != len(want) {
			t.Fatalf("%s: machine recorded %d scalar starts, want %d", label, len(positions), len(want))
		}
		for i := range want {
			if positions[i] != want[i] {
				t.Fatalf("%s: scalar start %d = %d, want %d", label, i, positions[i], want[i])
			}
		}
	}
}

func stage2SkipIfUnavailable(tb testing.TB) {
	tb.Helper()
	if !simdkernels.Stage2NativeEnabled() {
		tb.Skip("stage-2 machine not available on this build")
	}
}

// TestStage2WalkGrammarCases pins targeted grammar and scalar edges: pair
// rules, kind matching, depth limits, top-level termination, number and
// literal bodies, and the terminator rule.
func TestStage2WalkGrammarCases(t *testing.T) {
	stage2SkipIfUnavailable(t)
	cases := []string{
		// accepted
		`{}`, `[]`, `5`, `"a"`, `true`, `false`, `null`, `-0.5e+7`,
		`{"a":1}`, `[1,2,3]`, `{"a":{"b":[1,{"c":"d"}]},"e":[]}`,
		`[[[[[]]]]]`, `[{},{}]`, `{"a":[1,2],"b":{"c":3}}`, ` [ 1 , "x" ] `,
		`[["a"],["b"]]`, `[true,false,null]`, `{"k":"v","n":[1,2,3]}`,
		// rejected: pair grammar
		``, ` `, `{,}`, `[1 2]`, `{"a" "b"}`, `{"a":}`, `{:1}`, `[,1]`,
		`{"a":1,}`, `[1,]`, `{"a"}`, `"a" "b"`, `1 2`, `{} {}`, `[] []`,
		`{}[]`, `[]{}`, `1,2`, `{"a":1}:`, `[:]`, `,`, `:`, `[}`, `{]`,
		`[{]}`, `{[}]`, `]`, `}`, `[[]`, `{"a":1`, `[1]]`, `{"a":1}}`,
		`{1:2}`, `[";"]`, `{"a",1}`, `{"a"::1}`, `[[1],`, `["a":1]`,
		`{"a":1 "b":2}`,
		// rejected: scalar bodies and terminators
		`x`, `[x]`, `{"k":x}`, `[01]`, `[1.]`, `[.5]`, `[-]`, `[+1]`,
		`[1e]`, `[1e+]`, `[1.5.5]`, `tru`, `truex`, `[nul]`, `[nullx]`,
		`{"a":falsey}`, `12x`, `[5}`, `[true false]`, `1x2`,
	}
	for _, src := range cases {
		for _, chunkWords := range []int{1, 4} {
			stage2Differential(t, []byte(src), chunkWords, "case "+src[:min(len(src), 40)])
		}
	}
}

// TestStage2WalkDepthCases pins the depth limit, underflow run-on, and
// the kind-slab wrap adversary (indices masked after Bad is set).
func TestStage2WalkDepthCases(t *testing.T) {
	stage2SkipIfUnavailable(t)
	alt := func(depth int) string {
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
	cases := []string{
		strings.Repeat("[", defaultMaxDepth-1) + strings.Repeat("]", defaultMaxDepth-1),
		strings.Repeat("[", defaultMaxDepth) + strings.Repeat("]", defaultMaxDepth),
		strings.Repeat("[", defaultMaxDepth+1) + strings.Repeat("]", defaultMaxDepth+1),
		strings.Repeat("[", 20000),
		strings.Repeat("]", 20000),
		strings.Repeat("]", simdkernels.Stage2KindsLen+1) + `[{"a":1}]`,
		alt(defaultMaxDepth - 1),
		alt(defaultMaxDepth),
		alt(defaultMaxDepth + 1),
		strings.Repeat("[", 30) + "}" + strings.Repeat("]", 29),
		strings.Repeat(`{"k":[`, 40) + strings.Repeat("]}", 39) + "]",
	}
	for _, src := range cases {
		for _, chunkWords := range []int{1, 4, 16} {
			stage2Differential(t, []byte(src), chunkWords, fmt.Sprintf("depth case len %d", len(src)))
		}
	}
}

// TestStage2WalkTestSuite runs the whole JSONTestSuite corpus, plain and
// indentation-wrapped, through the differential.
func TestStage2WalkTestSuite(t *testing.T) {
	stage2SkipIfUnavailable(t)
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Skip("JSONTestSuite corpus not present")
	}
	indent := "\n" + strings.Repeat(" ", 10)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		stage2Differential(t, data, 4, entry.Name())

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
		stage2Differential(t, wrapped.Bytes(), 4, "wrapped "+entry.Name())
	}
}

// TestStage2WalkTruncations cuts a mid-size document at every engine
// chunk boundary, and a small prefix at every byte, comparing the
// consumers on each cut.
func TestStage2WalkTruncations(t *testing.T) {
	stage2SkipIfUnavailable(t)
	doc := buildBitmapTestDocument(t)
	for cut := 0; cut <= len(doc); cut += 4 * 64 {
		stage2Differential(t, doc[:cut], 4, fmt.Sprintf("chunk cut %d", cut))
	}
	small := doc[:2048]
	for cut := 0; cut <= len(small); cut++ {
		stage2Differential(t, small[:cut], 1, fmt.Sprintf("byte cut %d", cut))
		stage2Differential(t, small[:cut], 4, fmt.Sprintf("byte cut %d", cut))
	}
}

// TestStage2WalkChunkResume carries machine state across randomized
// split points: any chunking of the same masks must produce the same
// verdict at the same source position, matching the reference walk run
// with the identical chunking.
func TestStage2WalkChunkResume(t *testing.T) {
	stage2SkipIfUnavailable(t)
	doc := buildBitmapTestDocument(t)
	rng := rand.New(rand.NewPCG(29, 31))

	run := func(src []byte, label string) {
		emit := stage2EmitMasks(src)
		wholeValid, wholeDone, _ := stage2AsmRunScalars(src, emit, max(len(emit), 1))
		for round := 0; round < 25; round++ {
			// One random chunking, applied identically to both consumers.
			var splits []int
			for w := 0; w < len(emit); {
				k := 1 + rng.IntN(9)
				w += k
				splits = append(splits, min(w, len(emit)))
			}
			base := unsafe.Pointer(unsafe.SliceData(src))
			n := len(src)

			var g vbState
			g.state = vbValue
			refValid, refDone := false, -1
			start := 0
			for ci, end := range splits {
				if v, done := validBitmapWalk(src, base, n, start*64, emit[start:end], nil, &g); done {
					refValid, refDone = v, ci
					break
				}
				start = end
			}
			if refDone == -1 {
				refValid = g.state == vbDone && g.depth == 0
			}

			var st simdkernels.Stage2State
			simdkernels.Stage2Reset(&st)
			kinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, 64*10)
			asmValid, asmDone := false, -1
			start = 0
			for ci, end := range splits {
				if v, done := validBitmapWalkAsm(src, base, n, start*64, emit[start:end], &st, kinds, scalars); done {
					asmValid, asmDone = v, ci
					break
				}
				start = end
			}
			if asmDone == -1 {
				asmValid = simdkernels.Stage2Finish(&st)
			}

			if refValid != asmValid || refDone != asmDone {
				t.Fatalf("%s round %d: machine = (%v, done %d), walk = (%v, done %d)",
					label, round, asmValid, asmDone, refValid, refDone)
			}
			if asmDone == -1 && asmValid != wholeValid {
				t.Fatalf("%s round %d: chunked accept %v != whole-run accept %v (whole done %d)",
					label, round, asmValid, wholeValid, wholeDone)
			}
		}
	}

	run(doc, "valid document")
	for i := 0; i < 12; i++ {
		mutated := append([]byte(nil), doc...)
		mutated[rng.IntN(len(mutated))] = byte(rng.IntN(256))
		run(mutated, fmt.Sprintf("mutant %d", i))
	}
}

// TestStage2WalkScalarBufferGuard pins the fail-closed contract: a
// scalars buffer below the emit-bit bound must panic before the machine
// runs, never write out of bounds.
func TestStage2WalkScalarBufferGuard(t *testing.T) {
	stage2SkipIfUnavailable(t)
	defer func() {
		if recover() == nil {
			t.Fatal("undersized scalar buffer did not panic")
		}
	}()
	src := []byte(`[1,2,3]`)
	emit := stage2EmitMasks(src)
	var st simdkernels.Stage2State
	simdkernels.Stage2Reset(&st)
	kinds := new([simdkernels.Stage2KindsLen]byte)
	simdkernels.Stage2Walk(unsafe.SliceData(src), emit, kinds, make([]uint32, 63), &st)
}

// TestGCCorruptionStage2Machine is the standing corruption gate for the
// asm machine: concurrent validations on goroutines that force stack
// movement and GC between iterations, with sentinel checks proving the
// machine never writes outside its buffers and retained scalar records
// re-verified after the fact. The machine is NOSPLIT pure computation —
// no calls out, no pointer stores, every pointer dead at RET — so any
// corruption here would indict the wrapper contracts. Stress form:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionStage2 -count=5 -cpu=1,4,8 ./
func TestGCCorruptionStage2Machine(t *testing.T) {
	stage2SkipIfUnavailable(t)
	doc := buildBitmapTestDocument(t)
	emit := stage2EmitMasks(doc)
	wantValid, wantDone := stage2RefRun(doc, emit, 4)
	wantScalars := stage2ExpectedScalars(doc, emit)

	bad := append([]byte(nil), doc...)
	bad[bytes.IndexByte(bad, ':')] = ' '
	badEmit := stage2EmitMasks(bad)
	badValid, badDone := stage2RefRun(bad, badEmit, 4)

	const sentinel = 0xDEADBEEF
	workers := runtime.GOMAXPROCS(0) * 2
	iters := 60
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			kinds := new([simdkernels.Stage2KindsLen]byte)
			badKinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, 64*4+8)
			for i := range scalars {
				scalars[i] = sentinel
			}
			var retained [][]uint32
			for it := 0; it < iters; it++ {
				forceStackMovement(64+id, it)

				// Valid document: the slab is reused across iterations —
				// a valid walk never touches byte 0, so reuse stays in
				// contract and the assertion below proves it.
				var st simdkernels.Stage2State
				simdkernels.Stage2Reset(&st)
				base := unsafe.Pointer(unsafe.SliceData(doc))
				var got []uint32
				valid, done := true, -1
				for w, ci := 0, 0; w < len(emit); w, ci = w+4, ci+1 {
					end := min(w+4, len(emit))
					pos := w * 64
					ns := simdkernels.Stage2Walk((*byte)(unsafe.Add(base, pos)), emit[w:end], kinds, scalars[:64*4], &st)
					for _, rel := range scalars[:ns] {
						got = append(got, uint32(pos)+rel)
					}
					if st.Bad != 0 {
						valid, done = false, ci
						break
					}
					for _, rel := range scalars[:ns] {
						if !validScalarTokenAt(doc, base, len(doc), pos+int(rel)) {
							valid, done = false, ci
							break
						}
					}
					if done >= 0 {
						break
					}
				}
				if done == -1 {
					valid = simdkernels.Stage2Finish(&st)
				}
				if valid != wantValid || done != wantDone {
					errs <- fmt.Errorf("goroutine %d iter %d: verdict (%v,%d), want (%v,%d)", id, it, valid, done, wantValid, wantDone)
					return
				}
				if len(got) != len(wantScalars) {
					errs <- fmt.Errorf("goroutine %d iter %d: %d scalar starts, want %d", id, it, len(got), len(wantScalars))
					return
				}
				for i := range scalars[64*4:] {
					if scalars[64*4+i] != sentinel {
						errs <- fmt.Errorf("goroutine %d iter %d: sentinel %d overwritten", id, it, i)
						return
					}
				}
				if kinds[0] != 0 {
					errs <- fmt.Errorf("goroutine %d iter %d: kinds[0] dirtied on a valid walk", id, it)
					return
				}
				retained = append(retained, got)
				if len(retained) > 4 {
					retained = retained[1:]
				}

				// Invalid document on its own zeroed slab.
				clear(badKinds[:])
				bv, bd := false, -1
				var bst simdkernels.Stage2State
				simdkernels.Stage2Reset(&bst)
				bbase := unsafe.Pointer(unsafe.SliceData(bad))
				for w, ci := 0, 0; w < len(badEmit); w, ci = w+4, ci+1 {
					end := min(w+4, len(badEmit))
					if v, done := validBitmapWalkAsm(bad, bbase, len(bad), w*64, badEmit[w:end], &bst, badKinds, scalars[:64*4]); done {
						bv, bd = v, ci
						break
					}
				}
				if bd == -1 {
					bv = simdkernels.Stage2Finish(&bst)
				}
				if bv != badValid || bd != badDone {
					errs <- fmt.Errorf("goroutine %d iter %d: bad-doc verdict (%v,%d), want (%v,%d)", id, it, bv, bd, badValid, badDone)
					return
				}

				if it%8 == 0 {
					runtime.GC()
				}
				// Retained records must still read back correct after GCs.
				for _, r := range retained {
					for i := range r {
						if r[i] != wantScalars[i] {
							errs <- fmt.Errorf("goroutine %d iter %d: retained scalar %d = %d, want %d", id, it, i, r[i], wantScalars[i])
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
