package simdjson

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
	simdkernels "github.com/thesyncim/simdjson/internal/kernels"
)

// Key hashing is opt-in enrichment (document.IndexOptions.HashKeys): the
// default build stays byte-identical to baseline, and the reader consults the
// pre-filter only under an Object header's keys-hashed marker. These tests
// close the correctness holes from four directions: an unenriched index must
// be untouched (every key next == 1), enrichment must store the exact hash of
// every key and mark every Object header, gated lookups must match a gate-free
// linear reference over adversarial objects, and the byte/word/string hashers
// must agree on identical content.

// TestKeyHashByteStringAgreement pins the reader/builder handshake: the query
// side hashes a string, the enrichment side hashes source bytes, and the two
// must produce the identical word for identical content.
func TestKeyHashByteStringAgreement(t *testing.T) {
	vectors := []string{
		"", "a", "ab", "abc", "abcd", "abcde", "abcdef", "abcdefg", "abcdefgh",
		"abcdefghi", "abcdefghijklmnop", "abcdefghijklmnopq",
		strings.Repeat("k", 31), strings.Repeat("k", 32), strings.Repeat("k", 33),
		"héllo", "😀", `back\slash`, `esc\nspelling`, "\x00\x01\x02", "key with spaces",
	}
	for _, v := range vectors {
		if fromBytes, fromString := hashKeyContent([]byte(v)), hashKeyString(v); fromBytes != fromString {
			t.Fatalf("hashKeyContent(%q) = %#x, hashKeyString = %#x", v, fromBytes, fromString)
		}
	}
	// Different lengths of a shared prefix, and single-byte differences at
	// either end, must not collide on these fixed vectors; a systematic
	// collision here would gut the pre-filter.
	distinct := []string{"a", "aa", "aaa", "aaaa", "aaaaaaaa", "aaaaaaab", "baaaaaaa", "ab", "ba", ""}
	seen := map[uint32]string{}
	for _, v := range distinct {
		h := hashKeyString(v)
		if prev, dup := seen[h]; dup {
			t.Fatalf("hash collision between %q and %q", prev, v)
		}
		seen[h] = v
	}
}

// TestKeyHashWordAgreement pins the register variant to the load variant: for
// every content length the enrichment hashes from a word, hashKeyContentWord
// must equal hashKeyContent on the word's low bytes, whatever the bytes beyond
// the content hold.
func TestKeyHashWordAgreement(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 13))
	for round := 0; round < 4096; round++ {
		word := rng.Uint64()
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], word)
		for n := 0; n <= 8; n++ {
			got := hashKeyContentWord(word, n)
			if want := hashKeyContent(buf[:n]); got != want {
				t.Fatalf("hashKeyContentWord(%#016x, %d) = %#x, hashKeyContent = %#x", word, n, got, want)
			}
			if n < 8 {
				// Garbage beyond the content must not influence the hash.
				dirty := word | ^uint64(0)<<(8*n)
				if hashKeyContentWord(dirty, n) != got {
					t.Fatalf("hashKeyContentWord(%#016x, %d) depends on bytes past the content", word, n)
				}
			}
		}
	}
}

// keyHashCorpus is the shared adversarial document set: duplicate keys in both
// flat and span-chased objects, escaped and unicode-escaped spellings whose
// decoded forms collide with raw siblings, shared prefixes across the hash's
// four- and eight-byte tail boundaries, and pointer-escaping metacharacters.
var keyHashCorpus = []string{
	`{}`,
	`{"a":1}`,
	`{"":1,"x":2}`,
	`{"a":1,"b":2,"c":3}`,
	`{"a":1,"ab":2,"abc":3,"abcd":4,"abcde":5,"abcdef":6,"abcdefg":7,"abcdefgh":8,"abcdefghi":9,"abcdefghijklmnop":10,"abcdefghijklmnopq":11}`,
	`{"aaaaaaaa":1,"aaaaaaab":2,"baaaaaaa":3,"ab":4,"ba":5}`,
	`{"a":1,"a":2}`,
	`{"dup":1,"other":{"dup":2},"dup":3}`,
	`{"k\n":1,"k\u000a":2,"kn":3}`,
	`{"\u0061bc":1,"abc":2}`,
	`{"abc":1,"\u0061bc":2}`,
	`{"héllo":1,"h\u00e9llo":2,"hello":3}`,
	`{"a/b":1,"a~b":2,"a~1b":3}`,
	`{"\ud83d\ude00":1,"😀":2}`,
	`{"outer":{"inner":{"deep":1,"deep":2},"list":[{"z":9},{"z":10}]},"outer":{"inner":3}}`,
	`{"x":[1,{"y":{"a":1,"b":[2,3]}},"s"],"x":{"y":4}}`,
}

// keyHashWideDoc builds an object of width members whose keys mix lengths,
// escaped spellings, and duplicates, with an occasional container value so
// the span-chased (non-flat) lookup loop is exercised alongside the flat one.
func keyHashWideDoc(width int, padValue string) string {
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < width; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		switch {
		case i%13 == 12:
			fmt.Fprintf(&sb, `"member%04d":`, i-1) // duplicate of the previous key
		case i%5 == 4:
			fmt.Fprintf(&sb, `"m\tember%04d":`, i) // escaped spelling
		case i%3 == 2:
			fmt.Fprintf(&sb, `"very_long_member_key_%04d_with_tail":`, i)
		default:
			fmt.Fprintf(&sb, `"member%04d":`, i)
		}
		switch {
		case i%11 == 10:
			fmt.Fprintf(&sb, `[%d,{"nested":%d}]`, i, i)
		case padValue != "":
			fmt.Fprintf(&sb, `"%s%d"`, padValue, i)
		default:
			fmt.Fprintf(&sb, "%d", i)
		}
	}
	sb.WriteString("}")
	return sb.String()
}

// checkKeysUnenriched asserts a default build is byte-untouched: every key and
// value string keeps next == 1 and no Object header carries the keys-hashed
// marker.
func checkKeysUnenriched(t *testing.T, entries []IndexEntry, label string) {
	t.Helper()
	for i := range entries {
		e := &entries[i]
		switch {
		case e.Kind() == document.String:
			if e.next != 1 {
				t.Fatalf("%s: string entry %d next = %d, want 1 (unenriched)", label, i, e.next)
			}
		case e.Kind() == document.Object:
			if e.keysHashed() {
				t.Fatalf("%s: Object header %d carries the keys-hashed marker (unenriched)", label, i)
			}
		}
	}
}

// checkKeysEnriched asserts enrichment stored the exact content hash in every
// key entry, left value strings at next == 1, and marked every Object header.
func checkKeysEnriched(t *testing.T, src []byte, entries []IndexEntry, label string) {
	t.Helper()
	keys := 0
	for i := range entries {
		e := &entries[i]
		switch {
		case e.flags()&tapeFlagKey != 0:
			keys++
			content := src[e.start+1 : e.end-1]
			if want := hashKeyContent(content); e.next != want {
				t.Fatalf("%s: key entry %d (%q) next = %#x, want hash %#x", label, i, content, e.next, want)
			}
		case e.Kind() == document.String:
			if e.next != 1 {
				t.Fatalf("%s: value string entry %d next = %d, want 1", label, i, e.next)
			}
		case e.Kind() == document.Object:
			if !e.keysHashed() {
				t.Fatalf("%s: Object header %d missing the keys-hashed marker", label, i)
			}
		}
	}
	if strings.HasPrefix(string(src), `{"`) && keys == 0 {
		t.Fatalf("%s: no key entries found in an object document", label)
	}
}

// buildEnrichedMachine builds a machine tape and enriches it, the opt-in
// counterpart to buildIndexBitmap. It returns false when the machine declines.
func buildEnrichedMachine(src []byte, storage []IndexEntry) (Index, bool) {
	entries, ok := buildIndexBitmap(src, storage)
	if !ok {
		return Index{}, false
	}
	index := Index{src: src, entries: entries}
	enrichKeyHashes(&index)
	return index, true
}

// TestKeyEntryUnenrichedUntouched is the non-enriched regression proof: over
// the adversarial corpus, a default HashKeys-false build stores next == 1 for
// every key on both the portable and the machine tape, and the two remain
// byte-identical (the standing differential must stay green).
func TestKeyEntryUnenrichedUntouched(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	docs = append(docs, keyHashWideDoc(64, ""), keyHashWideDoc(200, "pad-value-"))
	for _, doc := range docs {
		src := []byte(doc)
		tape, err := BuildIndex(src, make([]IndexEntry, len(src)+2))
		if err != nil {
			t.Fatalf("BuildIndex(%.60q): %v", doc, err)
		}
		checkKeysUnenriched(t, tape.entries, "BuildIndex "+doc[:min(len(doc), 30)])

		machine, ok := buildIndexBitmap(src, make([]IndexEntry, 0, len(src)+2))
		if !ok {
			t.Fatalf("machine declined %.60q", doc)
		}
		checkKeysUnenriched(t, machine, "machine "+doc[:min(len(doc), 30)])
		if len(machine) != len(tape.entries) {
			t.Fatalf("%.60q: machine %d entries, portable %d", doc, len(machine), len(tape.entries))
		}
		for i := range machine {
			if machine[i] != tape.entries[i] {
				t.Fatalf("%.60q: unenriched machine entry %d differs from portable", doc, i)
			}
		}
	}
}

// TestKeyEntryHashEnriched proves the enrichment pass stores the identical
// content hash and marker on every build path — the production route (both
// fast walkers and the diagnostic parser via BuildIndexOptions) and the
// stage-2 machine — and that the enriched machine and portable tapes stay
// byte-identical.
func TestKeyEntryHashEnriched(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	docs = append(docs, keyHashWideDoc(64, ""), keyHashWideDoc(200, "pad-value-"))
	for _, doc := range docs {
		src := []byte(doc)
		tape, err := BuildIndexOptions(src, make([]IndexEntry, len(src)+2), document.IndexOptions{HashKeys: true})
		if err != nil {
			t.Fatalf("BuildIndexOptions(%.60q): %v", doc, err)
		}
		checkKeysEnriched(t, src, tape.entries, "BuildIndexOptions "+doc[:min(len(doc), 30)])

		machine, ok := buildEnrichedMachine(src, make([]IndexEntry, 0, len(src)+2))
		if !ok {
			t.Fatalf("machine declined %.60q", doc)
		}
		checkKeysEnriched(t, src, machine.entries, "machine "+doc[:min(len(doc), 30)])
		if len(machine.entries) != len(tape.entries) {
			t.Fatalf("%.60q: enriched machine %d entries, portable %d", doc, len(machine.entries), len(tape.entries))
		}
		for i := range machine.entries {
			if machine.entries[i] != tape.entries[i] {
				t.Fatalf("%.60q: enriched machine entry %d differs from portable", doc, i)
			}
		}
	}
}

// TestGCCorruptionKeyHashEnrich is the standing corruption gate for the
// enrichment pass, which reads source bytes through an unsafe word load while
// patching key entries. Concurrent enriched builds under forced stack movement
// and GC, plus sentinel entries past the tape, prove the pass never writes out
// of the entry slice and that retained enriched tapes stay stable. Stress:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionKeyHashEnrich -count=5 -cpu=1,4,8 ./
func TestGCCorruptionKeyHashEnrich(t *testing.T) {
	src := []byte(keyHashWideDoc(96, "value-"))
	need, err := RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	opts := document.IndexOptions{HashKeys: true}
	want, err := BuildIndexOptions(src, make([]IndexEntry, need), opts)
	if err != nil {
		t.Fatal(err)
	}

	const slack = 8
	sentinel := IndexEntry{start: ^uint32(0), end: ^uint32(0), next: ^uint32(0), info: ^uint32(0)}
	workers := runtime.GOMAXPROCS(0) * 2
	const iters = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			storage := make([]IndexEntry, need+slack)
			var retained [][]IndexEntry
			for it := 0; it < iters; it++ {
				forceStackMovement(48+id, it)
				for i := need; i < len(storage); i++ {
					storage[i] = sentinel
				}
				tape, err := BuildIndexOptions(src, storage[:need], opts)
				if err != nil || len(tape.entries) != len(want.entries) {
					errs <- fmt.Errorf("worker %d iter %d: err=%v len=%d", id, it, err, len(tape.entries))
					return
				}
				for i := range tape.entries {
					if tape.entries[i] != want.entries[i] {
						errs <- fmt.Errorf("worker %d iter %d: entry %d mismatch", id, it, i)
						return
					}
				}
				for i := need; i < len(storage); i++ {
					if storage[i] != sentinel {
						errs <- fmt.Errorf("worker %d iter %d: sentinel %d overwritten", id, it, i)
						return
					}
				}
				retained = append(retained, append([]IndexEntry(nil), tape.entries...))
				if len(retained) > 3 {
					retained = retained[1:]
				}
				if it%8 == 0 {
					runtime.GC()
				}
				for _, r := range retained {
					for i := range r {
						if r[i] != want.entries[i] {
							errs <- fmt.Errorf("worker %d iter %d: retained entry %d corrupted", id, it, i)
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

// refObjectGetLast is the gate-free reference for Node.Get: scan every member
// with the byte comparison alone and keep the last match.
func refObjectGetLast(v Node, key string) (Node, bool) {
	iter, ok := v.ObjectIter()
	if !ok {
		return Node{}, false
	}
	var found Node
	var has bool
	for {
		k, val, ok := iter.Next()
		if !ok {
			return found, has
		}
		if tapeKeyEqual(k.Raw().Bytes(), k.entry.flags(), key) {
			found, has = val, true
		}
	}
}

// refFieldCursor is the gate-free reference for FieldCursor.Find: first match
// at or after the position with a plain byte comparison, wrapping once; a hit
// advances past the member, a miss resets to the first member.
type refFieldCursor struct {
	keys   []Node
	values []Node
	pos    int
}

func newRefFieldCursor(v Node) *refFieldCursor {
	c := &refFieldCursor{}
	iter, ok := v.ObjectIter()
	if !ok {
		return c
	}
	for {
		k, val, ok := iter.Next()
		if !ok {
			return c
		}
		c.keys = append(c.keys, k)
		c.values = append(c.values, val)
	}
}

func (c *refFieldCursor) find(key string) (Node, bool) {
	n := len(c.keys)
	if n == 0 {
		return Node{}, false
	}
	for scanned := 0; scanned < n; scanned++ {
		i := (c.pos + scanned) % n
		k := c.keys[i]
		if tapeKeyEqual(k.Raw().Bytes(), k.entry.flags(), key) {
			c.pos = (i + 1) % n
			return c.values[i], true
		}
	}
	c.pos = 0
	return Node{}, false
}

// keyHashQuerySet returns the deterministic query battery for one object:
// every decoded key plus absent neighbours — extensions, truncations, and
// prefixed variants that shadow real hashes' shapes without matching.
func keyHashQuerySet(v Node) []string {
	set := map[string]struct{}{
		"":                       {},
		"\x00 definitely absent": {},
	}
	iter, ok := v.ObjectIter()
	if ok {
		for {
			k, _, ok := iter.Next()
			if !ok {
				break
			}
			q := nodeKeyString(k)
			set[q] = struct{}{}
			set[q+"x"] = struct{}{}
			set["x"+q] = struct{}{}
			if len(q) > 0 {
				set[q[:len(q)-1]] = struct{}{}
			}
		}
	}
	queries := make([]string, 0, len(set))
	for q := range set {
		queries = append(queries, q)
	}
	sort.Strings(queries)
	return queries
}

// checkObjectLookupDifferential drives the gated lookups on an enriched object
// node against the gate-free references, which iterate the same tape without
// consulting the hash.
func checkObjectLookupDifferential(t *testing.T, v Node, label string) {
	t.Helper()
	queries := keyHashQuerySet(v)
	for _, q := range queries {
		got, gotOK := v.Get(q)
		want, wantOK := refObjectGetLast(v, q)
		if gotOK != wantOK || got.entry != want.entry {
			t.Fatalf("%s: Get(%q) = (%p, %v), reference (%p, %v)",
				label, q, got.entry, gotOK, want.entry, wantOK)
		}
	}
	// The resumable cursor must match the reference query for query across a
	// full pass, then again from the advanced positions later passes leave.
	cursor := v.Fields()
	ref := newRefFieldCursor(v)
	for pass := 0; pass < 3; pass++ {
		for _, q := range queries {
			got, gotOK := cursor.Find(q)
			want, wantOK := ref.find(q)
			if gotOK != wantOK || got.entry != want.entry {
				t.Fatalf("%s: pass %d Find(%q) = (%p, %v), reference (%p, %v)",
					label, pass, q, got.entry, gotOK, want.entry, wantOK)
			}
		}
	}
}

// TestIndexKeyHashLookupDifferential is the zero-regression gate for the hash
// pre-filter: on every object of every corpus document, an enriched index's
// gated Get, Find, and Pointer return entry-identical results to gate-free
// reference scans, on both the walk-built and the machine-built enriched tape.
func TestIndexKeyHashLookupDifferential(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	docs = append(docs,
		keyHashWideDoc(32, ""),
		keyHashWideDoc(400, ""),
		// Large enough to take the production stage-1/stage-2 machine route.
		keyHashWideDoc(2000, strings.Repeat("pad", 12)),
	)
	pointerEscaper := strings.NewReplacer("~", "~0", "/", "~1")
	for _, doc := range docs {
		src := []byte(doc)
		need, err := RequiredIndexEntries(src)
		if err != nil {
			t.Fatalf("RequiredIndexEntries(%.60q): %v", doc, err)
		}
		tapes := map[string]Index{}
		tape, err := BuildIndexOptions(src, make([]IndexEntry, need), document.IndexOptions{HashKeys: true})
		if err != nil {
			t.Fatalf("BuildIndexOptions(%.60q): %v", doc, err)
		}
		tapes["build"] = tape
		// The machine may decline shapes its stage-1 sampling routes to the
		// fallback; must-accept coverage lives in TestKeyEntryHashEnriched.
		if machine, ok := buildEnrichedMachine(src, make([]IndexEntry, 0, need)); ok {
			tapes["machine"] = machine
		}
		for name, tape := range tapes {
			for i := range tape.entries {
				if tape.entries[i].Kind() != document.Object {
					continue
				}
				node := Node{src: unsafe.SliceData(tape.src), entry: &tape.entries[i]}
				checkObjectLookupDifferential(t, node, fmt.Sprintf("%s %.40q entry %d", name, doc, i))
			}
			root := tape.Root()
			if root.Kind() != document.Object {
				continue
			}
			for _, q := range keyHashQuerySet(root) {
				pointer := "/" + pointerEscaper.Replace(q)
				got, gotOK, err := tape.Pointer(pointer)
				if err != nil {
					t.Fatalf("%s: Pointer(%q): %v", name, q, err)
				}
				want, wantOK := refObjectGetLast(root, q)
				if gotOK != wantOK || got.entry != want.entry {
					t.Fatalf("%s %.40q: Pointer(%q) = (%p, %v), reference (%p, %v)",
						name, doc, q, got.entry, gotOK, want.entry, wantOK)
				}
				compiled, compiledOK, err := tape.PointerCompiled(MustCompilePointer(pointer))
				if err != nil {
					t.Fatalf("%s: PointerCompiled(%q): %v", name, q, err)
				}
				if compiledOK != gotOK || compiled.entry != got.entry {
					t.Fatalf("%s %.40q: PointerCompiled(%q) = (%p, %v), Pointer (%p, %v)",
						name, doc, q, compiled.entry, compiledOK, got.entry, gotOK)
				}
			}
		}
	}
}

// TestCompiledKeyLookupDifferential is the alias proof for the compiled-key
// primitive: on every entry of every corpus document — enriched and
// unenriched, objects and wrong kinds alike — GetCompiled(CompileKey(q)) must
// return entry-identical results to Get(q), and a cursor driven through
// FindCompiled must stay in lockstep with one driven through Find across
// multiple passes of the full query battery.
func TestCompiledKeyLookupDifferential(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	docs = append(docs, keyHashWideDoc(32, ""), keyHashWideDoc(400, ""))
	for _, doc := range docs {
		src := []byte(doc)
		need, err := RequiredIndexEntries(src)
		if err != nil {
			t.Fatalf("RequiredIndexEntries(%.60q): %v", doc, err)
		}
		for _, hashKeys := range []bool{false, true} {
			tape, err := BuildIndexOptions(src, make([]IndexEntry, need), document.IndexOptions{HashKeys: hashKeys})
			if err != nil {
				t.Fatalf("BuildIndexOptions(%.60q, HashKeys=%v): %v", doc, hashKeys, err)
			}
			for i := range tape.entries {
				node := Node{src: unsafe.SliceData(tape.src), entry: &tape.entries[i]}
				label := fmt.Sprintf("HashKeys=%v %.40q entry %d", hashKeys, doc, i)
				queries := keyHashQuerySet(node)
				for _, q := range queries {
					got, gotOK := node.GetCompiled(CompileKey(q))
					want, wantOK := node.Get(q)
					if gotOK != wantOK || got.entry != want.entry {
						t.Fatalf("%s: GetCompiled(%q) = (%p, %v), Get (%p, %v)",
							label, q, got.entry, gotOK, want.entry, wantOK)
					}
				}
				if node.Kind() != document.Object {
					continue
				}
				plain := node.Fields()
				compiled := node.Fields()
				for pass := 0; pass < 3; pass++ {
					for _, q := range queries {
						got, gotOK := compiled.FindCompiled(CompileKey(q))
						want, wantOK := plain.Find(q)
						if gotOK != wantOK || got.entry != want.entry {
							t.Fatalf("%s: pass %d FindCompiled(%q) = (%p, %v), Find (%p, %v)",
								label, pass, q, got.entry, gotOK, want.entry, wantOK)
						}
					}
				}
			}
		}
	}
	// The zero Node and zero cursor resolve nothing through either spelling.
	if _, ok := (Node{}).GetCompiled(CompileKey("a")); ok {
		t.Fatal("zero Node resolved a compiled key")
	}
	var zero FieldCursor
	if _, ok := zero.FindCompiled(CompileKey("a")); ok {
		t.Fatal("zero FieldCursor resolved a compiled key")
	}
}

// collectPointerPaths appends prefix and every pointer path reachable from v
// within depth further tokens, plus absent members and malformed array indexes
// at each level, so compiled resolution is compared against string resolution
// over hits, misses, and index errors alike.
func collectPointerPaths(v Node, prefix string, depth int, out *[]string) {
	*out = append(*out, prefix)
	if depth == 0 {
		return
	}
	escaper := strings.NewReplacer("~", "~0", "/", "~1")
	if iter, ok := v.ObjectIter(); ok {
		for {
			k, val, ok := iter.Next()
			if !ok {
				break
			}
			collectPointerPaths(val, prefix+"/"+escaper.Replace(nodeKeyString(k)), depth-1, out)
		}
		*out = append(*out, prefix+"/\x00absent")
	}
	if n, ok := v.ArrayLen(); ok {
		for i := 0; i < n; i++ {
			elem, _ := v.Index(i)
			collectPointerPaths(elem, prefix+"/"+strconv.Itoa(i), depth-1, out)
		}
		*out = append(*out, prefix+"/"+strconv.Itoa(n), prefix+"/-", prefix+"/01", prefix+"/x")
	}
}

// TestCompiledPointerDifferential pins PointerCompiled to Pointer over deep
// paths on enriched and unenriched tapes: for every collected pointer — hits,
// absent members, out-of-range and malformed array indexes — the two must
// agree on target entry, verdict, and error text.
func TestCompiledPointerDifferential(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	docs = append(docs, keyHashWideDoc(32, ""), keyHashWideDoc(400, ""))
	for _, doc := range docs {
		src := []byte(doc)
		need, err := RequiredIndexEntries(src)
		if err != nil {
			t.Fatalf("RequiredIndexEntries(%.60q): %v", doc, err)
		}
		for _, hashKeys := range []bool{false, true} {
			tape, err := BuildIndexOptions(src, make([]IndexEntry, need), document.IndexOptions{HashKeys: hashKeys})
			if err != nil {
				t.Fatalf("BuildIndexOptions(%.60q, HashKeys=%v): %v", doc, hashKeys, err)
			}
			var pointers []string
			collectPointerPaths(tape.Root(), "", 3, &pointers)
			for _, pointer := range pointers {
				want, wantOK, wantErr := tape.Pointer(pointer)
				compiled, err := CompilePointer(pointer)
				if err != nil {
					t.Fatalf("CompilePointer(%q): %v", pointer, err)
				}
				got, gotOK, gotErr := tape.PointerCompiled(compiled)
				if gotOK != wantOK || got.entry != want.entry ||
					(gotErr == nil) != (wantErr == nil) ||
					(gotErr != nil && gotErr.Error() != wantErr.Error()) {
					t.Fatalf("HashKeys=%v %.40q: PointerCompiled(%q) = (%p, %v, %v), Pointer (%p, %v, %v)",
						hashKeys, doc, pointer, got.entry, gotOK, gotErr, want.entry, wantOK, wantErr)
				}
			}
		}
	}
}

// TestIndexKeyHashChunkStraddle sweeps a key across the stage-1 chunk boundary
// so the machine's resumable string path finishes the key in a later chunk.
// The machine tape must stay byte-identical to the portable one (unenriched
// oracle), and after enrichment its gated lookups must resolve every key,
// whatever its length relative to the hash's word boundaries.
func TestIndexKeyHashChunkStraddle(t *testing.T) {
	const chunk = simdkernels.Stage1ChunkBlocks * 64
	var bufs indexOracleBufs
	cases := []struct {
		spelling string // raw spelling between the quotes
		decoded  string // Get query that must resolve
	}{
		{"straddle_key_ABCDEFGHIJKLMNOP", "straddle_key_ABCDEFGHIJKLMNOP"},
		{`stra\nddle_key_ABCDEFGHIJKLMN`, "stra\nddle_key_ABCDEFGHIJKLMN"},
		{`straddle_key_ABCDEFGHIJK`, "straddle_key_ABCDEFGHIJK"},
	}
	for _, tc := range cases {
		span := len(tc.spelling) + 2 // quotes included
		for pad := chunk - span - 16; pad <= chunk+4; pad++ {
			doc := `{"p":"` + strings.Repeat("x", pad) + `","` + tc.spelling + `":1,"z":[2,3]}`
			src := []byte(doc)
			label := fmt.Sprintf("pad %d key %.20q", pad, tc.spelling)
			indexBitmapOracle(t, src, &bufs, true, label)

			machine, ok := buildEnrichedMachine(src, make([]IndexEntry, 0, len(src)))
			if !ok {
				t.Fatalf("%s: machine declined", label)
			}
			checkKeysEnriched(t, src, machine.entries, label)
			value, found := machine.Root().Get(tc.decoded)
			if !found {
				t.Fatalf("%s: straddling key missing", label)
			}
			if n, ok := value.Int64(); !ok || n != 1 {
				t.Fatalf("%s: straddling key value = (%d, %v), want 1", label, n, ok)
			}
			checkObjectLookupDifferential(t, machine.Root(), label)
		}
	}
}
