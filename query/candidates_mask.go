package query

import (
	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/store"
)

// Declared index binding is deliberately late. A Query is immutable and may
// outlive online index creation, backfill, or drop; each snapshot carries
// the exact catalog generation against which this execution chooses a plan.
//
// This file is the mask-based candidate planner shared by every backend
// whose snapshot type satisfies store.IndexSource — today store.Snapshot
// (directly) and store/durable's snapshot type (through durable.QuerySnapshot).
// It replaces what were previously two hand-duplicated planners
// (store_candidates.go and file_candidates.go's AND/OR/Cmp/Contains/Not
// dispatch was ~60% structurally identical between them). The remaining
// per-backend files keep only their genuinely distinct entry points and
// capabilities: store_candidates.go's storeCandidateMasks* wrap
// store.Snapshot directly, file_candidates.go's file* wrap durable's
// workspace-and-stats-carrying probe API.
//
// Needle-scratch discipline: every AppendIndexMasks/AppendIndexCandidateMasks
// call below passes w.needleScratch (a Workspace-owned, reused array),
// never a freshly-built local array or a bare scalar spread with `...`.
// This is not a style preference — verified empirically during this
// redesign, Go's generic dictionary dispatch defeats escape analysis for a
// variadic slice built inside a function generic over an interface type
// parameter, even though the concrete (non-generic) equivalent stack-
// allocates the same construction for free. Passing an already-existing
// slice through the generic call, instead of constructing one inside it,
// keeps these calls at zero allocations.

// snapshotCandidateMasks is the shared entry point: plan p's predicate
// against snapshot's declared index catalog and return the resulting
// stable-slot masks, or a nil, unbounded result when the catalog can't
// answer it. requireExact selects between candidate masks (may still need a
// document recheck) and exact masks (already collision-verified).
func snapshotCandidateMasks[S store.IndexSource](p *plan, snapshot S, w *Workspace, requireExact bool) ([]store.Mask, bool, error) {
	if p.where == nil {
		return nil, false, nil
	}
	w.storeMaskUsed = 0
	w.storeIndexProbes = 0
	w.storeIndexes = snapshot.AppendIndexes(w.storeIndexes[:0])
	masks, bounded, exact, err := candidatesFor(p.where, snapshot, p.valuePaths, w.storeIndexes, w, requireExact)
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

// candidatesFor is the generic Cmp/Contains/And/Or/Not dispatch shared by
// every store.IndexSource-satisfying backend.
func candidatesFor[S store.IndexSource](p *compiledPredicate, snapshot S, paths []compiledPath, indexes []store.IndexInfo, w *Workspace, requireExact bool) ([]store.Mask, bool, bool, error) {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return nil, false, false, nil
		}
		if index, ok := singleColumnIndex(p.indexPath(paths), indexes); ok {
			out := w.nextStoreMasks()
			w.needleScratch[0] = p.needle
			var err error
			if requireExact {
				out, err = snapshot.AppendIndexMasks(out, index.Name, w.needleScratch[:1]...)
			} else {
				out, err = snapshot.AppendIndexCandidateMasks(out, index.Name, w.needleScratch[:1]...)
			}
			if err != nil {
				return nil, false, false, err
			}
			w.storeIndexProbes++
			w.keepStoreMasks(out)
			return out, true, requireExact, nil
		}
		return nil, false, false, nil
	case predIn:
		// The index probes one exact value at a time (its variadic values are
		// a compound key's columns, not alternatives), so a membership costs
		// one probe per alternative, unioned. That is the same probe count a
		// disjunction of equalities would pay; what In saves is on the other
		// side of the probe, where every returned candidate is rechecked by
		// binary search instead of by a walk of the whole set.
		if len(p.needles) == 0 {
			// No alternative, or an alternative with no scalar needle. An
			// empty membership matches nothing, which is a sound and exact
			// bound needing no index at all.
			return nil, len(p.lits) == 0, len(p.lits) == 0, nil
		}
		index, ok := singleColumnIndex(p.indexPath(paths), indexes)
		if !ok {
			return nil, false, false, nil
		}
		var acc []store.Mask
		for i := range p.needles {
			out := w.nextStoreMasks()
			w.needleScratch[0] = p.needles[i]
			var err error
			if requireExact {
				out, err = snapshot.AppendIndexMasks(out, index.Name, w.needleScratch[:1]...)
			} else {
				out, err = snapshot.AppendIndexCandidateMasks(out, index.Name, w.needleScratch[:1]...)
			}
			if err != nil {
				return nil, false, false, err
			}
			w.storeIndexProbes++
			w.keepStoreMasks(out)
			if i == 0 {
				acc = out
				continue
			}
			acc = unionStoreMasks(w.nextStoreMasks(), acc, out)
			w.keepStoreMasks(acc)
		}
		return acc, true, requireExact, nil
	case predContains:
		if p.containPlan == nil {
			return nil, false, false, nil
		}
		return candidatesFor(p.containPlan, snapshot, paths, indexes, w, requireExact)
	case predAnd:
		return andCandidatesFor(p, snapshot, paths, indexes, w, requireExact)
	case predOr:
		for _, kid := range p.kids {
			if !kid.canBound(paths, indexes) {
				return nil, false, false, nil
			}
		}
		return orCandidatesFor(p, snapshot, paths, indexes, w, requireExact)
	case predNot:
		if len(p.kids) != 1 {
			return nil, false, false, nil
		}
		// Complementing against the live universe is a metadata-only operation
		// on a heap Snapshot (LiveMaskSource) but would need real page I/O on
		// durable, so NOT stays unbounded for any backend that can't provide
		// it — matching durable's historical behavior of declining NOT
		// entirely, now expressed as an optional-capability check instead of
		// a hard-coded backend split. Checked before recursing into the
		// child: a backend that can never complete a NOT should decline it
		// at zero cost, not after paying for a child probe it can't use.
		live, ok := any(snapshot).(store.LiveMaskSource)
		if !ok {
			return nil, false, false, nil
		}
		// Complementing a candidate superset is unsafe: a hash collision in the
		// child could remove a real NOT match. Force exact leaf rechecks before
		// subtracting from the live universe.
		inner, bounded, exact, err := candidatesFor(p.kids[0], snapshot, paths, indexes, w, true)
		if err != nil {
			return nil, false, false, err
		}
		if !bounded || !exact {
			return nil, false, false, nil
		}
		liveMasks := live.AppendLiveMasks(w.nextStoreMasks())
		w.keepStoreMasks(liveMasks)
		out := andNotStoreMasks(w.nextStoreMasks(), liveMasks, inner)
		w.keepStoreMasks(out)
		return out, true, true, nil
	default:
		return nil, false, false, nil
	}
}

func andCandidatesFor[S store.IndexSource](p *compiledPredicate, snapshot S, paths []compiledPath, indexes []store.IndexInfo, w *Workspace, requireExact bool) ([]store.Mask, bool, bool, error) {
	var acc []store.Mask
	have := false
	allExact := true
	var compound store.IndexInfo
	if index, count, ok := p.bestCompoundIndexInto(paths, indexes, &w.needleScratch); ok {
		compound = index
		acc = w.nextStoreMasks()
		var err error
		if requireExact {
			acc, err = snapshot.AppendIndexMasks(acc, index.Name, w.needleScratch[:count]...)
		} else {
			acc, err = snapshot.AppendIndexCandidateMasks(acc, index.Name, w.needleScratch[:count]...)
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
		rows, bounded, exact, err := candidatesFor(kid, snapshot, paths, indexes, w, requireExact)
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

func orCandidatesFor[S store.IndexSource](p *compiledPredicate, snapshot S, paths []compiledPath, indexes []store.IndexInfo, w *Workspace, requireExact bool) ([]store.Mask, bool, bool, error) {
	var acc []store.Mask
	allExact := true
	for i, kid := range p.kids {
		rows, bounded, exact, err := candidatesFor(kid, snapshot, paths, indexes, w, requireExact)
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

// singleColumnIndex finds a ready exact index whose only column is path. It is
// the one lookup shared by candidate generation and by both no-I/O planner
// passes, so a leaf's "is this indexable" answer cannot drift from what
// candidate generation would actually probe.
func singleColumnIndex(path string, indexes []store.IndexInfo) (store.IndexInfo, bool) {
	for _, index := range indexes {
		if index.Kind == store.StoreIndexExact && index.State == store.StoreIndexReady &&
			index.ColumnCount == 1 && index.Columns[0] == path {
			return index, true
		}
	}
	return store.IndexInfo{}, false
}

// membershipBounded reports whether an In leaf can be answered from the index
// catalog. An empty membership is bounded without any index — it matches no
// row, and an empty candidate set proves that exactly. Otherwise every
// alternative must carry a scalar needle (compilation drops all of them if any
// one does not, so a partial, unsound bound cannot arise) and the path must
// have a ready single-column exact index.
func (p *compiledPredicate) membershipBounded(paths []compiledPath, indexes []store.IndexInfo) bool {
	if len(p.lits) == 0 {
		return true
	}
	if len(p.needles) != len(p.lits) {
		return false
	}
	_, ok := singleColumnIndex(p.indexPath(paths), indexes)
	return ok
}

// canBound is the no-I/O planner pass: does the declared index catalog
// alone (no snapshot access) let this predicate return a bounded candidate
// set? OR requires every branch to prove usable before any backend attempts
// real work on the first one — a probe that turns out unbounded after a
// sibling already paid its cost (page I/O on durable) is wasted work.
func (p *compiledPredicate) canBound(paths []compiledPath, indexes []store.IndexInfo) bool {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return false
		}
		_, ok := singleColumnIndex(p.indexPath(paths), indexes)
		return ok
	case predIn:
		return p.membershipBounded(paths, indexes)
	case predContains:
		return p.containPlan != nil && p.containPlan.canBound(paths, indexes)
	case predAnd:
		if _, _, ok := p.bestCompoundIndex(paths, indexes); ok {
			return true
		}
		for _, kid := range p.kids {
			if kid.canBound(paths, indexes) {
				return true
			}
		}
		return false
	case predOr:
		if len(p.kids) == 0 {
			return false
		}
		for _, kid := range p.kids {
			if !kid.canBound(paths, indexes) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// canAnswerExactly is the no-I/O proof for a direct indexed-answer lane
// (e.g. an indexed count that never touches JSON): every predicate leaf must
// have a persistent exact probe, with no unbounded residual left for the
// general row evaluator.
func (p *compiledPredicate) canAnswerExactly(paths []compiledPath, indexes []store.IndexInfo) bool {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return false
		}
		_, ok := singleColumnIndex(p.indexPath(paths), indexes)
		return ok
	case predIn:
		return p.membershipBounded(paths, indexes)
	case predContains:
		return p.containPlan != nil && p.containPlan.canAnswerExactly(paths, indexes)
	case predAnd:
		if len(p.kids) == 0 {
			return false
		}
		compound, _, _ := p.bestCompoundIndex(paths, indexes)
		for _, kid := range p.kids {
			if kid.coveredEquality(paths, compound) {
				continue
			}
			if !kid.canAnswerExactly(paths, indexes) {
				return false
			}
		}
		return compound.ColumnCount != 0 || len(p.kids) != 0
	case predOr:
		if len(p.kids) == 0 {
			return false
		}
		for _, kid := range p.kids {
			if !kid.canAnswerExactly(paths, indexes) {
				return false
			}
		}
		return true
	default:
		return false
	}
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

// bestCompoundIndex is the value-returning form used by canBound/
// canAnswerExactly, which only need the chosen index (or just whether one
// exists) and never spread the values through a generic variadic call, so
// the local array it builds is never at risk of the escape this file's
// header documents.
func (p *compiledPredicate) bestCompoundIndex(paths []compiledPath, indexes []store.IndexInfo) (store.IndexInfo, [store.MaxIndexColumns]vibejson.Index, bool) {
	var best store.IndexInfo
	var bestValues [store.MaxIndexColumns]vibejson.Index
	for _, index := range indexes {
		if index.Kind != store.StoreIndexExact || index.State != store.StoreIndexReady || index.ColumnCount < 2 || index.ColumnCount <= best.ColumnCount {
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

// bestCompoundIndexInto is bestCompoundIndex writing into a caller-owned
// buffer (a Workspace's needleScratch) instead of returning a fresh array —
// the form the generic andCandidatesFor uses, so its later
// `dst[:count]...` spread passes an already-existing slice into the
// store.IndexSource call.
func (p *compiledPredicate) bestCompoundIndexInto(paths []compiledPath, indexes []store.IndexInfo, dst *[store.MaxIndexColumns]vibejson.Index) (store.IndexInfo, int, bool) {
	best, bestValues, ok := p.bestCompoundIndex(paths, indexes)
	if !ok {
		return best, 0, false
	}
	*dst = bestValues
	return best, int(best.ColumnCount), true
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
