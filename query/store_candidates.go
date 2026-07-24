package query

import (
	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/store"
)

// Declared Store-index binding is deliberately late. A Query is immutable and
// may outlive online index creation, backfill, or drop; each Snapshot carries
// the exact catalog generation against which this execution chooses a plan.

func (p *plan) storeCandidateMasks(snapshot store.Snapshot, w *Workspace) ([]store.Mask, error) {
	masks, _, err := p.storeCandidateMasksMode(snapshot, w, false)
	return masks, err
}

func (p *plan) storeCandidateMasksMode(snapshot store.Snapshot, w *Workspace, requireExact bool) ([]store.Mask, bool, error) {
	if p.where == nil {
		return nil, false, nil
	}
	w.storeMaskUsed = 0
	w.storeIndexProbes = 0
	w.storeIndexes = snapshot.AppendIndexes(w.storeIndexes[:0])
	masks, bounded, exact, err := p.where.storeCandidates(snapshot, p.valuePaths, w.storeIndexes, w, requireExact)
	if err != nil {
		return nil, false, err
	}
	if !bounded {
		return nil, false, nil
	}
	if masks == nil {
		return w.emptyStoreMask[:0], exact, nil
	}
	return masks, exact, nil
}

// storeCandidates is the statically dispatched heap-Snapshot planner lane.
// Keep this concrete rather than routing through an interface: boxing a
// Snapshot makes variadic index needles escape and breaks RunSnapshotInto's
// warmed zero-allocation contract.
func (p *compiledPredicate) storeCandidates(snapshot store.Snapshot, paths []compiledPath, indexes []store.IndexInfo, w *Workspace, requireExact bool) ([]store.Mask, bool, bool, error) {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return nil, false, false, nil
		}
		path := p.indexPath(paths)
		for _, index := range indexes {
			if index.Kind != vibejson.StoreIndexExact || index.State != vibejson.StoreIndexReady || index.ColumnCount != 1 || index.Columns[0] != path {
				continue
			}
			out := w.nextStoreMasks()
			var err error
			if requireExact {
				out, err = snapshot.AppendIndexMasks(out, index.Name, p.needle)
			} else {
				out, err = snapshot.AppendIndexCandidateMasks(out, index.Name, p.needle)
			}
			if err != nil {
				return nil, false, false, err
			}
			w.storeIndexProbes++
			w.keepStoreMasks(out)
			return out, true, requireExact, nil
		}
		return nil, false, false, nil
	case predContains:
		if p.containPlan == nil {
			return nil, false, false, nil
		}
		return p.containPlan.storeCandidates(snapshot, paths, indexes, w, requireExact)
	case predAnd:
		return p.storeAndCandidates(snapshot, paths, indexes, w, requireExact)
	case predOr:
		return p.storeOrCandidates(snapshot, paths, indexes, w, requireExact)
	case predNot:
		if len(p.kids) != 1 {
			return nil, false, false, nil
		}
		// Complementing a candidate superset is unsafe: a hash collision in the
		// child could remove a real NOT match. Force exact leaf rechecks before
		// subtracting from the live universe.
		inner, bounded, exact, err := p.kids[0].storeCandidates(snapshot, paths, indexes, w, true)
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

func (p *compiledPredicate) storeAndCandidates(snapshot store.Snapshot, paths []compiledPath, indexes []store.IndexInfo, w *Workspace, requireExact bool) ([]store.Mask, bool, bool, error) {
	var acc []store.Mask
	have := false
	allExact := true
	var compound store.IndexInfo
	if index, values, ok := p.bestCompoundIndex(paths, indexes); ok {
		compound = index
		acc = w.nextStoreMasks()
		var err error
		if requireExact {
			acc, err = snapshot.AppendIndexMasks(acc, index.Name, values[:index.ColumnCount]...)
		} else {
			acc, err = snapshot.AppendIndexCandidateMasks(acc, index.Name, values[:index.ColumnCount]...)
		}
		if err != nil {
			return nil, false, false, err
		}
		w.storeIndexProbes++
		w.keepStoreMasks(acc)
		have = true
		allExact = requireExact
	}
	for _, kid := range p.kids {
		if kid.coveredEquality(paths, compound) {
			continue
		}
		rows, bounded, exact, err := kid.storeCandidates(snapshot, paths, indexes, w, requireExact)
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

func (p *compiledPredicate) storeOrCandidates(snapshot store.Snapshot, paths []compiledPath, indexes []store.IndexInfo, w *Workspace, requireExact bool) ([]store.Mask, bool, bool, error) {
	var acc []store.Mask
	allExact := true
	for i, kid := range p.kids {
		rows, bounded, exact, err := kid.storeCandidates(snapshot, paths, indexes, w, requireExact)
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

func (p *compiledPredicate) coveredEquality(paths []compiledPath, compound store.IndexInfo) bool {
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

func (p *compiledPredicate) bestCompoundIndex(paths []compiledPath, indexes []store.IndexInfo) (store.IndexInfo, [store.MaxIndexColumns]vibejson.Index, bool) {
	var best store.IndexInfo
	var bestValues [store.MaxIndexColumns]vibejson.Index
	for _, index := range indexes {
		if index.Kind != vibejson.StoreIndexExact || index.State != vibejson.StoreIndexReady || index.ColumnCount < 2 || index.ColumnCount <= best.ColumnCount {
			continue
		}
		var values [store.MaxIndexColumns]vibejson.Index
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

func (p *compiledPredicate) findEquality(path string, paths []compiledPath) (vibejson.Index, bool) {
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
	return vibejson.Index{}, false
}

func intersectStoreMasks(dst, a, b []store.Mask) []store.Mask {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].Chunk < b[j].Chunk:
			i = advanceStoreMasksUntil(a, i, b[j].Chunk)
		case a[i].Chunk > b[j].Chunk:
			j = advanceStoreMasksUntil(b, j, a[i].Chunk)
		default:
			if bits := a[i].Bits & b[j].Bits; bits != 0 {
				dst = append(dst, store.Mask{Chunk: a[i].Chunk, Bits: bits})
			}
			i++
			j++
		}
	}
	return dst
}

func unionStoreMasks(dst, a, b []store.Mask) []store.Mask {
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
			dst = append(dst, store.Mask{Chunk: a[i].Chunk, Bits: a[i].Bits | b[j].Bits})
			i++
			j++
		}
	}
	dst = append(dst, a[i:]...)
	return append(dst, b[j:]...)
}

func andNotStoreMasks(dst, a, b []store.Mask) []store.Mask {
	j := 0
	for _, left := range a {
		if j < len(b) && b[j].Chunk < left.Chunk {
			j = advanceStoreMasksUntil(b, j, left.Chunk)
		}
		bits := left.Bits
		if j < len(b) && b[j].Chunk == left.Chunk {
			bits &^= b[j].Bits
		}
		if bits != 0 {
			dst = append(dst, store.Mask{Chunk: left.Chunk, Bits: bits})
		}
	}
	return dst
}

// advanceStoreMasksUntil returns the first position after pos whose chunk is
// at least target. The immediate next word is checked first; when postings are
// highly skewed an exponential probe brackets the target before binary search.
// This is Roaring's advanceUntil strategy applied to stable chunk words.
// Provenance: ALGO-ROARING-001.
func advanceStoreMasksUntil(masks []store.Mask, pos int, target uint32) int {
	lower := pos + 1
	if lower >= len(masks) || masks[lower].Chunk >= target {
		return lower
	}
	remaining := len(masks) - lower
	// Dense neighbours favour the branch-predictable linear walk. Galloping
	// pays for itself when both the positional and chunk-key distances are
	// large; this is the same adaptive distinction Roaring makes between
	// locally dense and strongly skewed container streams.
	if remaining <= 16 || uint64(target)-uint64(masks[lower].Chunk) <= 8 {
		for lower < len(masks) && masks[lower].Chunk < target {
			lower++
		}
		return lower
	}

	span := 1
	previous := 0
	for span < remaining && masks[lower+span].Chunk < target {
		previous = span
		// Clamp before doubling so an adversarially large slice cannot wrap
		// int and turn a bounds-safe search into an invalid address.
		if span > (remaining-1)/2 {
			span = remaining
			break
		}
		span *= 2
	}
	upper := len(masks) - 1
	if span < remaining {
		upper = lower + span
	}
	if masks[upper].Chunk < target {
		return len(masks)
	}
	if masks[upper].Chunk == target {
		return upper
	}
	lower += previous
	if lower == upper {
		return upper
	}
	for lower+1 < upper {
		middle := lower + (upper-lower)/2
		if masks[middle].Chunk < target {
			lower = middle
		} else {
			upper = middle
		}
	}
	return upper
}
