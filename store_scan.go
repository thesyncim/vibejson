package slopjson

// Snapshot column gathers used by the query engine. They preserve Store scan
// order while delegating JSON path resolution to each immutable DocSet chunk,
// so shape tapes, compiled keys, value dictionaries, and duplicate-key
// semantics stay centralized in the existing column primitives.

// AppendField appends the top-level object member named name for every live
// row in Store order. An absent member appends a zero RawValue. It is the
// Store-wide form of [ShapeCache.AppendField]: shape and structural-template
// routing are reused independently in every immutable chunk, and values
// borrow s. cache belongs to the calling worker; nil uses an invocation-local
// cache. With sufficient dst capacity the operation allocates nothing.
func (s Snapshot) AppendField(dst []RawValue, name string, cache *ShapeCache) []RawValue {
	if s.state == nil {
		return dst
	}
	var local ShapeCache
	if cache == nil {
		cache = &local
	}
	s.state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		dst = cache.AppendField(dst, &chunk.docs, name)
		return true
	})
	return dst
}

// AppendPointer appends one value per live Snapshot row in chunk/slot order.
// An absent path appends a zero RawValue. Values borrow s. With sufficient dst
// capacity the operation allocates nothing.
func (s Snapshot) AppendPointer(dst []RawValue, pointer CompiledPointer) ([]RawValue, error) {
	mark := len(dst)
	if s.state == nil {
		return dst, nil
	}
	var scanErr error
	s.state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		var err error
		dst, err = chunk.docs.AppendPointer(dst, pointer)
		if err != nil {
			scanErr = err
			return false
		}
		return true
	})
	if scanErr != nil {
		return dst[:mark], scanErr
	}
	return dst, nil
}

// AppendFieldFloat64 appends one typed numeric cell and validity bit per live
// row for a top-level object field. It is the fused Store scan counterpart of
// [ShapeCache.AppendFieldFloat64]: shape routing, span lookup, and number
// parsing happen in one pass without a []RawValue intermediate. cache belongs
// to the calling worker; nil uses an invocation-local cache. With sufficient
// output capacity the operation allocates nothing.
func (s Snapshot) AppendFieldFloat64(dst []float64, valid []bool, name string, cache *ShapeCache) ([]float64, []bool) {
	if s.state == nil {
		return dst, valid
	}
	var local ShapeCache
	if cache == nil {
		cache = &local
	}
	s.state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		dst, valid = cache.AppendFieldFloat64(dst, valid, &chunk.docs, name)
		return true
	})
	return dst, valid
}

// ReduceFieldFloat64 fuses one top-level numeric field scan into aggregate
// state without materializing a RawValue, float, validity, or row column.
// cache has the same ownership contract as AppendFieldFloat64.
func (s Snapshot) ReduceFieldFloat64(name string, cache *ShapeCache) Float64Aggregate {
	var total Float64Aggregate
	if s.state == nil {
		return total
	}
	var local ShapeCache
	if cache == nil {
		cache = &local
	}
	s.state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		part := cache.ReduceFieldFloat64(&chunk.docs, name)
		if part.Count != 0 {
			if total.Count == 0 {
				total.Min, total.Max = part.Min, part.Max
			} else {
				if part.Min < total.Min {
					total.Min = part.Min
				}
				if part.Max > total.Max {
					total.Max = part.Max
				}
			}
			total.Count += part.Count
			total.Sum += part.Sum
		}
		return true
	})
	return total
}

// AppendPointerRows is the sparse-gather form of [Snapshot.AppendPointer].
// Rows may be in any order and may repeat. Invalid or stale addresses panic;
// callers must use rows produced by this Snapshot.
func (s Snapshot) AppendPointerRows(dst []RawValue, rows []StoreRow, pointer CompiledPointer) ([]RawValue, error) {
	mark := len(dst)
	for first := 0; first < len(rows); {
		chunkID := rows[first].Chunk
		chunk := (*storeChunk)(nil)
		if s.state != nil {
			chunk = s.state.chunks.get(chunkID)
		}
		if chunk == nil {
			panic("slopjson: StoreRow chunk is not live in Snapshot")
		}
		var ords [storeMaxChunkDocuments]int
		last := first
		for last < len(rows) && rows[last].Chunk == chunkID && last-first < len(ords) {
			slot := int(rows[last].Slot)
			if slot >= len(chunk.ord) || chunk.live&(uint64(1)<<uint(slot)) == 0 {
				panic("slopjson: StoreRow slot is not live in Snapshot")
			}
			ords[last-first] = int(chunk.ord[slot])
			last++
		}
		var err error
		dst, err = chunk.docs.AppendPointerRows(dst, ords[:last-first], pointer)
		if err != nil {
			return dst[:mark], err
		}
		first = last
	}
	return dst, nil
}

// AppendRowKeys appends the keys addressed by rows. With sufficient capacity
// it allocates nothing.
func (s Snapshot) AppendRowKeys(dst []string, rows []StoreRow) []string {
	for _, row := range rows {
		chunk := (*storeChunk)(nil)
		if s.state != nil {
			chunk = s.state.chunks.get(row.Chunk)
		}
		if chunk == nil || int(row.Slot) >= len(chunk.ord) || chunk.live&(uint64(1)<<row.Slot) == 0 {
			panic("slopjson: StoreRow is not live in Snapshot")
		}
		dst = append(dst, chunk.key(int(row.Slot)))
	}
	return dst
}
