package vibejson

import (
	"reflect"
	"testing"
	"time"
)

type replaceJSONState struct {
	hidden int
	Seen   int
}

func (s *replaceJSONState) UnmarshalJSON(data []byte) error {
	s.Seen = s.hidden
	return nil
}

type replaceTextState struct {
	hidden int
	Seen   int
}

func (s *replaceTextState) UnmarshalText(text []byte) error {
	s.Seen = s.hidden
	return nil
}

type replaceHookState struct {
	hidden int
	Seen   int
}

func (s *replaceHookState) UnmarshalVibeJSON(cursor DecodeCursor) (DecodeCursor, error) {
	s.Seen = s.hidden
	return cursor, cursor.Skip()
}

func TestReplaceCustomDecodersReceiveZeroState(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		decoder := mustCompileTestDecoder[replaceJSONState](t, DecoderOptions{Replace: true})
		got := replaceJSONState{hidden: 7, Seen: 7}
		if err := decoder.Decode([]byte(`{}`), &got); err != nil {
			t.Fatal(err)
		}
		if got.hidden != 0 || got.Seen != 0 {
			t.Fatalf("Replace JSON receiver = %#v, want zero-state receiver", got)
		}
	})

	t.Run("text", func(t *testing.T) {
		decoder := mustCompileTestDecoder[replaceTextState](t, DecoderOptions{Replace: true})
		got := replaceTextState{hidden: 7, Seen: 7}
		if err := decoder.Decode([]byte(`"value"`), &got); err != nil {
			t.Fatal(err)
		}
		if got.hidden != 0 || got.Seen != 0 {
			t.Fatalf("Replace text receiver = %#v, want zero-state receiver", got)
		}
	})

	t.Run("native hook", func(t *testing.T) {
		decoder := mustCompileTestDecoder[replaceHookState](t, DecoderOptions{Replace: true})
		got := replaceHookState{hidden: 7, Seen: 7}
		if err := decoder.Decode([]byte(`0`), &got); err != nil {
			t.Fatal(err)
		}
		if got.hidden != 0 || got.Seen != 0 {
			t.Fatalf("Replace native-hook receiver = %#v, want zero-state receiver", got)
		}
	})
}

func TestReplaceClearsAbsentCustomDecoderField(t *testing.T) {
	type document struct {
		Value replaceJSONState `json:"value"`
		Other int              `json:"other"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{
		Value: replaceJSONState{hidden: 7, Seen: 7},
	}
	if err := decoder.Decode([]byte(`{"other":1}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Value.hidden != 0 || got.Value.Seen != 0 {
		t.Fatalf("absent custom field = %#v, want zero value", got.Value)
	}
}

func TestReplaceNullClearsTimeValue(t *testing.T) {
	decoder := mustCompileTestDecoder[time.Time](t, DecoderOptions{Replace: true})
	got := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if err := decoder.Decode([]byte(`null`), &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("Replace null retained time %v, want zero", got)
	}
}

func TestReplaceQuotedNullClearsScalar(t *testing.T) {
	type document struct {
		Number  int  `json:"number,string"`
		Pointer *int `json:"pointer,string"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	seven := 7
	got := document{Number: 7, Pointer: &seven}
	if err := decoder.Decode([]byte(`{"number":"null","pointer":"null"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Number != 0 || got.Pointer != nil {
		t.Fatalf("quoted null retained Number=%d Pointer=%v, want zero values", got.Number, got.Pointer)
	}
}

func TestReplaceDynamicInterfaceIgnoresExistingPointer(t *testing.T) {
	type pointee struct {
		Value int `json:"value"`
	}
	type document struct {
		Dynamic any `json:"dynamic"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{Dynamic: &pointee{Value: 7}}
	if err := decoder.Decode([]byte(`{"dynamic":{"value":1}}`), &got); err != nil {
		t.Fatal(err)
	}
	want := document{Dynamic: map[string]any{"value": float64(1)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Replace dynamic interface:\n got  %#v\n want %#v", got, want)
	}

	root := mustCompileTestDecoder[any](t, DecoderOptions{Replace: true})
	var rootGot any = &pointee{Value: 7}
	if err := root.Decode([]byte(`{"value":1}`), &rootGot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rootGot, want.Dynamic) {
		t.Fatalf("Replace root interface = %#v, want %#v", rootGot, want.Dynamic)
	}
}

type replaceNonEmpty interface {
	replaceMarker()
}

type replaceNonEmptyValue struct {
	Value int `json:"value"`
}

func (*replaceNonEmptyValue) replaceMarker() {}

func TestReplaceNonEmptyInterfaceDoesNotMergeExistingPointer(t *testing.T) {
	type document struct {
		Value replaceNonEmpty `json:"value"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{Value: &replaceNonEmptyValue{Value: 7}}
	if err := decoder.Decode([]byte(`{"value":{"value":1}}`), &got); err == nil {
		t.Fatalf("Replace decoded into stale non-empty interface: %#v", got)
	}
}

type replaceEmbeddedValue struct {
	Value int `json:"value"`
}

type replaceEmbeddedDocument struct {
	*replaceEmbeddedValue
	Other int `json:"other"`
}

func TestReplaceClearsAbsentEmbeddedPointer(t *testing.T) {
	decoder := mustCompileTestDecoder[replaceEmbeddedDocument](t, DecoderOptions{Replace: true})
	got := replaceEmbeddedDocument{
		replaceEmbeddedValue: &replaceEmbeddedValue{Value: 7},
	}
	if err := decoder.Decode([]byte(`{"other":1}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.replaceEmbeddedValue != nil {
		t.Fatalf("absent embedded pointer = %#v, want nil", got.replaceEmbeddedValue)
	}
}

func TestReplaceBreaksStalePointerAliases(t *testing.T) {
	type document struct {
		First  *int `json:"first"`
		Second *int `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := 7
	original := &shared
	got := document{First: original, Second: original}
	if err := decoder.Decode([]byte(`{"first":1,"second":2}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.First == nil || got.Second == nil || *got.First != 1 || *got.Second != 2 {
		t.Fatalf("Replace aliased pointers decoded as %#v", got)
	}
	if got.First == got.Second {
		t.Fatalf("Replace retained stale pointer alias %p", got.First)
	}
	if got.First != original {
		t.Fatalf("Replace discarded unique first pointee: got %p, want reuse of %p", got.First, original)
	}
}

func TestReplacePointerReuseStaysAllocationFree(t *testing.T) {
	type document struct {
		First  *int `json:"first"`
		Second *int `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	first, second := 1, 2
	got := document{First: &first, Second: &second}
	src := []byte(`{"first":3,"second":4}`)
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	firstPointer, secondPointer := got.First, got.Second
	allocs := testing.AllocsPerRun(100, func() {
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("Replace pointer reuse allocated %.1f times per decode, want 0", allocs)
	}
	if got.First != firstPointer || got.Second != secondPointer {
		t.Fatal("Replace discarded unique pointees during allocation-free reuse")
	}
}

func TestReplaceWideReferenceTrackingStaysAllocationFree(t *testing.T) {
	decoder := mustCompileTestDecoder[[][]int](t, DecoderOptions{Replace: true})
	got := make([][]int, 20)
	for i := range got {
		got[i] = []int{i}
	}
	src := []byte(`[[0],[1],[2],[3],[4],[5],[6],[7],[8],[9],[10],[11],[12],[13],[14],[15],[16],[17],[18],[19]]`)
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("wide Replace reference tracking allocated %.1f times per decode, want 0", allocs)
	}
}

func TestReplaceBreaksStaleSliceAndMapAliases(t *testing.T) {
	type document struct {
		FirstSlice  []int          `json:"firstSlice"`
		SecondSlice []int          `json:"secondSlice"`
		FirstMap    map[string]int `json:"firstMap"`
		SecondMap   map[string]int `json:"secondMap"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := []int{7, 7, 7}
	sharedMap := map[string]int{"stale": 7}
	got := document{
		FirstSlice:  backing[:2],
		SecondSlice: backing[1:],
		FirstMap:    sharedMap,
		SecondMap:   sharedMap,
	}
	if err := decoder.Decode([]byte(
		`{"firstSlice":[1,2],"secondSlice":[3,4],"firstMap":{"a":1},"secondMap":{"b":2}}`,
	), &got); err != nil {
		t.Fatal(err)
	}
	want := document{
		FirstSlice:  []int{1, 2},
		SecondSlice: []int{3, 4},
		FirstMap:    map[string]int{"a": 1},
		SecondMap:   map[string]int{"b": 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Replace retained stale slice/map aliases:\n got  %#v\n want %#v", got, want)
	}
}

func TestReplaceDecodeArrayClearsAliasedElements(t *testing.T) {
	type element struct {
		Values map[string]int `json:"values"`
	}
	decoder := mustCompileTestDecoder[element](t, DecoderOptions{Replace: true})
	shared := map[string]int{"stale": 7}
	got := []element{{Values: shared}, {Values: shared}}
	var err error
	got, err = decoder.DecodeArray(
		[]byte(`[{"values":{"a":1}},{"values":{"b":2}}]`),
		got[:0],
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []element{
		{Values: map[string]int{"a": 1}},
		{Values: map[string]int{"b": 2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Replace DecodeArray retained element aliases:\n got  %#v\n want %#v", got, want)
	}
}
