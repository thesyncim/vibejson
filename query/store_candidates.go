package query

import "github.com/thesyncim/simdjson"

// Declared Store-index binding is deliberately late. A Query is immutable and
// may outlive online index creation, backfill, or drop; each Snapshot carries
// the exact catalog generation against which this execution chooses a plan.

func (p *plan) storeCandidateMasks(snapshot simdjson.Snapshot, w *Workspace) ([]simdjson.StoreMask, error) {
	if p.where == nil {
		return nil, nil
	}
	w.storeMaskUsed = 0
	w.storeIndexProbes = 0
	w.storeIndexes = snapshot.AppendIndexes(w.storeIndexes[:0])
	masks, bounded, _, err := p.where.storeCandidates(snapshot, p.valuePaths, w.storeIndexes, w)
	if err != nil {
		return nil, err
	}
	if !bounded {
		return nil, nil
	}
	if masks == nil {
		return w.emptyStoreMask[:0], nil
	}
	return masks, nil
}

// storeCandidates is the statically dispatched heap-Snapshot planner lane.
// Keep this concrete rather than routing through an interface: boxing a
// Snapshot makes variadic index needles escape and breaks RunSnapshotInto's
// warmed zero-allocation contract.
func (p *compiledPredicate) storeCandidates(snapshot simdjson.Snapshot, paths []compiledPath, indexes []simdjson.StoreIndexInfo, w *Workspace) ([]simdjson.StoreMask, bool, bool, error) {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return nil, false, false, nil
		}
		path := p.indexPath(paths)
		for _, index := range indexes {
			if index.Kind != simdjson.StoreIndexExact || index.State != simdjson.StoreIndexReady || index.ColumnCount != 1 || index.Columns[0] != path {
				continue
			}
			out := w.nextStoreMasks()
			out, err := snapshot.AppendIndexMasks(out, index.Name, p.needle)
			if err != nil {
				return nil, false, false, err
			}
			w.storeIndexProbes++
			w.keepStoreMasks(out)
			return out, true, true, nil
		}
		return nil, false, false, nil
	case predContains:
		if p.containPlan == nil {
			return nil, false, false, nil
		}
		return p.containPlan.storeCandidates(snapshot, paths, indexes, w)
	case predAnd:
		return p.storeAndCandidates(snapshot, paths, indexes, w)
	case predOr:
		return p.storeOrCandidates(snapshot, paths, indexes, w)
	case predNot:
		if len(p.kids) != 1 {
			return nil, false, false, nil
		}
		inner, bounded, exact, err := p.kids[0].storeCandidates(snapshot, paths, indexes, w)
		if err != nil {
			return nil, false, false, err
		}
		if !bounded || !exact {
			return nil, false, false, nil
		}
		live := snapshot.AppendLiveMasks(w.nextStoreMasks())
		w.keepStoreMasks(live)
		out := andNotStoreMasks(w.nextStoreMasks(), live, inner)
		w.keepStoreMasks(out)
		return out, true, true, nil
	default:
		return nil, false, false, nil
	}
}

func (p *compiledPredicate) storeAndCandidates(snapshot simdjson.Snapshot, paths []compiledPath, indexes []simdjson.StoreIndexInfo, w *Workspace) ([]simdjson.StoreMask, bool, bool, error) {
	var acc []simdjson.StoreMask
	have := false
	allExact := true
	var compound simdjson.StoreIndexInfo
	if index, values, ok := p.bestCompoundIndex(paths, indexes); ok {
		compound = index
		acc = w.nextStoreMasks()
		var err error
		acc, err = snapshot.AppendIndexMasks(acc, index.Name, values[:index.ColumnCount]...)
		if err != nil {
			return nil, false, false, err
		}
		w.storeIndexProbes++
		w.keepStoreMasks(acc)
		have = true
		allExact = false
	}
	for _, kid := range p.kids {
		if kid.coveredEquality(paths, compound) {
			continue
		}
		rows, bounded, exact, err := kid.storeCandidates(snapshot, paths, indexes, w)
		if err != nil {
			return nil, false, false, err
		}
		if !bounded {
			allExact = false
			continue
		}
		allExact = allExact && exact
		if !have {
			acc, have = rows, true
			continue
		}
		acc = intersectStoreMasks(w.nextStoreMasks(), acc, rows)
		w.keepStoreMasks(acc)
	}
	if !have {
		return nil, false, false, nil
	}
	return acc, true, allExact, nil
}

func (p *compiledPredicate) storeOrCandidates(snapshot simdjson.Snapshot, paths []compiledPath, indexes []simdjson.StoreIndexInfo, w *Workspace) ([]simdjson.StoreMask, bool, bool, error) {
	var acc []simdjson.StoreMask
	allExact := true
	for i, kid := range p.kids {
		rows, bounded, exact, err := kid.storeCandidates(snapshot, paths, indexes, w)
		if err != nil {
			return nil, false, false, err
		}
		if !bounded {
			return nil, false, false, nil
		}
		allExact = allExact && exact
		if i == 0 {
			acc = rows
			continue
		}
		acc = unionStoreMasks(w.nextStoreMasks(), acc, rows)
		w.keepStoreMasks(acc)
	}
	return acc, true, allExact, nil
}

func (p *compiledPredicate) coveredEquality(paths []compiledPath, compound simdjson.StoreIndexInfo) bool {
	if compound.ColumnCount < 2 || p.kind != predCmp || p.op != Eq {
		return false
	}
	path := p.indexPath(paths)
	for i := 0; i < int(compound.ColumnCount); i++ {
		if compound.Columns[i] == path {
			return true
		}
	}
	return false
}

func (p *compiledPredicate) bestCompoundIndex(paths []compiledPath, indexes []simdjson.StoreIndexInfo) (simdjson.StoreIndexInfo, [simdjson.StoreIndexMaxColumns]simdjson.Index, bool) {
	var best simdjson.StoreIndexInfo
	var bestValues [simdjson.StoreIndexMaxColumns]simdjson.Index
	for _, index := range indexes {
		if index.Kind != simdjson.StoreIndexExact || index.State != simdjson.StoreIndexReady || index.ColumnCount < 2 || index.ColumnCount <= best.ColumnCount {
			continue
		}
		var values [simdjson.StoreIndexMaxColumns]simdjson.Index
		matched := true
		for i := 0; i < int(index.ColumnCount); i++ {
			value, ok := p.findEquality(index.Columns[i], paths)
			if !ok {
				matched = false
				break
			}
			values[i] = value
		}
		if matched {
			best, bestValues = index, values
		}
	}
	return best, bestValues, best.ColumnCount != 0
}

func (p *compiledPredicate) findEquality(path string, paths []compiledPath) (simdjson.Index, bool) {
	if p.kind == predCmp && p.op == Eq && p.indexPath(paths) == path {
		return p.needle, true
	}
	if p.kind == predAnd {
		for _, kid := range p.kids {
			if value, ok := kid.findEquality(path, paths); ok {
				return value, true
			}
		}
	}
	return simdjson.Index{}, false
}

func intersectStoreMasks(dst, a, b []simdjson.StoreMask) []simdjson.StoreMask {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].Chunk < b[j].Chunk:
			i++
		case a[i].Chunk > b[j].Chunk:
			j++
		default:
			if bits := a[i].Bits & b[j].Bits; bits != 0 {
				dst = append(dst, simdjson.StoreMask{Chunk: a[i].Chunk, Bits: bits})
			}
			i++
			j++
		}
	}
	return dst
}

func unionStoreMasks(dst, a, b []simdjson.StoreMask) []simdjson.StoreMask {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].Chunk < b[j].Chunk:
			dst = append(dst, a[i])
			i++
		case a[i].Chunk > b[j].Chunk:
			dst = append(dst, b[j])
			j++
		default:
			dst = append(dst, simdjson.StoreMask{Chunk: a[i].Chunk, Bits: a[i].Bits | b[j].Bits})
			i++
			j++
		}
	}
	dst = append(dst, a[i:]...)
	return append(dst, b[j:]...)
}

func andNotStoreMasks(dst, a, b []simdjson.StoreMask) []simdjson.StoreMask {
	j := 0
	for _, left := range a {
		for j < len(b) && b[j].Chunk < left.Chunk {
			j++
		}
		bits := left.Bits
		if j < len(b) && b[j].Chunk == left.Chunk {
			bits &^= b[j].Bits
		}
		if bits != 0 {
			dst = append(dst, simdjson.StoreMask{Chunk: left.Chunk, Bits: bits})
		}
	}
	return dst
}
