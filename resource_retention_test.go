package simdjson

import (
	"reflect"
	"runtime"
	"sync"
	"testing"
	"unsafe"
)

func oversizedEncoderMapLen(elemSize uintptr) int {
	entryLimit := encoderMapEntriesRetentionBytes/int(unsafe.Sizeof(mapEncodeEntry{})) + 1
	valueLimit := 1
	if elemSize > 0 {
		valueLimit = encoderValueBackingRetentionBytes/int(elemSize) + 1
	}
	if valueLimit > entryLimit {
		return valueLimit
	}
	return entryLimit
}

func dedicatedEncoderScratch[T any](enc *Encoder[T]) (*encoderScratch, *sync.Pool) {
	scratch := enc.scratch.Get().(*encoderScratch)
	pool := &sync.Pool{New: func() any { return scratch }}
	enc.scratch = pool
	pool.Put(scratch)
	return scratch, pool
}

func assertEncoderScratchBudgets(t *testing.T, scratch *encoderScratch) {
	t.Helper()
	if scratch.mapEntriesUsed != 0 {
		t.Fatalf("map entry scratch retained dirty count %d", scratch.mapEntriesUsed)
	}
	entryBytes := uintptr(cap(scratch.mapEntries)) * unsafe.Sizeof(mapEncodeEntry{})
	if entryBytes > encoderMapEntriesRetentionBytes {
		t.Fatalf("map entry scratch retained %d bytes, budget %d", entryBytes, encoderMapEntriesRetentionBytes)
	}
	if cap(scratch.mapKeyArena) > encoderMapKeyArenaRetentionBytes {
		t.Fatalf("map key arena retained %d bytes, budget %d", cap(scratch.mapKeyArena), encoderMapKeyArenaRetentionBytes)
	}
	assertBacking := func(name string, backing reflect.Value) {
		if !backing.IsValid() {
			return
		}
		bytes := uintptr(backing.Cap()) * backing.Type().Elem().Size()
		if bytes > encoderValueBackingRetentionBytes {
			t.Fatalf("%s retained %d bytes, budget %d", name, bytes, encoderValueBackingRetentionBytes)
		}
	}
	assertBacking("value backing", scratch.valueBacking)
	for i := range scratch.valueBackings {
		assertBacking("typed value backing", scratch.valueBackings[i])
	}
	assertBacking("dynamic value backing", scratch.dynamicValueBacking)
}

func TestEncoderScratchDropsOversizedMap(t *testing.T) {
	t.Run("cold-scratch", func(t *testing.T) {
		enc, err := CompileEncoder[map[int]uint64](EncoderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, pool := dedicatedEncoderScratch(&enc)
		n := oversizedEncoderMapLen(unsafe.Sizeof(uint64(0)))
		huge := make(map[int]uint64, n)
		for i := 0; i < n; i++ {
			huge[i] = uint64(i)
		}
		out, err := enc.AppendJSON(nil, &huge)
		if err != nil {
			t.Fatal(err)
		}
		if !Valid(out) {
			t.Fatal("oversized map produced invalid JSON")
		}
		returned := pool.Get().(*encoderScratch)
		assertEncoderScratchBudgets(t, returned)
		if cap(returned.mapEntries) != 0 || cap(returned.mapKeyArena) != 0 {
			t.Fatalf("oversized map populated cold scratch: entries=%d arena=%d",
				cap(returned.mapEntries), cap(returned.mapKeyArena))
		}
		pool.Put(returned)
	})

	t.Run("pointer-bearing", func(t *testing.T) {
		shared := uint64(7)
		tiny := map[int]*uint64{1: &shared, 2: nil}
		enc, err := CompileEncoder[map[int]*uint64](EncoderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		scratch, pool := dedicatedEncoderScratch(&enc)
		_, err = enc.AppendJSON(make([]byte, 0, 32), &tiny)
		if err != nil {
			t.Fatal(err)
		}
		warmEntries := cap(scratch.mapEntries)
		warmBacking := scratch.valueBacking.Cap()

		n := oversizedEncoderMapLen(unsafe.Sizeof((*uint64)(nil)))
		huge := make(map[int]*uint64, n)
		for i := 0; i < n; i++ {
			huge[i] = &shared
		}
		out, err := enc.AppendJSON(nil, &huge)
		if err != nil {
			t.Fatal(err)
		}
		if !Valid(out) {
			t.Fatal("oversized pointer-bearing map produced invalid JSON")
		}
		huge = nil
		out = nil
		runtime.GC()

		returned := pool.Get().(*encoderScratch)
		if returned != scratch {
			t.Fatal("dedicated pool returned a different scratch object")
		}
		assertEncoderScratchBudgets(t, returned)
		if cap(returned.mapEntries) != warmEntries {
			t.Fatalf("oversized map replaced warm entry scratch: cap=%d, want %d", cap(returned.mapEntries), warmEntries)
		}
		if returned.valueBacking.Cap() != warmBacking {
			t.Fatalf("oversized map replaced warm value backing: cap=%d, want %d", returned.valueBacking.Cap(), warmBacking)
		}
		pool.Put(returned)
	})

	t.Run("pointer-free", func(t *testing.T) {
		tiny := map[int]uint64{1: 7, 2: 9}
		enc, err := CompileEncoder[map[int]uint64](EncoderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		scratch, pool := dedicatedEncoderScratch(&enc)
		_, err = enc.AppendJSON(make([]byte, 0, 32), &tiny)
		if err != nil {
			t.Fatal(err)
		}
		warmEntries := cap(scratch.mapEntries)
		warmArena := cap(scratch.mapKeyArena)
		warmBacking := scratch.valueBacking.Cap()

		n := oversizedEncoderMapLen(unsafe.Sizeof(uint64(0)))
		huge := make(map[int]uint64, n)
		for i := 0; i < n; i++ {
			huge[i] = uint64(i)
		}
		out, err := enc.AppendJSON(nil, &huge)
		if err != nil {
			t.Fatal(err)
		}
		if !Valid(out) {
			t.Fatal("oversized pointer-free map produced invalid JSON")
		}
		huge = nil
		out = nil
		runtime.GC()

		returned := pool.Get().(*encoderScratch)
		assertEncoderScratchBudgets(t, returned)
		if cap(returned.mapEntries) != warmEntries || cap(returned.mapKeyArena) != warmArena {
			t.Fatalf("oversized map replaced warm map scratch: entries=%d/%d arena=%d/%d",
				cap(returned.mapEntries), warmEntries, cap(returned.mapKeyArena), warmArena)
		}
		if returned.valueBacking.Cap() != warmBacking {
			t.Fatalf("oversized map replaced warm value backing: cap=%d, want %d", returned.valueBacking.Cap(), warmBacking)
		}
		pool.Put(returned)
	})
}

func assertEncoderTinyAfterOversizedMapAllocs[V any](t *testing.T, tiny, huge map[int]V) {
	t.Helper()
	enc, err := CompileEncoder[map[int]V](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := enc.AppendJSON(make([]byte, 0, 64), &tiny)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = enc.AppendJSON(nil, &huge); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(200, func() {
		dst, err = enc.AppendJSON(dst[:0], &tiny)
		if err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("tiny map after oversized map allocated %.1f times, want 0", allocs)
	}
	runtime.KeepAlive(dst)
}

func TestEncoderScratchTinyAfterOversizedMapAllocs(t *testing.T) {
	t.Run("pointer-bearing", func(t *testing.T) {
		shared := uint64(7)
		tiny := map[int]*uint64{1: &shared, 2: nil}
		n := oversizedEncoderMapLen(unsafe.Sizeof((*uint64)(nil)))
		huge := make(map[int]*uint64, n)
		for i := 0; i < n; i++ {
			huge[i] = &shared
		}
		assertEncoderTinyAfterOversizedMapAllocs(t, tiny, huge)
	})
	t.Run("pointer-free", func(t *testing.T) {
		tiny := map[int]uint64{1: 7, 2: 9}
		n := oversizedEncoderMapLen(unsafe.Sizeof(uint64(0)))
		huge := make(map[int]uint64, n)
		for i := 0; i < n; i++ {
			huge[i] = uint64(i)
		}
		assertEncoderTinyAfterOversizedMapAllocs(t, tiny, huge)
	})
}

func TestEncoderScratchDropsOversizedDynamicMap(t *testing.T) {
	type dynamicMap map[int]any
	type document struct {
		Anchor  map[int]uint64 `json:"anchor"`
		Dynamic any            `json:"dynamic"`
	}
	tinyDynamic := dynamicMap{1: "one", 2: nil}
	tiny := document{Anchor: map[int]uint64{1: 1}, Dynamic: tinyDynamic}
	enc, err := CompileEncoder[document](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.AppendJSON(make([]byte, 0, 96), &tiny); err != nil {
		t.Fatal(err)
	}

	n := oversizedEncoderMapLen(unsafe.Sizeof(any(nil)))
	hugeMap := make(dynamicMap, n)
	for i := 0; i < n; i++ {
		hugeMap[i] = i
	}
	huge := document{Anchor: tiny.Anchor, Dynamic: hugeMap}
	out, err := enc.AppendJSON(nil, &huge)
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(out) {
		t.Fatal("oversized dynamic map produced invalid JSON")
	}
	huge.Dynamic = nil
	hugeMap = nil
	out = nil

	// Inspect one concrete-type box directly. A sync.Pool may move or discard
	// entries across a GC or race instrumentation, but a box that survives must
	// keep its bounded warm buffers instead of replacing them with an oversized
	// observation.
	entry, err := dynamicEncodeBoxFor(reflect.TypeOf(tinyDynamic), true)
	if err != nil {
		t.Fatal(err)
	}
	box := entry.pool.New().(*dynamicEncodeBox)
	state := encodeState{dst: make([]byte, 0, 96), escapeHTML: true, depth: 1}
	if err := state.encodeMapValue(entry.node, reflect.ValueOf(tinyDynamic), box); err != nil {
		t.Fatal(err)
	}
	warmEntries := cap(box.mapEntries)
	warmArena := cap(box.mapKeyArena)
	warmBacking := box.mapBacking.Cap()

	hugeMap = make(dynamicMap, n)
	for i := 0; i < n; i++ {
		hugeMap[i] = i
	}
	state = encodeState{escapeHTML: true, depth: 1}
	if err := state.encodeMapValue(entry.node, reflect.ValueOf(hugeMap), box); err != nil {
		t.Fatal(err)
	}
	if cap(box.mapEntries) != warmEntries || cap(box.mapKeyArena) != warmArena {
		t.Fatalf("oversized dynamic map replaced warm map scratch: entries=%d/%d arena=%d/%d",
			cap(box.mapEntries), warmEntries, cap(box.mapKeyArena), warmArena)
	}
	if box.mapBacking.Cap() != warmBacking {
		t.Fatalf("oversized dynamic map replaced warm backing: cap=%d, want %d",
			box.mapBacking.Cap(), warmBacking)
	}
}

func TestDynamicEncodeBoxRetentionBound(t *testing.T) {
	type small [64]byte
	type oversized [encoderValueBackingRetentionBytes + 1]byte

	smallEntry, err := dynamicEncodeBoxFor(reflect.TypeFor[small](), true)
	if err != nil {
		t.Fatal(err)
	}
	if !smallEntry.retainBox {
		t.Fatal("small dynamic value box is not reusable")
	}
	oversizedEntry, err := dynamicEncodeBoxFor(reflect.TypeFor[oversized](), true)
	if err != nil {
		t.Fatal(err)
	}
	if oversizedEntry.retainBox {
		t.Fatal("oversized dynamic value box may be retained")
	}
}

// checkEncoderScratchRetentionSequence interleaves bounded maps and checks the
// scratch budgets after every operation in FuzzEncoderScratchOperationSequence.
// Over-budget maps stay in the deterministic retention tests above:
// constructing one from an ordinary mutated byte can prevent a fuzz worker
// from returning before a short smoke deadline, which tests harness scheduling
// rather than a new input property.
func checkEncoderScratchRetentionSequence(t *testing.T, enc Encoder[map[int]uint64], scratch *encoderScratch, pool *sync.Pool, operations []byte) {
	t.Helper()
	for step, operation := range operations {
		size := int(operation & 31)
		value := make(map[int]uint64, size)
		for i := 0; i < size; i++ {
			value[i] = uint64(i + step)
		}
		out, err := enc.AppendJSON(nil, &value)
		if err != nil || !Valid(out) {
			t.Fatalf("step %d size %d = %q, %v", step, size, out, err)
		}
		returned := pool.Get().(*encoderScratch)
		if returned != scratch {
			t.Fatalf("step %d dedicated pool returned a different scratch object", step)
		}
		assertEncoderScratchBudgets(t, returned)
		pool.Put(returned)
	}
	tiny := map[int]uint64{1: 1}
	out, err := enc.AppendJSON(nil, &tiny)
	if err != nil || string(out) != `{"1":1}` {
		t.Fatalf("tiny recovery = %q, %v", out, err)
	}
}

func benchmarkEncodeTinyMap[V any](b *testing.B, tiny map[int]V, huge map[int]V) {
	b.Helper()
	enc, err := CompileEncoder[map[int]V](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	dst, err := enc.AppendJSON(make([]byte, 0, 64), &tiny)
	if err != nil {
		b.Fatal(err)
	}
	if huge != nil {
		if _, err = enc.AppendJSON(nil, &huge); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		dst, err = enc.AppendJSON(dst[:0], &tiny)
		if err != nil {
			b.Fatal(err)
		}
	}
	runtime.KeepAlive(dst)
}

func BenchmarkEncodeTinyAfterHuge(b *testing.B) {
	b.Run("pointer-bearing", func(b *testing.B) {
		shared := uint64(7)
		tiny := map[int]*uint64{1: &shared, 2: nil}
		n := oversizedEncoderMapLen(unsafe.Sizeof((*uint64)(nil)))
		huge := make(map[int]*uint64, n)
		for i := 0; i < n; i++ {
			huge[i] = &shared
		}
		benchmarkEncodeTinyMap(b, tiny, huge)
	})
	b.Run("pointer-free", func(b *testing.B) {
		tiny := map[int]uint64{1: 7, 2: 9}
		n := oversizedEncoderMapLen(unsafe.Sizeof(uint64(0)))
		huge := make(map[int]uint64, n)
		for i := 0; i < n; i++ {
			huge[i] = uint64(i)
		}
		benchmarkEncodeTinyMap(b, tiny, huge)
	})
}
