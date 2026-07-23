package simdjson

import (
	"bytes"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"math/bits"
	"slices"
	"strings"

	"github.com/thesyncim/simdjson/document"
)

// StoreIndexMaxColumns bounds compound exact indexes. Each indexed document
// is extracted with fixed stack storage, so the bound is part of the no-
// transient-allocation maintenance contract rather than an arbitrary parser
// limit. Wider predicates can combine independent indexes with bitmap AND.
const StoreIndexMaxColumns = 4

// StoreIndexDefinition declares one exact scalar index. Paths are RFC 6901
// JSON Pointers. One path creates a column index; two or more create an
// order-sensitive compound key. Missing, unresolvable, and container values
// are omitted; null, booleans, exact JSON numbers, and decoded strings are
// indexed.
type StoreIndexDefinition struct {
	Name  string
	Paths []string
}

var (
	// ErrStoreIndexDefinition reports an empty name, invalid path, or invalid
	// compound width.
	ErrStoreIndexDefinition = errors.New("simdjson: invalid Store index definition")
	// ErrStoreIndexArity reports a lookup whose value count does not match the
	// declared column count.
	ErrStoreIndexArity = errors.New("simdjson: Store index lookup arity mismatch")
	// ErrStoreIndexScalar reports a lookup value that is absent, invalid, or a
	// JSON container. Exact indexes deliberately accept scalars only.
	ErrStoreIndexScalar = errors.New("simdjson: Store exact index requires scalar values")
	// ErrStoreMaskOrder reports a sparse bitmap stream whose chunk ids are not
	// strictly increasing. Ordered masks permit allocation-free merge, lookup,
	// and range execution without copying or sorting caller storage.
	ErrStoreMaskOrder = errors.New("simdjson: Store masks are not strictly ordered")
	// ErrStoreMaskChunk reports a non-zero mask for a chunk absent from the
	// selected snapshot. Failing closed prevents stale or cross-snapshot masks
	// from silently dropping rows.
	ErrStoreMaskChunk = errors.New("simdjson: Store mask chunk is not live")
)

type storeExactIndex struct {
	paths [StoreIndexMaxColumns]CompiledPointer
	specs [StoreIndexMaxColumns]string
	seed  maphash.Seed
	n     uint8
}

type storeIndexSnapshot struct {
	info  StoreIndexInfo
	exact *storeExactIndex
	root  *storeIndexPostingNode
	base  *storePackedIndex
	dirty storeIndexMaskVector
}

// StoreRow is one immutable Snapshot row address. Addresses returned by an
// index are ordered by chunk then stable slot and remain valid only with the
// Snapshot that produced them. The fields are exposed so query workspaces can
// combine candidate masks without converting them to keys.
type StoreRow struct {
	Chunk uint32
	Slot  uint8
}

// StoreMask is one chunk's stable-slot candidate bitmap. Exact index result
// producers return strictly ordered live bits. Range consumers also accept
// dead candidate bits and ignore them, which lets complement plans remain
// metadata-only until the selected document page is admitted.
type StoreMask struct {
	Chunk uint32
	Bits  uint64
}

func compileStoreExactIndex(def StoreIndexDefinition) (*storeExactIndex, error) {
	if def.Name == "" {
		return nil, fmt.Errorf("%w: name is empty", ErrStoreIndexDefinition)
	}
	if len(def.Paths) == 0 || len(def.Paths) > StoreIndexMaxColumns {
		return nil, fmt.Errorf("%w: path count must be in [1,%d]", ErrStoreIndexDefinition, StoreIndexMaxColumns)
	}
	out := &storeExactIndex{n: uint8(len(def.Paths))}
	for i, spec := range def.Paths {
		owned := strings.Clone(spec)
		pointer, err := CompilePointer(owned)
		if err != nil {
			return nil, fmt.Errorf("%w: path %d: %v", ErrStoreIndexDefinition, i, err)
		}
		out.paths[i] = pointer
		out.specs[i] = owned
	}
	return out, nil
}

func storeIndexTupleHash(seed maphash.Seed, values []RawValue) (uint64, bool) {
	h := uint64(postFNVOffset)
	for _, value := range values {
		part, ok := storeIndexRawValueHash(seed, value)
		if !ok {
			return 0, false
		}
		// The delimiter makes tuple composition order-sensitive even when one
		// component's ending state resembles another component's start.
		h = (h ^ 0x9e3779b97f4a7c15) * postFNVPrime
		h = (h ^ part) * postFNVPrime
	}
	// The posting directory consumes low-order radix digits first. FNV is a
	// good streaming content fold but its adjacent small-integer buckets have
	// correlated low bits, which would create needlessly deep HAMT paths for
	// ordinary enum columns. A final bijective avalanche preserves equality
	// while spreading every input bit across the directory address.
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h, true
}

func storeIndexRawValueHash(seed maphash.Seed, v RawValue) (uint64, bool) {
	switch v.Kind() {
	case document.Null:
		return postScalarBucket(postTagNull, 0), true
	case document.Bool:
		value, _ := v.Bool()
		tag := uint64(postTagFalse)
		if value {
			tag = postTagTrue
		}
		return postScalarBucket(tag, 0), true
	case document.Number:
		value, ok := v.Float64()
		if !ok {
			return postScalarBucket(postTagNumberWide, 0), true
		}
		if value == 0 {
			value = 0
		}
		return postScalarBucket(postTagNumber, math.Float64bits(value)), true
	case document.String:
		if content, clean := v.StringBytes(); clean {
			return postScalarBucket(postTagString, maphash.Bytes(seed, content)), true
		}
		var h maphash.Hash
		h.SetSeed(seed)
		it := jsonStringByteIter{raw: v.Bytes()[1 : len(v.Bytes())-1]}
		for {
			b, ok := it.next()
			if !ok {
				return postScalarBucket(postTagString, h.Sum64()), true
			}
			_ = h.WriteByte(b)
		}
	default:
		return 0, false
	}
}

func storeIndexExtract(chunk *storeChunk, slot int, exact *storeExactIndex, out *[StoreIndexMaxColumns]RawValue) (uint64, bool) {
	if !storeIndexExtractValues(chunk, slot, exact, out) {
		return 0, false
	}
	return storeIndexTupleHash(exact.seed, out[:exact.n])
}

func storeIndexExtractValues(chunk *storeChunk, slot int, exact *storeExactIndex, out *[StoreIndexMaxColumns]RawValue) bool {
	if chunk == nil || chunk.live&(uint64(1)<<uint(slot)) == 0 {
		return false
	}
	row := [1]int{int(chunk.ord[slot])}
	for i := 0; i < int(exact.n); i++ {
		var one [1]RawValue
		values, err := chunk.docs.AppendPointerRows(one[:0], row[:], exact.paths[i])
		if err != nil || len(values) != 1 || len(values[0].Bytes()) == 0 {
			return false
		}
		out[i] = values[0]
	}
	return true
}

// storeIndexUpdateSlot moves one stable slot between tuple postings. Equal
// fingerprints require no physical change: exact verification reads the new
// immutable chunk, and even the deliberately coarse wide-number bucket stays
// correct. This makes an update outside the indexed paths metadata-free.
func storeIndexUpdateSlot(root *storeIndexPostingNode, exact *storeExactIndex, chunkID uint32, old, next *storeChunk, slot int) *storeIndexPostingNode {
	var oldValues, nextValues [StoreIndexMaxColumns]RawValue
	oldHash, oldOK := storeIndexExtract(old, slot, exact, &oldValues)
	nextHash, nextOK := storeIndexExtract(next, slot, exact, &nextValues)
	if oldOK && nextOK && oldHash == nextHash {
		return root
	}
	bit := uint64(1) << uint(slot)
	if oldOK {
		root = storeIndexPostingSet(root, oldHash, chunkID, bit, false)
	}
	if nextOK {
		root = storeIndexPostingSet(root, nextHash, chunkID, bit, true)
	}
	return root
}

func storeIndexSetChunk(root *storeIndexPostingNode, exact *storeExactIndex, chunkID uint32, chunk *storeChunk, present bool) *storeIndexPostingNode {
	var storage [storeMaxChunkDocuments]storeIndexHashMask
	entries := storeIndexCollectChunk(storage[:0], exact, chunk)
	for _, entry := range entries {
		root = storeIndexPostingSetMask(root, entry.hash, chunkID, entry.mask, present)
	}
	return root
}

type storeIndexHashMask struct {
	hash uint64
	mask uint64
}

func storeIndexCollectChunk(dst []storeIndexHashMask, exact *storeExactIndex, chunk *storeChunk) []storeIndexHashMask {
	if chunk == nil {
		return dst
	}
	for live := chunk.live; live != 0; live &= live - 1 {
		slot := bits.TrailingZeros64(live)
		var values [StoreIndexMaxColumns]RawValue
		hash, ok := storeIndexExtract(chunk, slot, exact, &values)
		if ok {
			dst = append(dst, storeIndexHashMask{hash: hash, mask: uint64(1) << uint(slot)})
		}
	}
	slices.SortFunc(dst, func(a, b storeIndexHashMask) int {
		switch {
		case a.hash < b.hash:
			return -1
		case a.hash > b.hash:
			return 1
		default:
			return 0
		}
	})
	out := dst[:0]
	for first := 0; first < len(dst); {
		last := first + 1
		mask := dst[first].mask
		for last < len(dst) && dst[last].hash == dst[first].hash {
			mask |= dst[last].mask
			last++
		}
		out = append(out, storeIndexHashMask{hash: dst[first].hash, mask: mask})
		first = last
	}
	return out
}

func storeIndexBuildBulk(pending map[uint64][]storeIndexChunkMask) *storeIndexPostingNode {
	leaves := make([]*storeIndexPostingLeaf, 0, len(pending))
	for hash, entries := range pending {
		leaves = append(leaves, &storeIndexPostingLeaf{
			hash:  hash,
			masks: storeIndexMasksFromSorted(entries),
		})
	}
	slices.SortFunc(leaves, func(a, b *storeIndexPostingLeaf) int {
		// The HAMT consumes low radix digits first. Bit-reversed order makes
		// every successive low-bit group contiguous for the one-allocation
		// recursive builder below.
		ah, bh := bits.Reverse64(a.hash), bits.Reverse64(b.hash)
		switch {
		case ah < bh:
			return -1
		case ah > bh:
			return 1
		default:
			return 0
		}
	})
	return storeIndexPostingBuild(leaves, 0)
}

func storeIndexApplyBulk(root *storeIndexPostingNode, pending map[uint64][]storeIndexChunkMask) *storeIndexPostingNode {
	for hash, entries := range pending {
		masks, _ := storeIndexPostingLookup(root, hash)
		masks = storeIndexMergeBulkMasks(masks, entries)
		root = storeIndexPostingInsert(root, 0, &storeIndexPostingLeaf{hash: hash, masks: masks})
	}
	return root
}

func storeIndexMergeBulkMasks(current storeIndexMasks, changes []storeIndexChunkMask) storeIndexMasks {
	n := int(current.n) + int(current.wide.words) + len(changes)
	var local [storeMaxChunkDocuments]storeIndexChunkMask
	entries := local[:0]
	if n > len(local) {
		entries = make([]storeIndexChunkMask, 0, n)
	}
	current.each(func(chunk uint32, mask uint64) bool {
		entries = append(entries, storeIndexChunkMask{chunk: chunk, mask: mask})
		return true
	})
	entries = append(entries, changes...)
	slices.SortFunc(entries, func(a, b storeIndexChunkMask) int {
		switch {
		case a.chunk < b.chunk:
			return -1
		case a.chunk > b.chunk:
			return 1
		default:
			return 0
		}
	})
	out := entries[:0]
	for first := 0; first < len(entries); {
		last := first + 1
		mask := entries[first].mask
		for last < len(entries) && entries[last].chunk == entries[first].chunk {
			mask |= entries[last].mask
			last++
		}
		out = append(out, storeIndexChunkMask{chunk: entries[first].chunk, mask: mask})
		first = last
	}
	return storeIndexMasksFromSorted(out)
}

func storeIndexScalarEqual(a, b RawValue) bool {
	kind := a.Kind()
	if kind != b.Kind() {
		return false
	}
	switch kind {
	case document.Null:
		return true
	case document.Bool:
		av, _ := a.Bool()
		bv, _ := b.Bool()
		return av == bv
	case document.Number:
		return jsonNumberEqual(a.Bytes(), b.Bytes())
	case document.String:
		aRaw, bRaw := a.Bytes(), b.Bytes()
		var af, bf uint8
		if bytes.IndexByte(aRaw, '\\') >= 0 {
			af = tapeFlagEscaped
		}
		if bytes.IndexByte(bRaw, '\\') >= 0 {
			bf = tapeFlagEscaped
		}
		return rawJSONStringEqual(aRaw, af, bRaw, bf)
	default:
		return false
	}
}

func storeIndexSlotEqual(chunk *storeChunk, slot int, exact *storeExactIndex, want []RawValue) bool {
	var got [StoreIndexMaxColumns]RawValue
	if !storeIndexExtractValues(chunk, slot, exact, &got) {
		return false
	}
	for i := 0; i < int(exact.n); i++ {
		if !storeIndexScalarEqual(got[i], want[i]) {
			return false
		}
	}
	return true
}

func (s Snapshot) exactIndex(name string) (storeIndexSnapshot, bool) {
	if s.state == nil {
		return storeIndexSnapshot{}, false
	}
	lo, hi := 0, len(s.state.secondary)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if s.state.secondary[mid].info.Name < name {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(s.state.secondary) || s.state.secondary[lo].info.Name != name || s.state.secondary[lo].exact == nil {
		return storeIndexSnapshot{}, false
	}
	return s.state.secondary[lo], true
}

func (s Snapshot) visitIndexMatches(name string, values []Index, visit func(uint32, *storeChunk, int)) error {
	index, ok := s.exactIndex(name)
	if !ok {
		return ErrStoreIndexNotFound
	}
	if len(values) != int(index.exact.n) {
		return ErrStoreIndexArity
	}
	var want [StoreIndexMaxColumns]RawValue
	for i := range values {
		root := values[i].Root()
		if _, scalar := postValueHash(root); !scalar {
			return ErrStoreIndexScalar
		}
		want[i] = root.Raw()
	}
	hash, _ := storeIndexTupleHash(index.exact.seed, want[:index.exact.n])
	if index.info.State != StoreIndexReady {
		s.state.chunks.each(func(chunkID uint32, chunk *storeChunk) bool {
			for live := chunk.live; live != 0; live &= live - 1 {
				slot := bits.TrailingZeros64(live)
				if storeIndexSlotEqual(chunk, slot, index.exact, want[:index.exact.n]) {
					visit(chunkID, chunk, slot)
				}
			}
			return true
		})
		return nil
	}
	storeIndexEachCandidate(index, hash, func(chunkID uint32, candidates uint64) {
		chunk := s.state.chunks.get(chunkID)
		if chunk == nil {
			return
		}
		for live := candidates & chunk.live; live != 0; live &= live - 1 {
			slot := bits.TrailingZeros64(live)
			if storeIndexSlotEqual(chunk, slot, index.exact, want[:index.exact.n]) {
				visit(chunkID, chunk, slot)
			}
		}
	})
	return nil
}

// storeIndexEachCandidate merges the immutable packed base with the
// path-copied mutation delta in chunk order. A dirty chunk is wholly shadowed:
// its complete current postings live in root, while every old base word for
// that chunk is skipped. This keeps writes O(one bounded chunk) and avoids a
// corpus rebuild on the first mutation.
func storeIndexEachCandidate(index storeIndexSnapshot, hash uint64, visit func(uint32, uint64)) {
	delta, _ := storeIndexPostingLookup(index.root, hash)
	deltaChunk, deltaMask, deltaOK := delta.next(0)
	advanceDelta := func() {
		if deltaChunk == ^uint32(0) {
			deltaOK = false
			return
		}
		deltaChunk, deltaMask, deltaOK = delta.next(uint64(deltaChunk) + 1)
	}
	if index.base != nil {
		index.base.each(hash, func(baseChunk uint32, baseMask uint64) bool {
			for deltaOK && deltaChunk < baseChunk {
				visit(deltaChunk, deltaMask)
				advanceDelta()
			}
			if deltaOK && deltaChunk == baseChunk {
				visit(deltaChunk, deltaMask)
				advanceDelta()
				return true
			}
			if index.dirty.get(baseChunk) == 0 {
				visit(baseChunk, baseMask)
			}
			return true
		})
	}
	for deltaOK {
		visit(deltaChunk, deltaMask)
		advanceDelta()
	}
}

// AppendIndexRows appends immutable row addresses exactly matching the scalar
// values of a declared single-column or compound index. Each Index must have a
// scalar root. With sufficient dst capacity, the lookup and exact collision
// recheck allocate nothing. A Building index remains correct by scanning the
// snapshot; a Ready index visits only its stable-slot bitmap candidates.
func (s Snapshot) AppendIndexRows(dst []StoreRow, name string, values ...Index) ([]StoreRow, error) {
	err := s.visitIndexMatches(name, values, func(chunkID uint32, _ *storeChunk, slot int) {
		dst = append(dst, StoreRow{Chunk: chunkID, Slot: uint8(slot)})
	})
	return dst, err
}

// AppendIndexMasks appends exact matches in their native chunk bitmap form.
// Adjacent matches in one chunk coalesce into one word. With sufficient dst
// capacity the complete lookup allocates nothing.
func (s Snapshot) AppendIndexMasks(dst []StoreMask, name string, values ...Index) ([]StoreMask, error) {
	err := s.visitIndexMatches(name, values, func(chunkID uint32, _ *storeChunk, slot int) {
		bit := uint64(1) << uint(slot)
		if len(dst) != 0 && dst[len(dst)-1].Chunk == chunkID {
			dst[len(dst)-1].Bits |= bit
		} else {
			dst = append(dst, StoreMask{Chunk: chunkID, Bits: bit})
		}
	})
	return dst, err
}

// AppendLiveMasks appends one stable-slot word per live chunk. It is the
// universe used to complement an exactly indexed predicate.
func (s Snapshot) AppendLiveMasks(dst []StoreMask) []StoreMask {
	if s.state == nil {
		return dst
	}
	s.state.chunks.each(func(chunkID uint32, chunk *storeChunk) bool {
		dst = append(dst, StoreMask{Chunk: chunkID, Bits: chunk.live})
		return true
	})
	return dst
}

// AppendIndexKeys is [Snapshot.AppendIndexRows] with key materialization.
func (s Snapshot) AppendIndexKeys(dst []string, name string, values ...Index) ([]string, error) {
	err := s.visitIndexMatches(name, values, func(_ uint32, chunk *storeChunk, slot int) {
		dst = append(dst, chunk.key(slot))
	})
	return dst, err
}

// IndexKeys is the allocating convenience form of [Snapshot.AppendIndexKeys].
func (s Snapshot) IndexKeys(name string, values ...Index) ([]string, error) {
	return s.AppendIndexKeys(nil, name, values...)
}

// AppendIndexRawKeys validates scalar JSON values and probes a declared index.
// Scalar needles use fixed stack tape storage; with sufficient dst capacity,
// the complete operation allocates nothing.
func (s Snapshot) AppendIndexRawKeys(dst []string, name string, values ...[]byte) ([]string, error) {
	if len(values) > StoreIndexMaxColumns {
		return dst, ErrStoreIndexArity
	}
	var indexes [StoreIndexMaxColumns]Index
	var entries [StoreIndexMaxColumns]IndexEntry
	for i, src := range values {
		need, err := RequiredIndexEntries(src)
		if err != nil {
			return dst, err
		}
		if need != 1 {
			return dst, ErrStoreIndexScalar
		}
		index, err := BuildIndex(src, entries[i:i+1:i+1])
		if err != nil {
			return dst, err
		}
		indexes[i] = index
	}
	return s.AppendIndexKeys(dst, name, indexes[:len(values)]...)
}

// IndexRawKeys is the allocating convenience form of
// [Snapshot.AppendIndexRawKeys].
func (s Snapshot) IndexRawKeys(name string, values ...[]byte) ([]string, error) {
	return s.AppendIndexRawKeys(nil, name, values...)
}

// AppendIndexKeys probes the current Snapshot; see
// [Snapshot.AppendIndexKeys].
func (s *Store) AppendIndexKeys(dst []string, name string, values ...Index) ([]string, error) {
	return s.Snapshot().AppendIndexKeys(dst, name, values...)
}

// AppendIndexRows probes the current Snapshot; see
// [Snapshot.AppendIndexRows].
func (s *Store) AppendIndexRows(dst []StoreRow, name string, values ...Index) ([]StoreRow, error) {
	return s.Snapshot().AppendIndexRows(dst, name, values...)
}

// AppendIndexMasks probes the current Snapshot; see
// [Snapshot.AppendIndexMasks].
func (s *Store) AppendIndexMasks(dst []StoreMask, name string, values ...Index) ([]StoreMask, error) {
	return s.Snapshot().AppendIndexMasks(dst, name, values...)
}

// IndexKeys probes the current Snapshot; see [Snapshot.IndexKeys].
func (s *Store) IndexKeys(name string, values ...Index) ([]string, error) {
	return s.Snapshot().IndexKeys(name, values...)
}

// AppendIndexRawKeys probes the current Snapshot; see
// [Snapshot.AppendIndexRawKeys].
func (s *Store) AppendIndexRawKeys(dst []string, name string, values ...[]byte) ([]string, error) {
	return s.Snapshot().AppendIndexRawKeys(dst, name, values...)
}

// IndexRawKeys probes the current Snapshot; see [Snapshot.IndexRawKeys].
func (s *Store) IndexRawKeys(name string, values ...[]byte) ([]string, error) {
	return s.Snapshot().IndexRawKeys(name, values...)
}
