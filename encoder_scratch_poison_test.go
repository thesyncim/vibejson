package slopjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

type scratchPoisonMarshaler struct {
	Text string
}

func (m scratchPoisonMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.Text)
}

type scratchPoisonDoc struct {
	Ints    map[string]int         `json:"ints"`
	Strings map[string]string      `json:"strings"`
	Ptrs    map[string]*uint64     `json:"ptrs"`
	Dynamic any                    `json:"dynamic"`
	Custom  scratchPoisonMarshaler `json:"custom"`
}

// TestEncoderScratchPoolPoisoning seeds every pooled encoder-scratch surface
// with stale values, moves the goroutine stack, and runs a GC before reuse.
// The next encode must overwrite the poison, produce the stdlib-equivalent
// result, unbind the iterator, and clear every reference-bearing slot before
// returning the scratch to its pool.
func TestEncoderScratchPoolPoisoning(t *testing.T) {
	a, c := uint64(11), uint64(33)
	v := scratchPoisonDoc{
		Ints:    map[string]int{"a": 1, "b": 2, "c": 3},
		Strings: map[string]string{"a": "one", "b": "two", "c": "three"},
		Ptrs:    map[string]*uint64{"a": &a, "b": nil, "c": &c},
		Dynamic: map[string]any{
			"nested": map[string]*uint64{"a": &a, "b": nil, "c": &c},
			"text":   "dynamic",
		},
		Custom: scratchPoisonMarshaler{Text: "clean"},
	}
	want, err := json.Marshal(&v)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := CompileEncoder[scratchPoisonDoc](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.AppendJSON(make([]byte, 0, len(want)), &v); err != nil {
		t.Fatal(err) // populate every reusable backing before poisoning it
	}

	scratch := enc.scratch.Get().(*encoderScratch)
	// Use a dedicated pool whose New returns this exact object. Even if the GC
	// discards a Pool entry, the poisoned scratch remains the next object used.
	pool := &sync.Pool{New: func() any { return scratch }}
	enc.scratch = pool

	cases := []scratchPoisonDoc{
		v,
		{
			Ints:    map[string]int{"one": 1},
			Strings: map[string]string{"one": "one"},
			Ptrs:    map[string]*uint64{"one": &a},
			Dynamic: map[string]any{"one": map[string]*uint64{"one": &a}},
			Custom:  scratchPoisonMarshaler{Text: "empty"},
		},
	}
	for round := range cases {
		dirty := 3
		if round == 1 {
			dirty = 1
		}
		poisonEncoderScratch(t, scratch, dirty)
		pool.Put(scratch)
		stackSink := forceStackMovement(48+round, 17+round)
		runtime.GC()
		want, err = json.Marshal(&cases[round])
		if err != nil {
			t.Fatal(err)
		}
		dst, encodeErr := enc.AppendJSON(make([]byte, 0, len(want)), &cases[round])
		runtime.KeepAlive(stackSink)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		if !bytes.Equal(dst, want) {
			t.Fatalf("round %d poisoned scratch encode differs:\n got %s\nwant %s", round, dst, want)
		}

		returned := pool.Get().(*encoderScratch)
		if returned != scratch {
			t.Fatal("dedicated pool returned a different scratch object")
		}
		assertEncoderScratchCleared(t, returned)
		scratch = returned
	}
}

func poisonEncoderScratch(t *testing.T, scratch *encoderScratch, dirty int) {
	t.Helper()
	for i := range scratch.marshalers {
		poisonSettableValue(scratch.marshalers[i].value, i+1)
	}
	if cap(scratch.mapEntries) > 0 {
		entries := scratch.mapEntries[:cap(scratch.mapEntries)]
		for i := range entries {
			entries[i] = mapEncodeEntry{
				name:  fmt.Sprintf("poison-%d", i),
				value: reflect.ValueOf(fmt.Sprintf("stale-%d", i)),
			}
		}
		// The used count records the dirty prefix independently of capacity.
		scratch.mapEntries = entries[:0]
		scratch.mapEntriesUsed = len(entries)
	}
	scratch.mapKeyArena = append(scratch.mapKeyArena[:0], "poison-key-arena"...)
	for i := range scratch.valueBackings {
		poisonBacking(scratch.valueBackings[i], dirty, i+17)
	}
	poisonBacking(scratch.valueBacking, dirty, 41)
	poisonBacking(scratch.dynamicValueBacking, dirty, 59)

	staleMap := map[string]int{"poison": 99}
	staleValue := reflect.ValueOf(staleMap)
	if scratch.mapIter == nil {
		scratch.mapIter = staleValue.MapRange()
	} else {
		scratch.mapIter.Reset(staleValue)
	}
	runtime.KeepAlive(staleMap)
}

func poisonBacking(backing reflect.Value, dirty, seed int) {
	if !backing.IsValid() {
		return
	}
	if dirty > backing.Len() {
		dirty = backing.Len()
	}
	for i := 0; i < dirty; i++ {
		poisonSettableValue(backing.Index(i), seed+i)
	}
}

func poisonSettableValue(value reflect.Value, seed int) {
	if !value.IsValid() || !value.CanSet() {
		return
	}
	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(int64(seed + 1))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value.SetUint(uint64(seed + 1))
	case reflect.Float32, reflect.Float64:
		value.SetFloat(float64(seed) + 0.5)
	case reflect.String:
		value.SetString(fmt.Sprintf("poison-%d", seed))
	case reflect.Pointer:
		pointer := reflect.New(value.Type().Elem())
		poisonSettableValue(pointer.Elem(), seed+1)
		value.Set(pointer)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			poisonSettableValue(value.Field(i), seed+i+1)
		}
	case reflect.Slice:
		slice := reflect.MakeSlice(value.Type(), 1, 1)
		poisonSettableValue(slice.Index(0), seed+1)
		value.Set(slice)
	}
}

func assertEncoderScratchCleared(t *testing.T, scratch *encoderScratch) {
	t.Helper()
	if scratch.mapIter == nil || !mapIterIsUnbound(scratch.mapIter) {
		t.Fatal("returned scratch iterator was not unbound")
	}
	for i := range scratch.marshalers {
		if !scratch.marshalers[i].value.IsZero() {
			t.Fatalf("marshaler/key slot %d retained a value", i)
		}
	}
	for i, entry := range scratch.mapEntries[:cap(scratch.mapEntries)] {
		if entry.name != "" || entry.value.IsValid() {
			t.Fatalf("map entry slot %d retained a name or reflect value", i)
		}
	}
	if scratch.mapEntriesUsed != 0 {
		t.Fatalf("map entry scratch retained dirty count %d", scratch.mapEntriesUsed)
	}
	for i := range scratch.valueBackings {
		assertBackingCleared(t, scratch.valueBackings[i], i)
	}
	assertBackingCleared(t, scratch.valueBacking, -1)
	assertBackingCleared(t, scratch.dynamicValueBacking, -2)
	if scratch.dynamicValueBacking.IsValid() != (scratch.dynamicValueBackingElem != nil) {
		t.Fatal("dynamic backing value/type ownership is inconsistent")
	}
	if scratch.dynamicValueBacking.IsValid() &&
		scratch.dynamicValueBacking.Type().Elem() != scratch.dynamicValueBackingElem {
		t.Fatalf("dynamic backing element type is %v, metadata says %v",
			scratch.dynamicValueBacking.Type().Elem(), scratch.dynamicValueBackingElem)
	}
}

func mapIterIsUnbound(iterator *reflect.MapIter) (unbound bool) {
	defer func() {
		unbound = recover() != nil
	}()
	iterator.Next()
	return false
}

func assertBackingCleared(t *testing.T, backing reflect.Value, slot int) {
	t.Helper()
	if !backing.IsValid() {
		return
	}
	for i := 0; i < backing.Len(); i++ {
		if !backing.Index(i).IsZero() {
			t.Fatalf("value backing slot %d element %d retained poison", slot, i)
		}
	}
}
