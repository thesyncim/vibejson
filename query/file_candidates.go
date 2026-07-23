package query

import "github.com/thesyncim/slopjson"

type fileIndexSnapshot struct {
	snapshot     *slopjson.FileSnapshot
	workspace    *slopjson.FileIndexWorkspace
	exact        bool
	rechecks     *uint64
	certificates *uint64
	postingPages *int
}

func (s fileIndexSnapshot) AppendIndexes(dst []slopjson.StoreIndexInfo) []slopjson.StoreIndexInfo {
	return s.snapshot.AppendIndexes(dst)
}

func (s fileIndexSnapshot) AppendIndexMasks(dst []slopjson.StoreMask, name string, values ...slopjson.Index) ([]slopjson.StoreMask, error) {
	var (
		out []slopjson.StoreMask
		err error
	)
	if s.exact {
		out, err = s.snapshot.AppendIndexMasksInto(dst, s.workspace, name, values...)
	} else {
		out, err = s.snapshot.AppendIndexCandidateMasksInto(dst, s.workspace, name, values...)
	}
	if s.rechecks != nil {
		*s.rechecks += s.workspace.LastProbeStats().DocumentRecheckRows
	}
	if s.certificates != nil {
		*s.certificates += s.workspace.LastProbeStats().CertificateRows
	}
	if s.postingPages != nil {
		*s.postingPages += s.workspace.LastProbeStats().PostingPages
	}
	return out, err
}

func (p *plan) fileCandidateMasks(snapshot *slopjson.FileSnapshot, index *slopjson.FileIndexWorkspace, w *Workspace) ([]slopjson.StoreMask, error) {
	return fileCandidateMasksFor(p, fileIndexSnapshot{snapshot: snapshot, workspace: index}, w)
}

// fileExactCandidateMasks performs the mandatory collision recheck inside the
// persistent-index probe and returns masks that can be consumed as final
// answers. It is reserved for plans whose complete predicate is statically
// covered; ordinary execution keeps the candidate-only, single-JSON-pass lane.
func (p *plan) fileExactCandidateMasks(snapshot *slopjson.FileSnapshot, index *slopjson.FileIndexWorkspace, w *Workspace) ([]slopjson.StoreMask, uint64, uint64, int, bool, error) {
	if p.where == nil {
		return nil, 0, 0, 0, false, nil
	}
	w.storeMaskUsed = 0
	w.storeIndexProbes = 0
	w.storeIndexes = snapshot.AppendIndexes(w.storeIndexes[:0])
	if !p.where.fileCanAnswerExactly(p.valuePaths, w.storeIndexes) {
		return nil, 0, 0, 0, false, nil
	}
	var rechecks, certificates uint64
	var postingPages int
	masks, bounded, exact, err := fileCandidatesFor(
		p.where,
		fileIndexSnapshot{
			snapshot: snapshot, workspace: index, exact: true,
			rechecks: &rechecks, certificates: &certificates,
			postingPages: &postingPages,
		},
		p.valuePaths, w.storeIndexes, w,
	)
	if err != nil {
		return nil, rechecks, certificates, postingPages, true, err
	}
	if !bounded || !exact {
		return nil, rechecks, certificates, postingPages, false, nil
	}
	if masks == nil {
		masks = w.emptyStoreMask[:0]
	}
	return masks, rechecks, certificates, postingPages, true, nil
}

func fileCandidateMasksFor(p *plan, snapshot fileIndexSnapshot, w *Workspace) ([]slopjson.StoreMask, error) {
	if p.where == nil {
		return nil, nil
	}
	w.storeMaskUsed = 0
	w.storeIndexProbes = 0
	w.storeIndexes = snapshot.AppendIndexes(w.storeIndexes[:0])
	if !p.where.fileCanBound(p.valuePaths, w.storeIndexes) {
		return nil, nil
	}
	masks, bounded, _, err := fileCandidatesFor(p.where, snapshot, p.valuePaths, w.storeIndexes, w)
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

func fileCandidatesFor(p *compiledPredicate, snapshot fileIndexSnapshot, paths []compiledPath, indexes []slopjson.StoreIndexInfo, w *Workspace) ([]slopjson.StoreMask, bool, bool, error) {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return nil, false, false, nil
		}
		path := p.indexPath(paths)
		for _, index := range indexes {
			if index.Kind != slopjson.StoreIndexExact || index.State != slopjson.StoreIndexReady || index.ColumnCount != 1 || index.Columns[0] != path {
				continue
			}
			out := w.nextStoreMasks()
			out, err := snapshot.AppendIndexMasks(out, index.Name, p.needle)
			if err != nil {
				return nil, false, false, err
			}
			w.storeIndexProbes++
			w.keepStoreMasks(out)
			return out, true, snapshot.exact, nil
		}
		return nil, false, false, nil
	case predContains:
		if p.containPlan == nil {
			return nil, false, false, nil
		}
		return fileCandidatesFor(p.containPlan, snapshot, paths, indexes, w)
	case predAnd:
		return fileAndCandidatesFor(p, snapshot, paths, indexes, w)
	case predOr:
		for _, kid := range p.kids {
			if !kid.fileCanBound(paths, indexes) {
				return nil, false, false, nil
			}
		}
		return fileOrCandidatesFor(p, snapshot, paths, indexes, w)
	default:
		// Durable NOT deliberately stays unbounded. Constructing its exact live
		// complement requires fallible page I/O and is not a zero-cost metadata
		// operation like heap Snapshot.AppendLiveMasks.
		return nil, false, false, nil
	}
}

// fileCanBound is the no-I/O planner pass. In particular, OR must prove every
// branch usable before the first persistent probe; otherwise a full scan would
// pay index I/O that cannot restrict its universe.
func (p *compiledPredicate) fileCanBound(paths []compiledPath, indexes []slopjson.StoreIndexInfo) bool {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return false
		}
		path := p.indexPath(paths)
		for _, index := range indexes {
			if index.Kind == slopjson.StoreIndexExact && index.State == slopjson.StoreIndexReady &&
				index.ColumnCount == 1 && index.Columns[0] == path {
				return true
			}
		}
		return false
	case predContains:
		return p.containPlan != nil && p.containPlan.fileCanBound(paths, indexes)
	case predAnd:
		if _, _, ok := p.bestCompoundIndex(paths, indexes); ok {
			return true
		}
		for _, kid := range p.kids {
			if kid.fileCanBound(paths, indexes) {
				return true
			}
		}
		return false
	case predOr:
		if len(p.kids) == 0 {
			return false
		}
		for _, kid := range p.kids {
			if !kid.fileCanBound(paths, indexes) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// fileCanAnswerExactly is the no-I/O proof for the direct indexed-count lane.
// Every predicate leaf must have a persistent exact probe; no unbounded
// residual may remain for the general JSON evaluator.
func (p *compiledPredicate) fileCanAnswerExactly(paths []compiledPath, indexes []slopjson.StoreIndexInfo) bool {
	switch p.kind {
	case predCmp:
		if p.op != Eq {
			return false
		}
		path := p.indexPath(paths)
		for _, index := range indexes {
			if index.Kind == slopjson.StoreIndexExact && index.State == slopjson.StoreIndexReady &&
				index.ColumnCount == 1 && index.Columns[0] == path {
				return true
			}
		}
		return false
	case predContains:
		return p.containPlan != nil &&
			p.containPlan.fileCanAnswerExactly(paths, indexes)
	case predAnd:
		if len(p.kids) == 0 {
			return false
		}
		compound, _, _ := p.bestCompoundIndex(paths, indexes)
		for _, kid := range p.kids {
			if kid.coveredEquality(paths, compound) {
				continue
			}
			if !kid.fileCanAnswerExactly(paths, indexes) {
				return false
			}
		}
		return compound.ColumnCount != 0 || len(p.kids) != 0
	case predOr:
		if len(p.kids) == 0 {
			return false
		}
		for _, kid := range p.kids {
			if !kid.fileCanAnswerExactly(paths, indexes) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func fileAndCandidatesFor(p *compiledPredicate, snapshot fileIndexSnapshot, paths []compiledPath, indexes []slopjson.StoreIndexInfo, w *Workspace) ([]slopjson.StoreMask, bool, bool, error) {
	var acc []slopjson.StoreMask
	have := false
	allExact := true
	var compound slopjson.StoreIndexInfo
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
		allExact = snapshot.exact
	}
	for _, kid := range p.kids {
		if kid.coveredEquality(paths, compound) {
			continue
		}
		rows, bounded, exact, err := fileCandidatesFor(kid, snapshot, paths, indexes, w)
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

func fileOrCandidatesFor(p *compiledPredicate, snapshot fileIndexSnapshot, paths []compiledPath, indexes []slopjson.StoreIndexInfo, w *Workspace) ([]slopjson.StoreMask, bool, bool, error) {
	var acc []slopjson.StoreMask
	allExact := true
	for i, kid := range p.kids {
		rows, bounded, exact, err := fileCandidatesFor(kid, snapshot, paths, indexes, w)
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
