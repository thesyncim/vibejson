package simdjson

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

type marshalHintRetentionValue struct {
	Payload string `json:"payload"`
}

func TestMarshalHintRecoversAfterOversizedValue(t *testing.T) {
	typ := reflect.TypeFor[marshalHintRetentionValue]()
	marshalEncoders.Delete(typ)
	t.Cleanup(func() { marshalEncoders.Delete(typ) })

	huge := marshalHintRetentionValue{Payload: strings.Repeat("x", int(marshalSizeHintMax)*4)}
	if _, err := Marshal(&huge); err != nil {
		t.Fatal(err)
	}
	entry, ok := marshalEncoders.Load(typ)
	if !ok {
		t.Fatal("Marshal did not cache its encoder")
	}
	hint := entry.(*cachedEncoder[marshalHintRetentionValue]).sizeHint.Load()
	wantCandidate := marshalSizeHintUnconfirmed | uint64(len(huge.Payload)+14)
	if hint != wantCandidate {
		t.Fatalf("oversized observation stored state %#x, want %#x", hint, wantCandidate)
	}

	tiny := marshalHintRetentionValue{}
	firstTiny, err := Marshal(&tiny)
	if err != nil {
		t.Fatal(err)
	}
	wantOutlierBudget := marshalSizeHintMin * marshalSizeHintGrowth
	if cap(firstTiny) > int(wantOutlierBudget) {
		t.Fatalf("first tiny result retained oversized capacity %d, budget %d", cap(firstTiny), wantOutlierBudget)
	}
	secondTiny, err := Marshal(&tiny)
	if err != nil {
		t.Fatal(err)
	}
	if cap(secondTiny) > int(marshalSizeHintMin) {
		t.Fatalf("tiny result did not recover to minimum hint: cap=%d, want <= %d", cap(secondTiny), marshalSizeHintMin)
	}
}

func TestMarshalHintConcurrentGrowth(t *testing.T) {
	typ := reflect.TypeFor[marshalHintRetentionValue]()
	marshalEncoders.Delete(typ)
	t.Cleanup(func() { marshalEncoders.Delete(typ) })
	huge := marshalHintRetentionValue{Payload: strings.Repeat("x", int(marshalSizeHintMax)*4)}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Marshal(&huge); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	entry, ok := marshalEncoders.Load(typ)
	if !ok {
		t.Fatal("Marshal did not cache its encoder")
	}
	want := uint64(len(huge.Payload) + 14)
	if got := entry.(*cachedEncoder[marshalHintRetentionValue]).sizeHint.Load(); got != want {
		t.Fatalf("concurrent stable growth stored %d, want %d", got, want)
	}
}

func TestMarshalHintConfirmsStableLargeValue(t *testing.T) {
	typ := reflect.TypeFor[marshalHintRetentionValue]()
	marshalEncoders.Delete(typ)
	t.Cleanup(func() { marshalEncoders.Delete(typ) })

	large := marshalHintRetentionValue{Payload: strings.Repeat("x", int(marshalSizeHintMax)*4)}
	first, err := Marshal(&large)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Marshal(&large)
	if err != nil {
		t.Fatal(err)
	}
	if cap(second) == len(second) {
		t.Fatal("second observation unexpectedly used an already-confirmed hint")
	}
	third, err := Marshal(&large)
	if err != nil {
		t.Fatal(err)
	}
	if cap(third) != len(first) {
		t.Fatalf("stable large result capacity = %d, want exact %d", cap(third), len(first))
	}

	tiny, err := Marshal(&marshalHintRetentionValue{})
	if err != nil {
		t.Fatal(err)
	}
	if cap(tiny) != len(first) {
		t.Fatalf("first size change capacity = %d, want previous stable %d", cap(tiny), len(first))
	}
	recovered, err := Marshal(&marshalHintRetentionValue{})
	if err != nil {
		t.Fatal(err)
	}
	if cap(recovered) > int(marshalSizeHintMin) {
		t.Fatalf("size change did not recover: capacity = %d", cap(recovered))
	}
}
