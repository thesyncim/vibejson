package storeio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/bits"
)

// The exact-term leaf is an isolated format candidate. It proves compact,
// canonical term/posting storage and direct equality routes, but it is not yet
// part of the durable index tree. Its benchmarks intentionally retain the
// current packed-record lower bound: promotion requires closing the ordered
// traversal gap without giving back the measured space reduction.

const (
	// IndexTermLeafMaxBytes keeps every internal offset bounded to sixteen
	// bits. The codec is intentionally independent of today's physical page
	// size: a caller may place a leaf in one page or in a bounded extent.
	IndexTermLeafMaxBytes = 1<<16 - 1

	// Restarting every eight terms bounds an exact binary-search comparison to
	// eight suffix copies while retaining most adjacent-key prefix savings.
	IndexTermLeafRestartInterval = 8

	indexTermLeafHeaderBytes     = 32
	indexTermLeafDescriptorBytes = 12
	indexTermLeafDictionaryBytes = 8

	indexTermLeafVersion = byte(1)

	indexTermLeafDirect1 = byte(iota)
	indexTermLeafDirectN
	indexTermLeafAdaptiveInline
	indexTermLeafAdaptiveDictionary
)

var (
	indexTermLeafMagic = [4]byte{'V', 'J', 'T', 'L'}
	indexTermLeafCRC   = crc32.MakeTable(crc32.Castagnoli)

	// ErrIndexTermLeafCorrupt reports a checksum-valid but structurally or
	// semantically invalid exact-term leaf as well as checksum damage.
	ErrIndexTermLeafCorrupt = errors.New("vibejson: corrupt exact-term index leaf")
)

// IndexTermLeafLiveLookup resolves the immutable stable-slot liveness tile
// that was used to build a posting. It is retained by an admitted leaf view;
// callers must therefore keep both the function and returned masks valid for
// the view's lifetime.
type IndexTermLeafLiveLookup func(tileID uint32) *[TermPostingTileChunks]uint64

// IndexTermLeafPosting is one already canonical adaptive posting supplied to
// the leaf builder. Component is empty for inline TermPosting records and is
// the exact content-addressed payload for manifest records. Live is required
// so the builder admits the TermPosting before selecting a leaf-local direct
// representation.
type IndexTermLeafPosting struct {
	Posting   TermPosting
	Component []byte
	Live      *[TermPostingTileChunks]uint64
}

// IndexTermLeafTerm groups all non-empty tile postings for one exact canonical
// scalar tuple. Terms must be strictly ordered by Key.Canonical. Postings must
// be strictly ordered by TileID.
type IndexTermLeafTerm struct {
	Key      IndexTermKeyRecord
	Postings []IndexTermLeafPosting
}

// IndexTermLeafView is a fully admitted immutable exact-term leaf. It borrows
// encoded and the liveness masks returned by live for its lifetime.
type IndexTermLeafView struct {
	encoded          []byte
	storeID          [16]byte
	live             IndexTermLeafLiveLookup
	termCount        uint16
	postingCount     uint16
	descriptorAt     uint16
	equalityAt       uint16
	equalitySlots    uint16
	keyAt            uint16
	postingAt        uint16
	dictionaryAt     uint16
	dictionaryN      uint16
	dictionaryDataAt uint16
}

// IndexTermLeafMatch is the posting aggregate for one exact term. Its iterator
// starts directly at the term's first posting; equality never scans an
// unrelated posting.
type IndexTermLeafMatch struct {
	leaf  IndexTermLeafView
	first uint16
	count uint16
}

// IndexTermLeafIterator streams distinct exact terms in canonical order. Key
// borrows iterator-owned scratch and is valid until the next Next call.
type IndexTermLeafIterator struct {
	leaf        IndexTermLeafView
	next        uint16
	end         uint16
	keyLength   uint16
	initialized bool
	key         [IndexTermMaxKeyBytes]byte
}

// IndexTermLeafPostingIterator streams one exact term's tile postings.
type IndexTermLeafPostingIterator struct {
	leaf         IndexTermLeafView
	position     int
	remaining    uint16
	previousTile uint32
	havePrevious bool
}

// IndexTermLeafAggregateIterator fuses tile-posting and mask traversal for
// equality probes. Direct1 returns after one trusted record decode and DirectN
// retains its remaining mask bytes in the cursor; neither constructs an
// intermediate posting view or TermPosting iterator.
type IndexTermLeafAggregateIterator struct {
	leaf         IndexTermLeafView
	position     int
	remaining    uint16
	previousTile uint32
	havePrevious bool
	pendingTile  uint32
	pending      []byte
	pendingAt    int
	adaptive     TermPostingIterator
	haveAdaptive bool
}

// IndexTermLeafPostingView is one trusted tile posting obtained from an
// admitted leaf. Direct encodings avoid reconstructing or validating a
// TermPosting on the hot path; adaptive encodings reuse TermPosting's iterator.
type IndexTermLeafPostingView struct {
	tileID    uint32
	rows      uint16
	kind      byte
	direct    []byte
	adaptive  TermPosting
	component []byte
	live      *[TermPostingTileChunks]uint64
}

// IndexTermLeafMaskIterator streams ordered non-empty 64-row masks.
type IndexTermLeafMaskIterator struct {
	kind     byte
	direct   []byte
	position int
	one      TermPostingMask
	haveOne  bool
	adaptive TermPostingIterator
}

type indexTermLeafDerivedPosting struct {
	tileID        uint32
	rows          uint16
	kind          byte
	codec         TermPostingCodec
	payload       []byte
	direct        [2]TermPostingMask
	directCount   uint8
	dictionaryKey string
	dictionary    uint16
}

type indexTermLeafDerivedTerm struct {
	key          []byte
	shared       uint16
	suffixAt     uint16
	firstPosting uint16
	postings     []indexTermLeafDerivedPosting
}

type indexTermLeafDictionary struct {
	codec   TermPostingCodec
	rows    uint16
	payload []byte
}

type indexTermLeafDecodedPosting struct {
	kind       byte
	tileID     uint32
	rows       uint16
	codec      TermPostingCodec
	payload    []byte
	dictionary uint16
	direct     []byte
	next       int
}

// AppendIndexTermLeaf appends one deterministic exact-term leaf. Keys are
// prefix-compressed once per distinct term. The descriptor's first/count pair
// is the leaf-local posting aggregate used by equality. Repeated adaptive
// payloads larger than TermPostingInlineBytes enter a leaf dictionary iff
// (uses-1)*payloadBytes exceeds the eight-byte dictionary descriptor.
//
// On failure dst is returned unchanged.
func AppendIndexTermLeaf(
	dst []byte,
	storeID [16]byte,
	terms []IndexTermLeafTerm,
) ([]byte, error) {
	original := dst
	if len(terms) == 0 || len(terms) > int(^uint16(0)) {
		return original, fmt.Errorf("%w: exact-term leaf term count", ErrInvalidWrite)
	}

	derived := make([]indexTermLeafDerivedTerm, len(terms))
	totalPostings := 0
	keyBytes := 0
	payloadUses := make(map[string]int)
	for i := range terms {
		term := &terms[i]
		key := term.Key.Canonical
		if !ValidIndexTermKey(key) ||
			term.Key.RouteHash != IndexTermRouteHash(storeID, key) ||
			i != 0 && bytes.Compare(terms[i-1].Key.Canonical, key) >= 0 ||
			len(term.Postings) == 0 {
			return original, fmt.Errorf("%w: exact-term leaf key or order", ErrInvalidWrite)
		}
		if totalPostings > int(^uint16(0))-len(term.Postings) {
			return original, fmt.Errorf("%w: exact-term leaf posting count", ErrInvalidWrite)
		}
		totalPostings += len(term.Postings)

		shared := 0
		if i%IndexTermLeafRestartInterval != 0 {
			shared = commonIndexTermLeafPrefix(terms[i-1].Key.Canonical, key)
		}
		derived[i] = indexTermLeafDerivedTerm{
			key: key, shared: uint16(shared),
			postings: make([]indexTermLeafDerivedPosting, len(term.Postings)),
		}
		keyBytes += len(key) - shared

		var previousTile uint32
		for j := range term.Postings {
			input := &term.Postings[j]
			if input.Live == nil ||
				j != 0 && input.Posting.TileID <= previousTile ||
				input.Posting.Rows == 0 {
				return original, fmt.Errorf("%w: exact-term leaf posting order", ErrInvalidWrite)
			}
			view, err := OpenTermPosting(input.Posting, input.Component, input.Live)
			if err != nil {
				return original, fmt.Errorf("%w: exact-term leaf posting: %v", ErrInvalidWrite, err)
			}
			var masks [TermPostingTileChunks]uint64
			if !view.MasksInto(&masks) {
				return original, fmt.Errorf("%w: exact-term leaf posting masks", ErrInvalidWrite)
			}
			posting := deriveIndexTermLeafPosting(input.Posting, input.Component, &masks)
			derived[i].postings[j] = posting
			if posting.dictionaryKey != "" {
				payloadUses[posting.dictionaryKey]++
			}
			previousTile = input.Posting.TileID
		}
	}

	dictionaries := make([]indexTermLeafDictionary, 0)
	dictionaryIndex := make(map[string]uint16)
	postingBytes := 0
	for i := range derived {
		var previousTile uint32
		for j := range derived[i].postings {
			posting := &derived[i].postings[j]
			if posting.dictionaryKey != "" &&
				(payloadUses[posting.dictionaryKey]-1)*len(posting.payload) >
					indexTermLeafDictionaryBytes {
				index, ok := dictionaryIndex[posting.dictionaryKey]
				if !ok {
					index = uint16(len(dictionaries))
					dictionaryIndex[posting.dictionaryKey] = index
					dictionaries = append(dictionaries, indexTermLeafDictionary{
						codec: posting.codec, rows: posting.rows, payload: posting.payload,
					})
				}
				posting.kind = indexTermLeafAdaptiveDictionary
				posting.dictionary = index
			}
			delta := uint64(posting.tileID)
			if j != 0 {
				delta = uint64(posting.tileID - previousTile - 1)
			}
			postingBytes += indexTermLeafPostingBytes(posting, delta)
			previousTile = posting.tileID
		}
	}

	dictionaryPayloadBytes := 0
	for i := range dictionaries {
		dictionaryPayloadBytes += len(dictionaries[i].payload)
	}
	descriptorAt := indexTermLeafHeaderBytes
	equalityAt := descriptorAt + len(derived)*indexTermLeafDescriptorBytes
	equalitySlots := indexTermLeafEqualitySlots(len(derived))
	keyAt := equalityAt + equalitySlots*2
	postingAt := keyAt + keyBytes
	dictionaryAt := postingAt + postingBytes
	dictionaryDataAt := dictionaryAt + len(dictionaries)*indexTermLeafDictionaryBytes
	totalBytes := dictionaryDataAt + dictionaryPayloadBytes
	if totalBytes > IndexTermLeafMaxBytes {
		return original, fmt.Errorf("%w: exact-term leaf size", ErrInvalidWrite)
	}

	encoded := make([]byte, totalBytes)
	copy(encoded[0:4], indexTermLeafMagic[:])
	encoded[4] = indexTermLeafVersion
	encoded[5] = IndexTermLeafRestartInterval
	binary.LittleEndian.PutUint32(encoded[8:12], uint32(totalBytes))
	binary.LittleEndian.PutUint16(encoded[12:14], uint16(len(derived)))
	binary.LittleEndian.PutUint16(encoded[14:16], uint16(totalPostings))
	binary.LittleEndian.PutUint16(encoded[16:18], uint16(descriptorAt))
	binary.LittleEndian.PutUint16(encoded[18:20], uint16(keyAt))
	binary.LittleEndian.PutUint16(encoded[20:22], uint16(postingAt))
	binary.LittleEndian.PutUint16(encoded[22:24], uint16(dictionaryAt))
	binary.LittleEndian.PutUint16(encoded[24:26], uint16(len(dictionaries)))
	binary.LittleEndian.PutUint16(encoded[26:28], uint16(dictionaryDataAt))
	for position := equalityAt; position < keyAt; position += 2 {
		binary.LittleEndian.PutUint16(encoded[position:position+2], ^uint16(0))
	}
	for i := range terms {
		slot := int(terms[i].Key.RouteHash & uint64(equalitySlots-1))
		for binary.LittleEndian.Uint16(
			encoded[equalityAt+slot*2:equalityAt+slot*2+2],
		) != ^uint16(0) {
			slot = (slot + 1) & (equalitySlots - 1)
		}
		binary.LittleEndian.PutUint16(
			encoded[equalityAt+slot*2:equalityAt+slot*2+2], uint16(i),
		)
	}

	keyPosition := keyAt
	postingPosition := postingAt
	for i := range derived {
		term := &derived[i]
		term.suffixAt = uint16(keyPosition - keyAt)
		term.firstPosting = uint16(postingPosition - postingAt)
		suffix := term.key[int(term.shared):]
		copy(encoded[keyPosition:], suffix)
		keyPosition += len(suffix)

		var previousTile uint32
		for j := range term.postings {
			posting := &term.postings[j]
			delta := uint64(posting.tileID)
			if j != 0 {
				delta = uint64(posting.tileID - previousTile - 1)
			}
			postingPosition = appendIndexTermLeafPosting(
				encoded, postingPosition, posting, delta,
			)
			previousTile = posting.tileID
		}
		record := encoded[descriptorAt+i*indexTermLeafDescriptorBytes:]
		binary.LittleEndian.PutUint16(record[0:2], term.suffixAt)
		binary.LittleEndian.PutUint16(record[2:4], term.firstPosting)
		binary.LittleEndian.PutUint16(record[4:6], uint16(len(term.key)))
		binary.LittleEndian.PutUint16(record[6:8], term.shared)
		binary.LittleEndian.PutUint16(record[8:10], uint16(len(suffix)))
		binary.LittleEndian.PutUint16(record[10:12], uint16(len(term.postings)))
	}

	dictionaryPosition := dictionaryDataAt
	for i := range dictionaries {
		dictionary := &dictionaries[i]
		record := encoded[dictionaryAt+i*indexTermLeafDictionaryBytes:]
		binary.LittleEndian.PutUint16(record[0:2], uint16(dictionaryPosition-dictionaryDataAt))
		binary.LittleEndian.PutUint16(record[2:4], uint16(len(dictionary.payload)))
		binary.LittleEndian.PutUint16(record[4:6], dictionary.rows)
		record[6] = byte(dictionary.codec)
		copy(encoded[dictionaryPosition:], dictionary.payload)
		dictionaryPosition += len(dictionary.payload)
	}
	binary.LittleEndian.PutUint32(encoded[28:32], indexTermLeafChecksum(encoded))
	return append(dst, encoded...), nil
}

// OpenIndexTermLeaf validates checksum, exact section layout, restart-front
// coding, canonical keys and ordering, posting order and adaptive codec,
// live-slot containment, and profitable deterministic dictionary selection.
// It allocates nothing on success.
func OpenIndexTermLeaf(
	encoded []byte,
	storeID [16]byte,
	live IndexTermLeafLiveLookup,
) (IndexTermLeafView, error) {
	if len(encoded) < indexTermLeafHeaderBytes ||
		len(encoded) > IndexTermLeafMaxBytes ||
		!bytes.Equal(encoded[0:4], indexTermLeafMagic[:]) ||
		encoded[4] != indexTermLeafVersion ||
		encoded[5] != IndexTermLeafRestartInterval ||
		binary.LittleEndian.Uint16(encoded[6:8]) != 0 ||
		int(binary.LittleEndian.Uint32(encoded[8:12])) != len(encoded) ||
		binary.LittleEndian.Uint32(encoded[28:32]) != indexTermLeafChecksum(encoded) {
		return IndexTermLeafView{}, indexTermLeafCorrupt("header or checksum")
	}
	termCount := binary.LittleEndian.Uint16(encoded[12:14])
	postingCount := binary.LittleEndian.Uint16(encoded[14:16])
	descriptorAt := binary.LittleEndian.Uint16(encoded[16:18])
	keyAt := binary.LittleEndian.Uint16(encoded[18:20])
	postingAt := binary.LittleEndian.Uint16(encoded[20:22])
	dictionaryAt := binary.LittleEndian.Uint16(encoded[22:24])
	dictionaryN := binary.LittleEndian.Uint16(encoded[24:26])
	dictionaryDataAt := binary.LittleEndian.Uint16(encoded[26:28])
	equalityAt := uint32(descriptorAt) +
		uint32(termCount)*indexTermLeafDescriptorBytes
	equalitySlots := indexTermLeafEqualitySlots(int(termCount))
	if termCount == 0 || postingCount < termCount || live == nil ||
		descriptorAt != indexTermLeafHeaderBytes ||
		uint32(keyAt) != equalityAt+uint32(equalitySlots)*2 ||
		keyAt > postingAt || postingAt > dictionaryAt ||
		uint32(dictionaryDataAt) != uint32(dictionaryAt)+
			uint32(dictionaryN)*indexTermLeafDictionaryBytes ||
		int(dictionaryDataAt) > len(encoded) {
		return IndexTermLeafView{}, indexTermLeafCorrupt("section layout")
	}
	view := IndexTermLeafView{
		encoded: encoded, storeID: storeID, live: live, termCount: termCount,
		postingCount: postingCount, descriptorAt: descriptorAt,
		equalityAt: uint16(equalityAt), equalitySlots: uint16(equalitySlots),
		keyAt: keyAt, postingAt: postingAt, dictionaryAt: dictionaryAt,
		dictionaryN: dictionaryN, dictionaryDataAt: dictionaryDataAt,
	}
	if err := view.admitDictionaries(); err != nil {
		return IndexTermLeafView{}, err
	}
	if err := view.admitTermsAndPostings(); err != nil {
		return IndexTermLeafView{}, err
	}
	if err := view.admitEqualityTable(); err != nil {
		return IndexTermLeafView{}, err
	}
	if err := view.admitCanonicalDictionaryChoice(); err != nil {
		return IndexTermLeafView{}, err
	}
	return view, nil
}

func (v IndexTermLeafView) Len() int           { return int(v.termCount) }
func (v IndexTermLeafView) PostingLen() int    { return int(v.postingCount) }
func (v IndexTermLeafView) DictionaryLen() int { return int(v.dictionaryN) }
func (v IndexTermLeafView) EncodedBytes() int  { return len(v.encoded) }

// Lookup returns the exact term's first/count posting aggregate. Its internally
// computed route selects candidates only; complete canonical bytes remain the
// proof of identity, so a collision cannot produce a false match.
func (v IndexTermLeafView) Lookup(canonical []byte) (IndexTermLeafMatch, bool) {
	return v.LookupRecord(IndexTermKeyRecord{
		RouteHash: IndexTermRouteHash(v.storeID, canonical),
		Canonical: canonical,
	})
}

// LookupRecord reuses a query planner's already computed StoreID-keyed route.
// The compact table selects candidates only; complete canonical bytes remain
// the proof of identity.
func (v IndexTermLeafView) LookupRecord(
	key IndexTermKeyRecord,
) (IndexTermLeafMatch, bool) {
	if v.equalitySlots == 0 {
		return IndexTermLeafMatch{}, false
	}
	slot := int(key.RouteHash & uint64(v.equalitySlots-1))
	for probes := 0; probes < int(v.equalitySlots); probes++ {
		index := binary.LittleEndian.Uint16(v.encoded[int(v.equalityAt)+slot*2 : int(v.equalityAt)+slot*2+2])
		if index == ^uint16(0) {
			return IndexTermLeafMatch{}, false
		}
		if index < v.termCount &&
			v.exactKeyAt(int(index), key.Canonical) {
			return v.matchAt(int(index)), true
		}
		slot = (slot + 1) & (int(v.equalitySlots) - 1)
	}
	return IndexTermLeafMatch{}, false
}

// Range returns [lower, upper) in canonical key order. A nil bound is open.
func (v IndexTermLeafView) Range(lower, upper []byte) IndexTermLeafIterator {
	start, end := 0, int(v.termCount)
	if lower != nil {
		start = v.lowerBound(lower)
	}
	if upper != nil {
		end = v.lowerBound(upper)
	}
	if end < start {
		end = start
	}
	return IndexTermLeafIterator{leaf: v, next: uint16(start), end: uint16(end)}
}

func (v IndexTermLeafView) Ordered() IndexTermLeafIterator {
	return IndexTermLeafIterator{leaf: v, end: v.termCount}
}

func (it *IndexTermLeafIterator) Next() (
	key []byte,
	match IndexTermLeafMatch,
	ok bool,
) {
	if it == nil || it.next >= it.end {
		return nil, IndexTermLeafMatch{}, false
	}
	index := int(it.next)
	if !it.initialized {
		key, ok = it.leaf.reconstructKey(index, it.key[:0])
		if !ok {
			return nil, IndexTermLeafMatch{}, false
		}
		it.keyLength = uint16(len(key))
		it.initialized = true
	} else {
		record := it.leaf.descriptor(index)
		suffixAt := int(binary.LittleEndian.Uint16(record[0:2]))
		keyLength := int(binary.LittleEndian.Uint16(record[4:6]))
		shared := int(binary.LittleEndian.Uint16(record[6:8]))
		suffixLength := int(binary.LittleEndian.Uint16(record[8:10]))
		if shared > int(it.keyLength) || shared+suffixLength != keyLength {
			return nil, IndexTermLeafMatch{}, false
		}
		copy(it.key[shared:keyLength], it.leaf.encoded[int(it.leaf.keyAt)+suffixAt:int(it.leaf.keyAt)+suffixAt+suffixLength])
		it.keyLength = uint16(keyLength)
		key = it.key[:keyLength]
	}
	match = it.leaf.matchAt(index)
	it.next++
	return key, match, true
}

func (m IndexTermLeafMatch) Len() int { return int(m.count) }

func (m IndexTermLeafMatch) Iterator() IndexTermLeafPostingIterator {
	return IndexTermLeafPostingIterator{
		leaf: m.leaf, position: int(m.leaf.postingAt) + int(m.first),
		remaining: m.count,
	}
}

// MaskIterator is the preferred equality hot path when the consumer needs
// masks rather than posting metadata.
func (m IndexTermLeafMatch) MaskIterator() IndexTermLeafAggregateIterator {
	return IndexTermLeafAggregateIterator{
		leaf: m.leaf, position: int(m.leaf.postingAt) + int(m.first),
		remaining: m.count,
	}
}

func (it *IndexTermLeafAggregateIterator) Next() (
	tileID uint32,
	mask TermPostingMask,
	ok bool,
) {
	if it == nil {
		return 0, TermPostingMask{}, false
	}
	if it.pendingAt < len(it.pending) {
		record := it.pending[it.pendingAt:]
		it.pendingAt += 9
		return it.pendingTile, TermPostingMask{
			Chunk: record[0], Bits: binary.LittleEndian.Uint64(record[1:9]),
		}, true
	}
	if it.haveAdaptive {
		if mask, ok := it.adaptive.Next(); ok {
			return it.pendingTile, mask, true
		}
		it.haveAdaptive = false
	}
	for it.remaining != 0 {
		// Direct records were fully admitted by Open. Decode their tiny trusted
		// shape in place so singleton/few-mask equality does not pay the
		// adaptive union decoder or construct an intermediate posting view.
		position := it.position
		kind := it.leaf.encoded[position]
		if kind == indexTermLeafDirect1 || kind == indexTermLeafDirectN {
			delta, n := trustedTermPostingUvarint(
				it.leaf.encoded[position+1 : int(it.leaf.dictionaryAt)],
			)
			tile := uint32(delta)
			if it.havePrevious {
				tile = it.previousTile + 1 + uint32(delta)
			}
			payloadAt := position + 1 + n
			it.previousTile = tile
			it.havePrevious = true
			it.remaining--
			if kind == indexTermLeafDirect1 {
				row := int(binary.LittleEndian.Uint16(
					it.leaf.encoded[payloadAt : payloadAt+2],
				))
				it.position = payloadAt + 2
				return tile, TermPostingMask{
					Chunk: uint8(row >> 6), Bits: uint64(1) << uint(row&63),
				}, true
			}
			count := int(it.leaf.encoded[payloadAt])
			it.position = payloadAt + 1 + count*9
			it.pendingTile = tile
			it.pending = it.leaf.encoded[payloadAt+1 : it.position]
			it.pendingAt = 9
			record := it.pending
			return tile, TermPostingMask{
				Chunk: record[0], Bits: binary.LittleEndian.Uint64(record[1:9]),
			}, true
		}
		decoded, admitted := it.leaf.decodePosting(
			it.position, int(it.leaf.dictionaryAt),
			it.previousTile, it.havePrevious, true,
		)
		if !admitted {
			return 0, TermPostingMask{}, false
		}
		it.position = decoded.next
		it.previousTile = decoded.tileID
		it.havePrevious = true
		it.remaining--
		record, component := makeIndexTermLeafPosting(
			decoded.tileID, decoded.rows, decoded.codec, decoded.payload,
		)
		it.pendingTile = decoded.tileID
		it.adaptive = TermPostingIterator{
			posting: record, component: component,
			live:  it.leaf.live(decoded.tileID),
			chunk: -1, previousRow: -1, pendingRow: -1,
		}
		it.haveAdaptive = true
		if mask, ok := it.adaptive.Next(); ok {
			return decoded.tileID, mask, true
		}
		it.haveAdaptive = false
	}
	return 0, TermPostingMask{}, false
}

func (it *IndexTermLeafPostingIterator) Next() (
	IndexTermLeafPostingView,
	bool,
) {
	if it == nil || it.remaining == 0 {
		return IndexTermLeafPostingView{}, false
	}
	decoded, ok := it.leaf.decodePosting(
		it.position, it.postingEnd(), it.previousTile, it.havePrevious, true,
	)
	if !ok {
		return IndexTermLeafPostingView{}, false
	}
	result := IndexTermLeafPostingView{
		tileID: decoded.tileID, rows: decoded.rows, kind: decoded.kind,
		direct: decoded.direct, live: it.leaf.live(decoded.tileID),
	}
	if decoded.kind == indexTermLeafAdaptiveInline ||
		decoded.kind == indexTermLeafAdaptiveDictionary {
		result.adaptive, result.component = makeIndexTermLeafPosting(
			decoded.tileID, decoded.rows, decoded.codec, decoded.payload,
		)
	}
	it.position = decoded.next
	it.previousTile = decoded.tileID
	it.havePrevious = true
	it.remaining--
	return result, true
}

func (p IndexTermLeafPostingView) TileID() uint32 { return p.tileID }
func (p IndexTermLeafPostingView) Rows() uint16   { return p.rows }

func (p IndexTermLeafPostingView) Iterator() IndexTermLeafMaskIterator {
	switch p.kind {
	case indexTermLeafDirect1:
		row := int(binary.LittleEndian.Uint16(p.direct))
		return IndexTermLeafMaskIterator{
			kind: p.kind, haveOne: true,
			one: TermPostingMask{
				Chunk: uint8(row >> 6), Bits: uint64(1) << uint(row&63),
			},
		}
	case indexTermLeafDirectN:
		return IndexTermLeafMaskIterator{kind: p.kind, direct: p.direct[1:]}
	default:
		return IndexTermLeafMaskIterator{
			kind: p.kind,
			adaptive: TermPostingIterator{
				posting: p.adaptive, component: p.component, live: p.live,
				chunk: -1, previousRow: -1, pendingRow: -1,
			},
		}
	}
}

func (it *IndexTermLeafMaskIterator) Next() (TermPostingMask, bool) {
	if it == nil {
		return TermPostingMask{}, false
	}
	switch it.kind {
	case indexTermLeafDirect1:
		if !it.haveOne {
			return TermPostingMask{}, false
		}
		it.haveOne = false
		return it.one, true
	case indexTermLeafDirectN:
		if it.position >= len(it.direct) {
			return TermPostingMask{}, false
		}
		record := it.direct[it.position:]
		it.position += 9
		return TermPostingMask{
			Chunk: record[0], Bits: binary.LittleEndian.Uint64(record[1:9]),
		}, true
	default:
		return it.adaptive.Next()
	}
}

func deriveIndexTermLeafPosting(
	posting TermPosting,
	component []byte,
	masks *[TermPostingTileChunks]uint64,
) indexTermLeafDerivedPosting {
	derived := indexTermLeafDerivedPosting{
		tileID: posting.TileID, rows: posting.Rows,
		codec: posting.Codec, kind: indexTermLeafAdaptiveInline,
	}
	for chunk, mask := range masks {
		if mask == 0 {
			continue
		}
		if derived.directCount < 2 {
			derived.direct[derived.directCount] = TermPostingMask{
				Chunk: uint8(chunk), Bits: mask,
			}
		}
		derived.directCount++
	}
	if posting.Rows == 1 {
		derived.kind = indexTermLeafDirect1
		return derived
	}
	if derived.directCount <= 2 {
		derived.kind = indexTermLeafDirectN
		return derived
	}
	if posting.Placement == TermPostingInline {
		derived.payload = posting.Inline[:posting.EncodedBytes:posting.EncodedBytes]
	} else {
		derived.payload = component
	}
	if len(derived.payload) > TermPostingInlineBytes {
		var identity [3 + TermPostingMaxPayloadBytes]byte
		identity[0] = byte(posting.Codec)
		binary.LittleEndian.PutUint16(identity[1:3], posting.Rows)
		copy(identity[3:], derived.payload)
		derived.dictionaryKey = string(identity[:3+len(derived.payload)])
	}
	return derived
}

func indexTermLeafPostingBytes(
	posting *indexTermLeafDerivedPosting,
	tileDelta uint64,
) int {
	base := 1 + indexTermLeafUvarintBytes(tileDelta)
	switch posting.kind {
	case indexTermLeafDirect1:
		return base + 2
	case indexTermLeafDirectN:
		return base + 1 + int(posting.directCount)*9
	case indexTermLeafAdaptiveInline:
		return base + 5 + len(posting.payload)
	case indexTermLeafAdaptiveDictionary:
		return base + 5
	default:
		panic("invalid derived exact-term posting")
	}
}

func appendIndexTermLeafPosting(
	dst []byte,
	position int,
	posting *indexTermLeafDerivedPosting,
	tileDelta uint64,
) int {
	dst[position] = posting.kind
	position++
	position += putIndexTermLeafUvarint(dst[position:], tileDelta)
	switch posting.kind {
	case indexTermLeafDirect1:
		row := 0
		for _, mask := range posting.direct {
			if mask.Bits != 0 {
				row = int(mask.Chunk)*64 + bits.TrailingZeros64(mask.Bits)
				break
			}
		}
		binary.LittleEndian.PutUint16(dst[position:position+2], uint16(row))
		return position + 2
	case indexTermLeafDirectN:
		dst[position] = posting.directCount
		position++
		for i := 0; i < int(posting.directCount); i++ {
			dst[position] = posting.direct[i].Chunk
			binary.LittleEndian.PutUint64(dst[position+1:position+9], posting.direct[i].Bits)
			position += 9
		}
		return position
	case indexTermLeafAdaptiveInline:
		binary.LittleEndian.PutUint16(dst[position:position+2], posting.rows)
		dst[position+2] = byte(posting.codec)
		binary.LittleEndian.PutUint16(dst[position+3:position+5], uint16(len(posting.payload)))
		copy(dst[position+5:], posting.payload)
		return position + 5 + len(posting.payload)
	case indexTermLeafAdaptiveDictionary:
		binary.LittleEndian.PutUint16(dst[position:position+2], posting.rows)
		dst[position+2] = byte(posting.codec)
		binary.LittleEndian.PutUint16(dst[position+3:position+5], posting.dictionary)
		return position + 5
	default:
		panic("invalid derived exact-term posting")
	}
}

func (v IndexTermLeafView) admitDictionaries() error {
	expected := 0
	for i := 0; i < int(v.dictionaryN); i++ {
		record := v.dictionaryRecord(i)
		offset := int(binary.LittleEndian.Uint16(record[0:2]))
		length := int(binary.LittleEndian.Uint16(record[2:4]))
		rows := binary.LittleEndian.Uint16(record[4:6])
		codec := TermPostingCodec(record[6])
		if offset != expected || length <= TermPostingInlineBytes ||
			length > TermPostingMaxPayloadBytes || rows == 0 ||
			codec > TermPostingSparseRows || record[7] != 0 ||
			int(v.dictionaryDataAt)+offset+length > len(v.encoded) {
			return indexTermLeafCorrupt("dictionary layout")
		}
		payload := v.encoded[int(v.dictionaryDataAt)+offset : int(v.dictionaryDataAt)+offset+length]
		for previous := 0; previous < i; previous++ {
			other, otherCodec, otherRows, ok :=
				v.dictionaryPayload(previous)
			if !ok {
				return indexTermLeafCorrupt("dictionary predecessor")
			}
			if codec == otherCodec && rows == otherRows &&
				bytes.Equal(payload, other) {
				return indexTermLeafCorrupt("duplicate dictionary payload")
			}
		}
		expected += length
	}
	if int(v.dictionaryDataAt)+expected != len(v.encoded) {
		return indexTermLeafCorrupt("dictionary data length")
	}
	return nil
}

func (v IndexTermLeafView) admitTermsAndPostings() error {
	expectedSuffix := 0
	expectedPosting := 0
	seenPostings := 0
	var previous [IndexTermMaxKeyBytes]byte
	previousLen := 0
	nextDictionary := uint16(0)
	for i := 0; i < int(v.termCount); i++ {
		record := v.descriptor(i)
		suffixAt := int(binary.LittleEndian.Uint16(record[0:2]))
		firstPosting := int(binary.LittleEndian.Uint16(record[2:4]))
		keyLength := int(binary.LittleEndian.Uint16(record[4:6]))
		shared := int(binary.LittleEndian.Uint16(record[6:8]))
		suffixLength := int(binary.LittleEndian.Uint16(record[8:10]))
		count := int(binary.LittleEndian.Uint16(record[10:12]))
		if suffixAt != expectedSuffix || firstPosting != expectedPosting ||
			keyLength == 0 || keyLength > IndexTermMaxKeyBytes ||
			shared+suffixLength != keyLength || count == 0 ||
			i%IndexTermLeafRestartInterval == 0 && shared != 0 ||
			i%IndexTermLeafRestartInterval != 0 && shared > previousLen ||
			int(v.keyAt)+suffixAt+suffixLength > int(v.postingAt) {
			return indexTermLeafCorrupt("term descriptor")
		}
		var current [IndexTermMaxKeyBytes]byte
		copy(current[:shared], previous[:shared])
		copy(current[shared:keyLength], v.encoded[int(v.keyAt)+suffixAt:int(v.keyAt)+suffixAt+suffixLength])
		key := current[:keyLength]
		if !ValidIndexTermKey(key) ||
			i != 0 && bytes.Compare(previous[:previousLen], key) >= 0 ||
			i%IndexTermLeafRestartInterval != 0 &&
				shared != commonIndexTermLeafPrefix(previous[:previousLen], key) {
			return indexTermLeafCorrupt("canonical term key or prefix")
		}
		expectedSuffix += suffixLength
		copy(previous[:], key)
		previousLen = keyLength

		position := int(v.postingAt) + firstPosting
		end := int(v.dictionaryAt)
		if i+1 < int(v.termCount) {
			end = int(v.postingAt) +
				int(binary.LittleEndian.Uint16(v.descriptor(i + 1)[2:4]))
		}
		var previousTile uint32
		for j := 0; j < count; j++ {
			posting, ok := v.decodePosting(
				position, end, previousTile, j != 0, false,
			)
			if !ok || !v.admitPosting(posting) {
				return indexTermLeafCorrupt("posting")
			}
			if posting.kind == indexTermLeafAdaptiveDictionary {
				if posting.dictionary > nextDictionary {
					return indexTermLeafCorrupt("dictionary first-use order")
				}
				if posting.dictionary == nextDictionary {
					nextDictionary++
				}
			}
			position = posting.next
			previousTile = posting.tileID
			seenPostings++
		}
		if position != end {
			return indexTermLeafCorrupt("posting aggregate boundary")
		}
		expectedPosting = end - int(v.postingAt)
	}
	if int(v.keyAt)+expectedSuffix != int(v.postingAt) ||
		int(v.postingAt)+expectedPosting != int(v.dictionaryAt) ||
		seenPostings != int(v.postingCount) ||
		nextDictionary != v.dictionaryN {
		return indexTermLeafCorrupt("aggregate totals")
	}
	return nil
}

func (v IndexTermLeafView) admitEqualityTable() error {
	for slot := 0; slot < int(v.equalitySlots); slot++ {
		index := binary.LittleEndian.Uint16(v.encoded[int(v.equalityAt)+slot*2 : int(v.equalityAt)+slot*2+2])
		if index != ^uint16(0) && index >= v.termCount {
			return indexTermLeafCorrupt("equality table index")
		}
	}
	var scratch [IndexTermMaxKeyBytes]byte
	for index := 0; index < int(v.termCount); index++ {
		key, ok := v.reconstructKey(index, scratch[:0])
		if !ok {
			return indexTermLeafCorrupt("equality table key")
		}
		hash := IndexTermRouteHash(v.storeID, key)
		slot := int(hash & uint64(v.equalitySlots-1))
		found := false
		for probes := 0; probes < int(v.equalitySlots); probes++ {
			candidate := binary.LittleEndian.Uint16(v.encoded[int(v.equalityAt)+slot*2 : int(v.equalityAt)+slot*2+2])
			if candidate == uint16(index) {
				found = true
				break
			}
			// Canonical construction inserts terms in key order. Therefore an
			// empty slot or a later term before this term proves a different
			// (even if lookup-equivalent) table serialization.
			if candidate == ^uint16(0) || candidate > uint16(index) {
				break
			}
			slot = (slot + 1) & (int(v.equalitySlots) - 1)
		}
		if !found {
			return indexTermLeafCorrupt("non-canonical equality table")
		}
	}
	return nil
}

func (v IndexTermLeafView) admitPosting(posting indexTermLeafDecodedPosting) bool {
	live := v.live(posting.tileID)
	if live == nil {
		return false
	}
	switch posting.kind {
	case indexTermLeafDirect1:
		row := int(binary.LittleEndian.Uint16(posting.direct))
		return row < TermPostingTileRows &&
			live[row>>6]&(uint64(1)<<uint(row&63)) != 0
	case indexTermLeafDirectN:
		count := int(posting.direct[0])
		rows := 0
		previousChunk := -1
		for i := 0; i < count; i++ {
			record := posting.direct[1+i*9:]
			chunk := int(record[0])
			mask := binary.LittleEndian.Uint64(record[1:9])
			if chunk <= previousChunk || chunk >= TermPostingTileChunks ||
				mask == 0 || mask&^live[chunk] != 0 {
				return false
			}
			rows += bits.OnesCount64(mask)
			previousChunk = chunk
		}
		return rows > 1
	case indexTermLeafAdaptiveInline, indexTermLeafAdaptiveDictionary:
		record, component := makeIndexTermLeafPosting(
			posting.tileID, posting.rows, posting.codec, posting.payload,
		)
		view, err := OpenTermPosting(record, component, live)
		if err != nil {
			return false
		}
		iterator := view.Iterator()
		masks := 0
		for {
			_, ok := iterator.Next()
			if !ok {
				break
			}
			masks++
			if masks > 2 {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func (v IndexTermLeafView) admitCanonicalDictionaryChoice() error {
	for dictionary := 0; dictionary < int(v.dictionaryN); dictionary++ {
		payload, codec, rows, ok := v.dictionaryPayload(dictionary)
		if !ok {
			return indexTermLeafCorrupt("dictionary reference")
		}
		uses := v.countAdaptiveIdentity(codec, rows, payload)
		if uses < 2 || (uses-1)*len(payload) <= indexTermLeafDictionaryBytes {
			return indexTermLeafCorrupt("unprofitable dictionary")
		}
	}
	// Any repeated out-of-line inline payload that would save bytes is a
	// second valid representation of the same logical leaf and is rejected.
	return v.walkPostings(func(posting indexTermLeafDecodedPosting) error {
		if posting.kind != indexTermLeafAdaptiveInline ||
			len(posting.payload) <= TermPostingInlineBytes {
			return nil
		}
		uses := v.countAdaptiveIdentity(posting.codec, posting.rows, posting.payload)
		if (uses-1)*len(posting.payload) > indexTermLeafDictionaryBytes {
			return indexTermLeafCorrupt("missing profitable dictionary")
		}
		return nil
	})
}

func (v IndexTermLeafView) countAdaptiveIdentity(
	codec TermPostingCodec,
	rows uint16,
	payload []byte,
) int {
	count := 0
	_ = v.walkPostings(func(posting indexTermLeafDecodedPosting) error {
		if (posting.kind == indexTermLeafAdaptiveInline ||
			posting.kind == indexTermLeafAdaptiveDictionary) &&
			posting.codec == codec && posting.rows == rows &&
			bytes.Equal(posting.payload, payload) {
			count++
		}
		return nil
	})
	return count
}

func (v IndexTermLeafView) walkPostings(
	fn func(indexTermLeafDecodedPosting) error,
) error {
	for i := 0; i < int(v.termCount); i++ {
		record := v.descriptor(i)
		position := int(v.postingAt) +
			int(binary.LittleEndian.Uint16(record[2:4]))
		count := int(binary.LittleEndian.Uint16(record[10:12]))
		end := int(v.dictionaryAt)
		if i+1 < int(v.termCount) {
			end = int(v.postingAt) +
				int(binary.LittleEndian.Uint16(v.descriptor(i + 1)[2:4]))
		}
		var previousTile uint32
		for j := 0; j < count; j++ {
			posting, ok := v.decodePosting(
				position, end, previousTile, j != 0, true,
			)
			if !ok {
				return indexTermLeafCorrupt("trusted posting walk")
			}
			if err := fn(posting); err != nil {
				return err
			}
			position = posting.next
			previousTile = posting.tileID
		}
	}
	return nil
}

func (v IndexTermLeafView) decodePosting(
	position, end int,
	previousTile uint32,
	havePrevious, trusted bool,
) (indexTermLeafDecodedPosting, bool) {
	if position < int(v.postingAt) || position >= end || end > int(v.dictionaryAt) {
		return indexTermLeafDecodedPosting{}, false
	}
	kind := v.encoded[position]
	position++
	var delta uint64
	var n int
	if trusted {
		delta, n = trustedTermPostingUvarint(v.encoded[position:end])
	} else {
		delta, n = canonicalTermPostingUvarint(v.encoded[position:end])
	}
	if n <= 0 {
		return indexTermLeafDecodedPosting{}, false
	}
	position += n
	tile := delta
	if havePrevious {
		tile = uint64(previousTile) + 1 + delta
	}
	if tile > uint64(^uint32(0)) ||
		havePrevious && uint32(tile) <= previousTile {
		return indexTermLeafDecodedPosting{}, false
	}
	result := indexTermLeafDecodedPosting{kind: kind, tileID: uint32(tile)}
	switch kind {
	case indexTermLeafDirect1:
		if position+2 > end {
			return result, false
		}
		result.rows = 1
		result.direct = v.encoded[position : position+2]
		result.next = position + 2
	case indexTermLeafDirectN:
		if position >= end {
			return result, false
		}
		count := int(v.encoded[position])
		if count < 1 || count > 2 || position+1+count*9 > end {
			return result, false
		}
		result.direct = v.encoded[position : position+1+count*9]
		for i := 0; i < count; i++ {
			result.rows += uint16(bits.OnesCount64(binary.LittleEndian.Uint64(
				result.direct[1+i*9+1 : 1+i*9+9],
			)))
		}
		result.next = position + 1 + count*9
	case indexTermLeafAdaptiveInline:
		if position+5 > end {
			return result, false
		}
		result.rows = binary.LittleEndian.Uint16(v.encoded[position : position+2])
		result.codec = TermPostingCodec(v.encoded[position+2])
		length := int(binary.LittleEndian.Uint16(v.encoded[position+3 : position+5]))
		if length > TermPostingMaxPayloadBytes || position+5+length > end {
			return result, false
		}
		result.payload = v.encoded[position+5 : position+5+length]
		result.next = position + 5 + length
	case indexTermLeafAdaptiveDictionary:
		if position+5 > end {
			return result, false
		}
		result.rows = binary.LittleEndian.Uint16(v.encoded[position : position+2])
		result.codec = TermPostingCodec(v.encoded[position+2])
		result.dictionary = binary.LittleEndian.Uint16(v.encoded[position+3 : position+5])
		payload, codec, rows, ok := v.dictionaryPayload(int(result.dictionary))
		if !ok || codec != result.codec || rows != result.rows {
			return result, false
		}
		result.payload = payload
		result.next = position + 5
	default:
		return result, false
	}
	return result, true
}

func (it *IndexTermLeafPostingIterator) postingEnd() int {
	if it.remaining == 0 {
		return it.position
	}
	// The iterator operates on an already admitted stream; dictionaryAt is a
	// safe broad bound and remaining prevents crossing into the next term.
	return int(it.leaf.dictionaryAt)
}

func (v IndexTermLeafView) dictionaryPayload(
	index int,
) ([]byte, TermPostingCodec, uint16, bool) {
	if index < 0 || index >= int(v.dictionaryN) {
		return nil, 0, 0, false
	}
	record := v.dictionaryRecord(index)
	offset := int(binary.LittleEndian.Uint16(record[0:2]))
	length := int(binary.LittleEndian.Uint16(record[2:4]))
	start := int(v.dictionaryDataAt) + offset
	if start < int(v.dictionaryDataAt) || start+length > len(v.encoded) {
		return nil, 0, 0, false
	}
	return v.encoded[start : start+length],
		TermPostingCodec(record[6]),
		binary.LittleEndian.Uint16(record[4:6]),
		true
}

func makeIndexTermLeafPosting(
	tileID uint32,
	rows uint16,
	codec TermPostingCodec,
	payload []byte,
) (TermPosting, []byte) {
	record := TermPosting{
		TileID: tileID, Rows: rows, EncodedBytes: uint16(len(payload)),
		Codec: codec, Placement: TermPostingInline,
	}
	if len(payload) <= TermPostingInlineBytes {
		copy(record.Inline[:], payload)
		return record, nil
	}
	record.Placement = TermPostingManifest
	record.ComponentID = termPostingComponentID(codec, rows, payload)
	return record, payload
}

func (v IndexTermLeafView) descriptor(index int) []byte {
	start := int(v.descriptorAt) + index*indexTermLeafDescriptorBytes
	return v.encoded[start : start+indexTermLeafDescriptorBytes]
}

func (v IndexTermLeafView) dictionaryRecord(index int) []byte {
	start := int(v.dictionaryAt) + index*indexTermLeafDictionaryBytes
	return v.encoded[start : start+indexTermLeafDictionaryBytes]
}

func (v IndexTermLeafView) reconstructKey(index int, dst []byte) ([]byte, bool) {
	if index < 0 || index >= int(v.termCount) {
		return dst, false
	}
	dst = dst[:0]
	restart := index - index%IndexTermLeafRestartInterval
	for i := restart; i <= index; i++ {
		record := v.descriptor(i)
		suffixAt := int(binary.LittleEndian.Uint16(record[0:2]))
		keyLength := int(binary.LittleEndian.Uint16(record[4:6]))
		shared := int(binary.LittleEndian.Uint16(record[6:8]))
		suffixLength := int(binary.LittleEndian.Uint16(record[8:10]))
		if shared > len(dst) || shared+suffixLength != keyLength ||
			keyLength > cap(dst) {
			return dst, false
		}
		dst = dst[:keyLength]
		copy(dst[shared:], v.encoded[int(v.keyAt)+suffixAt:int(v.keyAt)+suffixAt+suffixLength])
	}
	return dst, true
}

func (v IndexTermLeafView) matchAt(index int) IndexTermLeafMatch {
	record := v.descriptor(index)
	return IndexTermLeafMatch{
		leaf:  v,
		first: binary.LittleEndian.Uint16(record[2:4]),
		count: binary.LittleEndian.Uint16(record[10:12]),
	}
}

func (v IndexTermLeafView) lowerBound(key []byte) int {
	low, high := 0, int(v.termCount)
	for low < high {
		middle := int(uint(low+high) >> 1)
		if v.compareKeyAt(middle, key) >= 0 {
			high = middle
		} else {
			low = middle + 1
		}
	}
	return low
}

func (v IndexTermLeafView) compareKeyAt(index int, key []byte) int {
	const commonKeyScratch = 256
	restart := index - index%IndexTermLeafRestartInterval
	for i := restart; i <= index; i++ {
		record := v.descriptor(i)
		if binary.LittleEndian.Uint16(record[4:6]) > commonKeyScratch {
			return v.compareLongKeyAt(index, key)
		}
	}
	var scratch [commonKeyScratch]byte
	candidate, ok := v.reconstructKey(index, scratch[:0])
	if !ok {
		return 1
	}
	return bytes.Compare(candidate, key)
}

// exactKeyAt verifies one accelerator candidate without materializing its
// front-coded key. It compares the final suffix, then follows retained-prefix
// ownership backward to the restart. For highly related terms this is usually
// two short byte comparisons rather than several complete key copies.
func (v IndexTermLeafView) exactKeyAt(index int, key []byte) bool {
	if index < 0 || index >= int(v.termCount) {
		return false
	}
	record := v.descriptor(index)
	keyLength := int(binary.LittleEndian.Uint16(record[4:6]))
	if len(key) != keyLength {
		return false
	}
	unresolved := keyLength
	restart := index - index%IndexTermLeafRestartInterval
	for i := index; i >= restart && unresolved != 0; i-- {
		record = v.descriptor(i)
		suffixAt := int(binary.LittleEndian.Uint16(record[0:2]))
		recordKeyLength := int(binary.LittleEndian.Uint16(record[4:6]))
		shared := int(binary.LittleEndian.Uint16(record[6:8]))
		suffixLength := int(binary.LittleEndian.Uint16(record[8:10]))
		end := min(recordKeyLength, unresolved)
		if end > shared {
			source := int(v.keyAt) + suffixAt
			if !bytes.Equal(
				key[shared:end],
				v.encoded[source:source+end-shared],
			) {
				return false
			}
			unresolved = shared
		}
		if suffixLength != recordKeyLength-shared {
			return false
		}
	}
	return unresolved == 0
}

// Keeping the uncommon maximum-key scratch in a separate frame prevents every
// ordinary equality probe from clearing four KiB merely because the format
// admits a four-KiB canonical tuple.
func (v IndexTermLeafView) compareLongKeyAt(index int, key []byte) int {
	var scratch [IndexTermMaxKeyBytes]byte
	candidate, ok := v.reconstructKey(index, scratch[:0])
	if !ok {
		return 1
	}
	return bytes.Compare(candidate, key)
}

func commonIndexTermLeafPrefix(left, right []byte) int {
	n := min(len(left), len(right))
	i := 0
	for i < n && left[i] == right[i] {
		i++
	}
	return i
}

func putIndexTermLeafUvarint(dst []byte, value uint64) int {
	i := 0
	for value >= 0x80 {
		dst[i] = byte(value) | 0x80
		value >>= 7
		i++
	}
	dst[i] = byte(value)
	return i + 1
}

func indexTermLeafUvarintBytes(value uint64) int {
	bytes := 1
	for value >= 0x80 {
		value >>= 7
		bytes++
	}
	return bytes
}

func indexTermLeafEqualitySlots(terms int) int {
	slots := 2
	for terms*4 > slots*3 {
		slots <<= 1
	}
	return slots
}

func indexTermLeafChecksum(encoded []byte) uint32 {
	checksum := crc32.Update(0, indexTermLeafCRC, encoded[:28])
	var zero [4]byte
	checksum = crc32.Update(checksum, indexTermLeafCRC, zero[:])
	return crc32.Update(checksum, indexTermLeafCRC, encoded[32:])
}

func indexTermLeafCorrupt(what string) error {
	return fmt.Errorf("%w: %s", ErrIndexTermLeafCorrupt, what)
}
