package simdjson

import (
	"reflect"
	"sync"
)

const (
	// Pooled encoder resources have independent byte budgets. Compiled map
	// plans use the lowest applicable element-count limit, so an exceptional
	// map borrows none of the pooled resources and cannot replace warm scratch.
	encoderMapEntriesRetentionBytes   = 512 << 10
	encoderMapKeyArenaRetentionBytes  = 512 << 10
	encoderValueBackingRetentionBytes = 512 << 10
)

func clearEncoderValueBacking(backing reflect.Value, used int) {
	if !backing.IsValid() || used <= 0 {
		return
	}
	if used > backing.Len() {
		used = backing.Len()
	}
	if used == backing.Len() {
		backing.Clear()
	} else {
		// Slicing a reflect.Value allocates a header on the pinned toolchain.
		// Small-after-large cleanup must scale with current use, so clear the
		// populated typed elements directly when only a prefix is dirty.
		for i := 0; i < used; i++ {
			backing.Index(i).SetZero()
		}
	}
}

func encoderValueBackingLimit(elemType reflect.Type) int {
	elemSize := elemType.Size()
	if elemSize == 0 {
		return int(^uint(0) >> 1)
	}
	return int(uintptr(encoderValueBackingRetentionBytes) / elemSize)
}

func encoderMapScratchLimit(elemType reflect.Type) int {
	entryLimit := int(uintptr(encoderMapEntriesRetentionBytes) / reflect.TypeFor[mapEncodeEntry]().Size())
	valueLimit := encoderValueBackingLimit(elemType)
	// Numeric keys use at most 20 bytes each. The entry limit is lower than
	// the key-arena limit at that width, so it also bounds the arena.
	if valueLimit < entryLimit {
		return valueLimit
	}
	return entryLimit
}

type encoderMarshalerScratch struct {
	value reflect.Value
	boxed any
}

type encoderScratch struct {
	marshalers []encoderMarshalerScratch
	// mapEntries, mapKeyArena, and mapIter are reused by encodeMap so
	// sorted map encoding does not allocate per map. Ownership transfers
	// to one encodeMap call at a time; nested maps fall back to fresh
	// allocations.
	mapEntries     []mapEncodeEntry
	mapEntriesUsed int
	mapKeyArena    []byte
	mapIter        *reflect.MapIter
	// valueBacking is the direct fast-path slot for plans with exactly one map
	// element type. Plans with multiple types use valueBackings instead. Every
	// slot is compile-time-assigned and fixed-type. A taken slot is invalid
	// until its owner releases it; recursive use of the same map node therefore
	// falls back to fresh storage without sharing a value box across levels.
	valueBacking  reflect.Value
	valueBackings []reflect.Value
	// dynamicValueBacking is deliberately separate from the statically typed
	// slots. Interface values are compiled independently of the Encoder that
	// reaches them, so their nodes cannot safely carry plan-local slot indexes.
	// This one bounded polymorphic slot preserves reuse without coupling a
	// dynamic plan to another plan's layout or retaining an unbounded type map.
	dynamicValueBacking     reflect.Value
	dynamicValueBackingElem reflect.Type
}

func (s *encoderScratch) reset() {
	for i := range s.marshalers {
		s.marshalers[i].value.SetZero()
	}
	// Clear only the largest prefix populated during this operation. Sequential
	// maps may overwrite it repeatedly, but cleanup happens once before pooling.
	clear(s.mapEntries[:s.mapEntriesUsed])
	s.mapEntriesUsed = 0
	s.mapEntries = s.mapEntries[:0]
	s.mapKeyArena = s.mapKeyArena[:0]
}

func newEncoderScratchPool(types []reflect.Type, backingSlots int, hasMap bool) *sync.Pool {
	if len(types) == 0 && backingSlots == 0 && !hasMap {
		return nil
	}
	types = append([]reflect.Type(nil), types...)
	return &sync.Pool{New: func() any {
		// A non-nil empty slice marks a cold map scratch slot as available.
		// Borrowers replace it with nil, while oversized operations never borrow;
		// this prevents an oversized first encode from seeding the pool.
		scratch := &encoderScratch{
			marshalers: make([]encoderMarshalerScratch, len(types)),
			mapEntries: make([]mapEncodeEntry, 0),
		}
		if backingSlots > 1 {
			scratch.valueBackings = make([]reflect.Value, backingSlots)
		}
		for i, typ := range types {
			value := reflect.New(typ)
			scratch.marshalers[i] = encoderMarshalerScratch{value: value.Elem(), boxed: value.Interface()}
		}
		return scratch
	}}
}
