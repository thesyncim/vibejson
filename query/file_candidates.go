package query

import (
	"github.com/thesyncim/vibejson/store"
	"github.com/thesyncim/vibejson/store/durable"
)

// fileCandidateMasks and fileExactCandidateMasks are the durable entry
// points into the shared generic planner (candidates_mask.go). They stay
// concretely typed on *durable.Snapshot/*durable.IndexWorkspace, wrapping
// them in a durable.QuerySnapshot — durable's probes carry extra I/O-
// workspace and stats-accumulator parameters that store.IndexSource's plain
// method set can't express, so this adapter (not the plan-level function
// signatures) is where that gap is bridged.

func (p *plan) fileCandidateMasks(snapshot *durable.Snapshot, index *durable.IndexWorkspace, w *Workspace) ([]store.Mask, error) {
	masks, _, err := snapshotCandidateMasks(p, durable.QuerySnapshot{Snapshot: snapshot, Workspace: index}, w, false)
	return masks, err
}

// fileExactCandidateMasks performs the mandatory collision recheck inside the
// persistent-index probe and returns masks that can be consumed as final
// answers. It is reserved for plans whose complete predicate is statically
// covered; ordinary execution keeps the candidate-only, single-JSON-pass lane.
func (p *plan) fileExactCandidateMasks(snapshot *durable.Snapshot, index *durable.IndexWorkspace, w *Workspace) ([]store.Mask, uint64, uint64, int, bool, error) {
	if p.where == nil {
		return nil, 0, 0, 0, false, nil
	}
	w.storeMaskUsed = 0
	w.storeIndexProbes = 0
	w.storeIndexes = snapshot.AppendIndexes(w.storeIndexes[:0])
	if !p.where.canAnswerExactly(p.valuePaths, w.storeIndexes, w) {
		return nil, 0, 0, 0, false, nil
	}
	var rechecks, certificates uint64
	var postingPages int
	qs := durable.QuerySnapshot{
		Snapshot: snapshot, Workspace: index,
		Rechecks: &rechecks, Certificates: &certificates, PostingPages: &postingPages,
	}
	// requireExact rules the chunk-summary tier out anyway (a zone mask is a
	// superset, and this lane consumes masks as final answers), so no zone
	// source is offered here at all.
	masks, bounded, exact, err := candidatesFor(p.where, qs, nil, p.valuePaths, w.storeIndexes, w, true)
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
