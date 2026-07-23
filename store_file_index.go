package slopjson

import (
	"bytes"
	"math/bits"

	"github.com/thesyncim/slopjson/document"
	"github.com/thesyncim/slopjson/internal/storeio"
)

// AppendIndexes appends the frozen exact-index catalog visible to this file
// snapshot. FileStore indexes are complete from generation one and therefore
// always report Ready.
func (s *FileSnapshot) AppendIndexes(dst []StoreIndexInfo) []StoreIndexInfo {
	if s == nil || s.store == nil || s.state == nil {
		return dst
	}
	for _, definition := range s.store.options.Indexes {
		info := StoreIndexInfo{
			Name: definition.Name, Kind: StoreIndexExact, State: StoreIndexReady,
			TotalChunks: s.state.root.LiveChunks, CoveredChunks: s.state.root.LiveChunks,
			ColumnCount: uint8(len(definition.Paths)),
		}
		copy(info.Columns[:], definition.Paths)
		dst = append(dst, info)
	}
	return dst
}

// AppendIndexMasks appends exact stable-slot masks for a frozen FileStore
// index. A collision-free posting certificate decides the complete stream
// without opening JSON. Legacy, missing, oversized, or collision-marked
// certificates fall back to exact document recheck.
func (s *FileSnapshot) AppendIndexMasks(dst []StoreMask, name string, values ...Index) ([]StoreMask, error) {
	var workspace FileIndexWorkspace
	return s.AppendIndexMasksInto(dst, &workspace, name, values...)
}

// AppendIndexMasksInto is AppendIndexMasks with reusable transient storage.
// A tuple hash alone is never a final answer: the probe either verifies the
// stream's exact scalar/compound certificate or compares every candidate
// document. With sufficient dst and workspace capacity, a warmed cache-hit
// probe allocates nothing.
func (s *FileSnapshot) AppendIndexMasksInto(dst []StoreMask, workspace *FileIndexWorkspace, name string, values ...Index) ([]StoreMask, error) {
	if s == nil || s.store == nil || s.state == nil {
		return dst, ErrFileStoreClosed
	}
	if workspace == nil {
		var local FileIndexWorkspace
		workspace = &local
	}
	workspace.lastProbe = FileIndexProbeStats{}
	probe, err := s.prepareFileIndexProbe(workspace, name, values)
	if err != nil {
		return dst, err
	}
	if err := s.loadFileIndexPostings(workspace, probe, values, true); err != nil {
		return dst, err
	}
	for _, decision := range workspace.postings {
		posting := decision.posting
		workspace.lastProbe.CandidateRows += uint64(bits.OnesCount64(posting.Bits))
		workspace.lastProbe.CandidateChunks++
		if decision.flags&fileIndexProbeCertified != 0 {
			workspace.lastProbe.CertificateRows += uint64(bits.OnesCount64(posting.Bits))
			if decision.flags&fileIndexProbeCertificateMatch != 0 {
				workspace.lastProbe.MatchedRows += uint64(bits.OnesCount64(posting.Bits))
				dst = append(dst, StoreMask{Chunk: posting.Chunk, Bits: posting.Bits})
			}
			continue
		}
		workspace.lastProbe.DocumentRecheckRows += uint64(bits.OnesCount64(posting.Bits))
		documentRef, ok, lookupErr := storeio.LookupChunkTree(s.store.cache, probe.state.chunkRoot, posting.Chunk, storeio.ChunkTreeBounds{
			FileEnd: probe.state.super.FileEnd, NextLogicalID: probe.state.root.NextLogicalID,
		})
		if lookupErr != nil {
			return dst, lookupErr
		}
		if !ok {
			return dst, storeio.ErrPostingPageCorrupt
		}
		documentLease, acquireErr := s.store.cache.Acquire(documentRef)
		if acquireErr != nil {
			return dst, acquireErr
		}
		documentPage, viewErr := admittedFileDocumentChunk(
			documentLease.Page(), documentRef, posting.Chunk,
		)
		if viewErr != nil {
			documentLease.Release()
			return dst, viewErr
		}
		verified := uint64(0)
		for bitsLeft := posting.Bits; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
			slot := uint8(bits.TrailingZeros64(bitsLeft))
			record, present := documentPage.lookup(slot)
			if !present {
				documentLease.Release()
				return dst, storeio.ErrPostingPageCorrupt
			}
			workspace.document = workspace.document[:0]
			workspace.document, err = s.store.appendFileDocumentValue(
				workspace.document, probe.state, documentPage, record.value,
				storeio.KeyLocation{Chunk: posting.Chunk, Slot: slot},
			)
			if err != nil {
				documentLease.Release()
				return dst, err
			}
			matches := true
			if probe.exact.n == 1 {
				var raw RawValue
				var found bool
				var pointerErr error
				if record.value.grouped || record.value.value.Overflow == (storeio.PageRef{}) {
					raw, found, pointerErr = probe.exact.paths[0].getRawTrusted(workspace.document)
				} else {
					raw, found, pointerErr = probe.exact.paths[0].GetRaw(workspace.document)
				}
				if pointerErr != nil {
					documentLease.Release()
					return dst, pointerErr
				}
				matches = found && fileIndexRawScalarEqual(raw, values[0].Root())
			} else {
				needed, countErr := RequiredIndexEntries(workspace.document)
				if countErr != nil {
					documentLease.Release()
					return dst, countErr
				}
				if cap(workspace.tape) < needed {
					workspace.tape = make([]IndexEntry, needed)
				}
				index, buildErr := BuildIndexOptions(
					workspace.document, workspace.tape[:needed],
					s.store.options.Store.IndexOptions,
				)
				if buildErr != nil {
					documentLease.Release()
					return dst, buildErr
				}
				for column := 0; column < int(probe.exact.n); column++ {
					node, found, pointerErr := index.PointerCompiled(probe.exact.paths[column])
					if pointerErr != nil || !found ||
						!node.Contains(values[column].Root()) ||
						!values[column].Root().Contains(node) {
						matches = false
						break
					}
				}
			}
			if matches {
				verified |= uint64(1) << slot
			}
		}
		documentLease.Release()
		if verified != 0 {
			workspace.lastProbe.MatchedRows += uint64(bits.OnesCount64(verified))
			dst = append(dst, StoreMask{Chunk: posting.Chunk, Bits: verified})
		}
	}
	return dst, nil
}

// fileIndexRawScalarEqual is the collision verifier for a single-column exact
// index. The raw seeker has already validated the complete document and
// resolved duplicate keys with last-wins semantics. Comparing the borrowed
// scalar directly avoids constructing a full document tape while retaining
// the same exact value relation as mutual Node.Contains.
func fileIndexRawScalarEqual(raw RawValue, needle Node) bool {
	return fileIndexRawValuesEqual(raw, needle.Raw())
}

func fileIndexRawValuesEqual(left, right RawValue) bool {
	if left.Kind() != right.Kind() {
		return false
	}
	switch left.Kind() {
	case document.Invalid:
		return false
	case document.Null:
		return true
	case document.Bool:
		leftValue, leftOK := left.Bool()
		rightValue, rightOK := right.Bool()
		return leftOK && rightOK && leftValue == rightValue
	case document.Number:
		leftNumber, leftOK := left.NumberBytes()
		rightNumber, rightOK := right.NumberBytes()
		return leftOK && rightOK && jsonNumberEqual(leftNumber, rightNumber)
	case document.String:
		leftRaw := left.Bytes()
		rightRaw := right.Bytes()
		leftFlags := uint8(0)
		if bytes.IndexByte(leftRaw, '\\') >= 0 {
			leftFlags = tapeFlagEscaped
		}
		rightFlags := uint8(0)
		if bytes.IndexByte(rightRaw, '\\') >= 0 {
			rightFlags = tapeFlagEscaped
		}
		return rawJSONStringEqual(leftRaw, leftFlags, rightRaw, rightFlags)
	default:
		return false
	}
}

// AppendIndexCandidateMasks appends hash-bounded stable-slot candidates
// without reopening documents. It may return false positives and must be
// followed by an exact predicate recheck; it never turns a non-match into a
// public query result by itself. This lane exists for query engines that will
// immediately parse every candidate and avoids reading each document twice.
func (s *FileSnapshot) AppendIndexCandidateMasks(dst []StoreMask, name string, values ...Index) ([]StoreMask, error) {
	var workspace FileIndexWorkspace
	return s.AppendIndexCandidateMasksInto(dst, &workspace, name, values...)
}

// AppendIndexCandidateMasksInto is AppendIndexCandidateMasks with reusable
// directory storage. The returned masks are ordered, non-zero posting
// candidates, not exact answers.
func (s *FileSnapshot) AppendIndexCandidateMasksInto(dst []StoreMask, workspace *FileIndexWorkspace, name string, values ...Index) ([]StoreMask, error) {
	if s == nil || s.store == nil || s.state == nil {
		return dst, ErrFileStoreClosed
	}
	if workspace == nil {
		var local FileIndexWorkspace
		workspace = &local
	}
	workspace.lastProbe = FileIndexProbeStats{}
	probe, err := s.prepareFileIndexProbe(workspace, name, values)
	if err != nil {
		return dst, err
	}
	if err := s.loadFileIndexPostings(workspace, probe, nil, false); err != nil {
		return dst, err
	}
	for _, decision := range workspace.postings {
		posting := decision.posting
		workspace.lastProbe.CandidateRows += uint64(bits.OnesCount64(posting.Bits))
		workspace.lastProbe.CandidateChunks++
		dst = append(dst, StoreMask{Chunk: posting.Chunk, Bits: posting.Bits})
	}
	return dst, nil
}

type fileIndexProbe struct {
	state   *fileStoreState
	exact   *storeExactIndex
	indexID uint32
	hash    uint64
}

type fileIndexProbePosting struct {
	posting storeio.PostingEntry
	flags   uint8
}

const (
	fileIndexProbeCertified uint8 = 1 << iota
	fileIndexProbeCertificateMatch
)

func (s *FileSnapshot) prepareFileIndexProbe(workspace *FileIndexWorkspace, name string, values []Index) (fileIndexProbe, error) {
	indexID := -1
	for i, definition := range s.store.options.Indexes {
		if definition.Name == name {
			indexID = i
			break
		}
	}
	if indexID < 0 {
		return fileIndexProbe{}, ErrStoreIndexNotFound
	}
	exact := s.store.options.indexes[indexID]
	hash, err := fileIndexNeedleHash(exact, values)
	if err != nil {
		return fileIndexProbe{}, err
	}
	state := s.state
	probe := fileIndexProbe{state: state, exact: exact, indexID: uint32(indexID), hash: hash}
	workspace.directory = workspace.directory[:0]
	if state.indexRoot == (storeio.PageRef{}) {
		return probe, nil
	}
	if uint64(state.root.LiveChunks) > uint64(^uint(0)>>1) {
		return fileIndexProbe{}, ErrStoreTooLarge
	}
	workspace.directory, err = storeio.AppendIndexTreeHash(
		s.store.cache, state.indexRoot, probe.indexID, hash,
		storeio.IndexTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
			IndexHighWater: state.root.IndexCount,
		}, workspace.directory, int(state.root.LiveChunks),
	)
	if err != nil {
		return fileIndexProbe{}, err
	}
	return probe, nil
}

// loadFileIndexPostings coalesces consecutive directory entries that select
// the same immutable packed page. The retained decisions preserve directory
// order and let exact fallbacks release the posting lease before opening a
// document page. Online delta pages naturally form one-entry groups.
func (s *FileSnapshot) loadFileIndexPostings(
	workspace *FileIndexWorkspace,
	probe fileIndexProbe,
	values []Index,
	verifyCertificate bool,
) error {
	workspace.postings = workspace.postings[:0]
	for first := 0; first < len(workspace.directory); {
		ref := workspace.directory[first].Posting.Page
		postingLease, err := s.store.cache.Acquire(ref)
		if err != nil {
			return err
		}
		postingPage, err := storeio.OpenPostingPage(
			postingLease.Page(), probe.state.root.NextLogicalID,
			probe.state.root.IndexCount,
		)
		if err != nil {
			postingLease.Release()
			return err
		}
		workspace.lastProbe.PostingPages++
		last := first
		for last < len(workspace.directory) &&
			workspace.directory[last].Posting.Page == ref {
			posting, certified, certificateMatch, postingErr :=
				fileIndexPostingFromPage(
					probe, postingPage, workspace.directory[last],
					values, verifyCertificate,
				)
			if postingErr != nil {
				postingLease.Release()
				return postingErr
			}
			flags := uint8(0)
			if certified {
				flags |= fileIndexProbeCertified
			}
			if certificateMatch {
				flags |= fileIndexProbeCertificateMatch
			}
			workspace.postings = append(workspace.postings, fileIndexProbePosting{
				posting: posting, flags: flags,
			})
			last++
		}
		postingLease.Release()
		first = last
	}
	return nil
}

func fileIndexPostingFromPage(
	probe fileIndexProbe,
	postingPage storeio.PostingPageView,
	directoryEntry storeio.IndexDirectoryEntry,
	values []Index,
	verifyCertificate bool,
) (storeio.PostingEntry, bool, bool, error) {
	segment, ok := postingPage.SegmentAt(int(directoryEntry.Posting.Segment))
	if !ok || postingPage.Header().IndexID != probe.indexID || segment.Header().TupleHash != probe.hash {
		return storeio.PostingEntry{}, false, false, storeio.ErrPostingPageCorrupt
	}
	iterator := segment.Iterator()
	posting, ok := iterator.Next()
	certified := false
	certificateMatch := false
	if verifyCertificate &&
		segment.Header().Flags&storeio.PostingSegmentCollision == 0 &&
		len(segment.Certificate()) != 0 {
		certificate := RawValue{src: segment.Certificate()}
		if !fileIndexCertificateValid(certificate.Bytes(), int(probe.exact.n)) {
			return storeio.PostingEntry{}, false, false, storeio.ErrPostingPageCorrupt
		}
		certified = true
		certificateMatch = fileIndexCertificateMatches(
			certificate.Bytes(), values, int(probe.exact.n),
		)
	}
	if !ok || posting.Chunk != directoryEntry.Key.Chunk {
		return storeio.PostingEntry{}, false, false, storeio.ErrPostingPageCorrupt
	}
	return posting, certified, certificateMatch, nil
}

func fileIndexCertificateScalar(raw RawValue) bool {
	switch raw.Kind() {
	case document.Null, document.Bool, document.Number, document.String:
		return true
	default:
		return false
	}
}

// AppendIndexMasks acquires a temporary snapshot and returns exact masks. Hot
// callers should retain a FileSnapshot and FileIndexWorkspace instead.
func (s *FileStore) AppendIndexMasks(dst []StoreMask, name string, values ...Index) ([]StoreMask, error) {
	snapshot, err := s.Snapshot()
	if err != nil {
		return dst, err
	}
	defer snapshot.Close()
	return snapshot.AppendIndexMasks(dst, name, values...)
}
