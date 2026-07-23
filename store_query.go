package simdjson

import "math/bits"

// Snapshot posting probes expose the online index without exposing mutable
// DocSet internals. Every probe remains exact while an index is building or
// being reclaimed: indexed chunks use postings and uncovered chunks use the
// existing scan fallback. Results are ordered by stable chunk/slot position.

type storeProbeKind uint8

const (
	storeProbeExists storeProbeKind = iota + 1
	storeProbeContains
)

// WhereExistsKeys returns the keys whose root object contains path. path is a
// decoded top-level member name, matching [DocSet.WhereExists].
func (s Snapshot) WhereExistsKeys(path string) []string {
	return s.AppendWhereExistsKeys(nil, path)
}

// AppendWhereExistsKeys is [Snapshot.WhereExistsKeys] with caller-owned
// result storage. It appends to dst and, given sufficient capacity, performs
// no heap allocation on either the posting or scan path.
func (s Snapshot) AppendWhereExistsKeys(dst []string, path string) []string {
	return s.appendWhereKeys(dst, storeProbeExists, path, Index{})
}

// WhereContainsKeys returns the keys whose top-level value at path contains
// needle according to [Node.Contains]. Invalid JSON returns an error.
func (s Snapshot) WhereContainsKeys(path string, needle []byte) ([]string, error) {
	return s.AppendWhereContainsKeys(nil, path, needle)
}

// AppendWhereContainsKeys is [Snapshot.WhereContainsKeys] with caller-owned
// result storage. It leaves dst unchanged when needle is invalid. Repeated hot
// paths should build the needle once and call AppendWhereContainsIndexKeys.
func (s Snapshot) AppendWhereContainsKeys(dst []string, path string, needle []byte) ([]string, error) {
	index, err := containsIndex(needle)
	if err != nil {
		return dst, err
	}
	return s.AppendWhereContainsIndexKeys(dst, path, index), nil
}

// WhereContainsIndexKeys returns the keys whose top-level value at path
// contains needle according to [Node.Contains]. A prebuilt needle avoids parse
// and allocation work across repeated calls.
func (s Snapshot) WhereContainsIndexKeys(path string, needle Index) []string {
	return s.AppendWhereContainsIndexKeys(nil, path, needle)
}

// AppendWhereContainsIndexKeys is [Snapshot.WhereContainsIndexKeys] with
// caller-owned result storage. With sufficient dst capacity it allocates
// nothing. Build needle once with caller-owned IndexEntry storage when the
// complete operation must be allocation-free.
func (s Snapshot) AppendWhereContainsIndexKeys(dst []string, path string, needle Index) []string {
	return s.appendWhereKeys(dst, storeProbeContains, path, needle)
}

func (s Snapshot) appendWhereKeys(dst []string, kind storeProbeKind, path string, needle Index) []string {
	if s.state == nil {
		return dst
	}
	s.state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		var storage [storeMaxChunkDocuments]int
		var rows []int
		switch kind {
		case storeProbeExists:
			rows = chunk.docs.AppendWhereExists(storage[:0], path)
		case storeProbeContains:
			rows = chunk.docs.AppendWhereContainsIndex(storage[:0], path, needle)
		}
		if len(rows) == 0 {
			return true
		}
		// A rebuilt DocSet is dense while Store slots are stable and may be
		// sparse. Invert the at-most-64 ordinal map on the stack.
		var slots [storeMaxChunkDocuments]uint8
		for live := chunk.live; live != 0; live &= live - 1 {
			slot := bits.TrailingZeros64(live)
			slots[chunk.ord[slot]] = uint8(slot)
		}
		for _, row := range rows {
			dst = append(dst, chunk.key(int(slots[row])))
		}
		return true
	})
	return dst
}

// AppendWhereExistsKeys probes the current snapshot. See the Snapshot method.
func (s *Store) AppendWhereExistsKeys(dst []string, path string) []string {
	return s.Snapshot().AppendWhereExistsKeys(dst, path)
}

// WhereExistsKeys probes the current snapshot. See the Snapshot method.
func (s *Store) WhereExistsKeys(path string) []string {
	return s.Snapshot().WhereExistsKeys(path)
}

// AppendWhereContainsKeys probes the current snapshot. See the Snapshot
// method.
func (s *Store) AppendWhereContainsKeys(dst []string, path string, needle []byte) ([]string, error) {
	return s.Snapshot().AppendWhereContainsKeys(dst, path, needle)
}

// WhereContainsKeys probes the current snapshot. See the Snapshot method.
func (s *Store) WhereContainsKeys(path string, needle []byte) ([]string, error) {
	return s.Snapshot().WhereContainsKeys(path, needle)
}

// AppendWhereContainsIndexKeys probes the current snapshot. See the Snapshot
// method.
func (s *Store) AppendWhereContainsIndexKeys(dst []string, path string, needle Index) []string {
	return s.Snapshot().AppendWhereContainsIndexKeys(dst, path, needle)
}

// WhereContainsIndexKeys probes the current snapshot. See the Snapshot method.
func (s *Store) WhereContainsIndexKeys(path string, needle Index) []string {
	return s.Snapshot().WhereContainsIndexKeys(path, needle)
}
