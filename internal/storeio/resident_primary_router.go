package storeio

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"time"
)

const residentPrimaryRouterWords = 4

// ResidentPrimaryRouter is an allocation-free point router built from one
// published primary graph. Its routing payload is immutable; only per-leaf
// cache-frame hints change. Each leaf occupies four packed words: fence bounds,
// physical offset, generation, and length/bucket identity. Fences share one
// byte arena. Logical IDs and page kind are derived rather than repeated.
//
// A mutation that publishes any catalog, tablet root, anchor, or leaf handle
// must build and publish a replacement router with the new state. The cutover
// graph is read-only, so Open can safely install one router for its lifetime.
type ResidentPrimaryRouter struct {
	storeID [16]byte
	fences  []byte
	rows    []uint64
	hints   []pageCacheFrameHint
	buildNS int64
}

// pageCacheFrameHint is mutable cache-local acceleration beside the router's
// immutable routing payload. packed holds a one-based frame index in its low
// word and an exact-key-derived identity stamp in its high word.
type pageCacheFrameHint struct {
	packed atomic.Uint64
}

// ResidentPrimaryRoute is the exact leaf selection returned by the resident
// router. Hash is carried into the ordered-hash leaf lookup.
type ResidentPrimaryRoute struct {
	Ref    PageRef
	Bucket BucketID
	Hash   uint64
	rank   uint32
}

// BuildResidentPrimaryRouter walks a fully validated published primary graph
// once and copies only its lexical fences and current leaf handles.
func BuildResidentPrimaryRouter(
	cache *PageCache,
	root PageRef,
	bounds GlobalTabletCatalogBounds,
) (*ResidentPrimaryRouter, error) {
	started := time.Now()
	if cache == nil || root == (PageRef{}) || !bounds.valid() {
		return nil, fmt.Errorf("%w: resident primary build bounds",
			ErrGlobalTabletCatalogCorrupt)
	}
	router := &ResidentPrimaryRouter{storeID: bounds.StoreID}
	if err := router.walkCatalog(cache, root, bounds, nil); err != nil {
		return nil, err
	}
	if router.Len() == 0 || len(router.fence(0)) != 0 {
		return nil, fmt.Errorf("%w: empty resident primary router",
			ErrGlobalTabletCatalogCorrupt)
	}
	router.hints = make([]pageCacheFrameHint, router.Len())
	router.buildNS = time.Since(started).Nanoseconds()
	return router, nil
}

func (r *ResidentPrimaryRouter) walkCatalog(
	cache *PageCache,
	ref PageRef,
	bounds GlobalTabletCatalogBounds,
	inheritedFloor []byte,
) error {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return err
	}
	node := AdmittedGlobalTabletCatalogNode(lease.Page(), bounds)
	cursor := node.LowerBound(nil)
	for ordinal := 0; ; ordinal++ {
		route, ok := cursor.Route()
		if !ok {
			lease.Release()
			return fmt.Errorf("%w: resident catalog cursor",
				ErrGlobalTabletCatalogCorrupt)
		}
		floor := inheritedFloor
		if ordinal != 0 {
			common, prefix, suffix := node.floors.fenceParts(ordinal - 1)
			floor = make([]byte, 0, len(common)+len(prefix)+len(suffix))
			floor = append(floor, common...)
			floor = append(floor, prefix...)
			floor = append(floor, suffix...)
		}
		switch node.Level() {
		case GlobalTabletCatalogLeaf:
			if err := r.walkTablet(cache, route.Ref, bounds, floor); err != nil {
				lease.Release()
				return err
			}
		case GlobalTabletCatalogRoot, GlobalTabletCatalogBranch:
			if err := r.walkCatalog(cache, route.Ref, bounds, floor); err != nil {
				lease.Release()
				return err
			}
		default:
			lease.Release()
			return fmt.Errorf("%w: resident catalog level",
				ErrGlobalTabletCatalogCorrupt)
		}
		if !cursor.Next() {
			break
		}
	}
	lease.Release()
	return nil
}

func (r *ResidentPrimaryRouter) walkTablet(
	cache *PageCache,
	ref PageRef,
	bounds GlobalTabletCatalogBounds,
	tabletFloor []byte,
) error {
	tabletLease, err := cache.Acquire(ref)
	if err != nil {
		return err
	}
	tablet := AdmittedGlobalTabletCatalogTabletRoot(tabletLease.Page(), bounds)
	leafRank := 0
	for anchorRank := 0; anchorRank < tablet.AnchorCount(); anchorRank++ {
		anchorRoute, ok := tablet.AnchorAt(anchorRank)
		if !ok {
			tabletLease.Release()
			return fmt.Errorf("%w: resident anchor route",
				ErrGlobalTabletCatalogCorrupt)
		}
		anchorLease, acquireErr := cache.Acquire(anchorRoute.Ref)
		if acquireErr != nil {
			tabletLease.Release()
			return acquireErr
		}
		anchor := AdmittedGlobalTabletCatalogAnchor(
			anchorLease.Page(), &tablet, anchorRoute.PageID,
		)
		for rank := 0; rank < anchor.Count(); rank++ {
			route, routeOK := anchor.RouteAt(rank, 0)
			fence, fenceOK := anchor.page.fenceAtChecked(rank)
			if !routeOK || !fenceOK {
				anchorLease.Release()
				tabletLease.Release()
				return fmt.Errorf("%w: resident anchor row",
					ErrSegmentedTabletRouterCorrupt)
			}
			start := len(r.fences)
			if leafRank == 0 {
				r.fences = append(r.fences, tabletFloor...)
			} else {
				r.fences = append(r.fences, fence.a...)
				r.fences = append(r.fences, fence.b...)
				r.fences = append(r.fences, fence.c...)
			}
			if len(r.fences) > int(^uint32(0)) ||
				len(r.fences)-start > int(^uint32(0)) {
				anchorLease.Release()
				tabletLease.Release()
				return fmt.Errorf("%w: resident fence arena",
					ErrSegmentedTabletRouterCorrupt)
			}
			if r.Len() != 0 &&
				bytes.Compare(r.fence(r.Len()-1), r.fences[start:]) >= 0 {
				anchorLease.Release()
				tabletLease.Release()
				return fmt.Errorf("%w: resident fence order",
					ErrSegmentedTabletRouterCorrupt)
			}
			r.rows = append(r.rows,
				uint64(uint32(start))|uint64(uint32(len(r.fences)))<<32,
				route.Ref.Offset,
				route.Ref.Generation,
				uint64(route.Ref.Length)|uint64(uint32(route.Bucket))<<32,
			)
			leafRank++
		}
		anchorLease.Release()
	}
	tabletLease.Release()
	if leafRank == 0 {
		return fmt.Errorf("%w: resident empty tablet",
			ErrSegmentedTabletRouterCorrupt)
	}
	return nil
}

// Route hashes key once, confirms its exact lexical interval, and returns the
// current leaf handle. It allocates no memory.
func (r *ResidentPrimaryRouter) Route(key []byte) (ResidentPrimaryRoute, bool) {
	if r == nil || len(r.rows) == 0 {
		return ResidentPrimaryRoute{}, false
	}
	hash := KeyHashBytes(r.storeID, key)
	low, high := 1, r.Len()
	for low < high {
		middle := int(uint(low+high) >> 1)
		if bytes.Compare(r.fence(middle), key) <= 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	rank := low - 1
	if rank < 0 || bytes.Compare(r.fence(rank), key) > 0 ||
		rank+1 < r.Len() && bytes.Compare(key, r.fence(rank+1)) >= 0 {
		return ResidentPrimaryRoute{}, false
	}
	at := rank * residentPrimaryRouterWords
	meta := r.rows[at+3]
	bucket := BucketID(uint32(meta >> 32))
	logicalID, ok := CommonPrimaryLeafLogicalID(bucket)
	if !ok {
		return ResidentPrimaryRoute{}, false
	}
	return ResidentPrimaryRoute{
		Ref: PageRef{
			Offset: r.rows[at+1], LogicalID: logicalID,
			Generation: r.rows[at+2], Length: uint32(meta),
			Kind: PagePrimaryLeaf,
		},
		Bucket: bucket,
		Hash:   hash,
		rank:   uint32(rank),
	}, true
}

// AcquireLeaf pins route's selected leaf, consulting its per-router frame hint
// before the cache table. A stale hint is harmless: PageCache rechecks the
// complete PageRef identity while holding the frame's existing pin lock.
func (r *ResidentPrimaryRouter) AcquireLeaf(
	cache *PageCache,
	route ResidentPrimaryRoute,
) (PageLease, error) {
	if r == nil || cache == nil || int(route.rank) >= len(r.hints) {
		if cache == nil {
			return PageLease{}, ErrPageCacheReference
		}
		return cache.Acquire(route.Ref)
	}
	return cache.acquireFrameHinted(route.Ref, &r.hints[route.rank])
}

func (r *ResidentPrimaryRouter) fence(rank int) []byte {
	word := r.rows[rank*residentPrimaryRouterWords]
	return r.fences[uint32(word):uint32(word>>32)]
}

func (r *ResidentPrimaryRouter) Len() int {
	if r == nil {
		return 0
	}
	return len(r.rows) / residentPrimaryRouterWords
}

// ResidentBytes is the exact packed payload capacity retained by the router.
func (r *ResidentPrimaryRouter) ResidentBytes() int {
	if r == nil {
		return 0
	}
	return cap(r.fences) + cap(r.rows)*8 + cap(r.hints)*8
}

// BuildDuration reports the wall time spent walking and packing the graph,
// including PageCache acquisitions made by the Open-time build.
func (r *ResidentPrimaryRouter) BuildDuration() time.Duration {
	if r == nil {
		return 0
	}
	return time.Duration(r.buildNS)
}
