package vibejson

import (
	"reflect"
	"testing"
	"time"
	"unsafe"
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

type replaceContainingInner struct {
	Marker int                     `json:"marker"`
	Parent *replaceContainingOuter `json:"parent"`
}

type replaceContainingOuter struct {
	Prefix int                    `json:"prefix"`
	Child  replaceContainingInner `json:"child"`
}

type ReplaceWideScalars struct {
	F00 int `json:"f00"`
	F01 int `json:"f01"`
	F02 int `json:"f02"`
	F03 int `json:"f03"`
	F04 int `json:"f04"`
	F05 int `json:"f05"`
	F06 int `json:"f06"`
	F07 int `json:"f07"`
	F08 int `json:"f08"`
	F09 int `json:"f09"`
	F10 int `json:"f10"`
	F11 int `json:"f11"`
	F12 int `json:"f12"`
	F13 int `json:"f13"`
	F14 int `json:"f14"`
	F15 int `json:"f15"`
	F16 int `json:"f16"`
	F17 int `json:"f17"`
	F18 int `json:"f18"`
	F19 int `json:"f19"`
	F20 int `json:"f20"`
	F21 int `json:"f21"`
	F22 int `json:"f22"`
	F23 int `json:"f23"`
	F24 int `json:"f24"`
	F25 int `json:"f25"`
	F26 int `json:"f26"`
	F27 int `json:"f27"`
	F28 int `json:"f28"`
	F29 int `json:"f29"`
	F30 int `json:"f30"`
	F31 int `json:"f31"`
	F32 int `json:"f32"`
	F33 int `json:"f33"`
	F34 int `json:"f34"`
	F35 int `json:"f35"`
	F36 int `json:"f36"`
	F37 int `json:"f37"`
	F38 int `json:"f38"`
	F39 int `json:"f39"`
	F40 int `json:"f40"`
	F41 int `json:"f41"`
	F42 int `json:"f42"`
	F43 int `json:"f43"`
	F44 int `json:"f44"`
	F45 int `json:"f45"`
	F46 int `json:"f46"`
	F47 int `json:"f47"`
	F48 int `json:"f48"`
	F49 int `json:"f49"`
	F50 int `json:"f50"`
	F51 int `json:"f51"`
	F52 int `json:"f52"`
	F53 int `json:"f53"`
	F54 int `json:"f54"`
	F55 int `json:"f55"`
	F56 int `json:"f56"`
	F57 int `json:"f57"`
	F58 int `json:"f58"`
	F59 int `json:"f59"`
	F60 int `json:"f60"`
	F61 int `json:"f61"`
	F62 int `json:"f62"`
	F63 int `json:"f63"`
	F64 int `json:"f64"`
}

type ReplaceWideTail struct {
	Nested []int `json:"nested"`
}

type replaceWideDocument struct {
	ReplaceWideScalars
	*ReplaceWideTail
	Direct []int `json:"direct"`
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

func TestReplaceClearsFieldsExcludedFromJSON(t *testing.T) {
	type embeddedValue struct {
		Kept    int `json:"kept"`
		Ignored int `json:"-"`
		hidden  int
	}
	type EmbeddedPointer struct {
		Other   int `json:"other"`
		Ignored int `json:"-"`
		hidden  int
	}
	type leftConflict struct {
		Conflict int
	}
	type rightConflict struct {
		Conflict int
	}
	type document struct {
		embeddedValue
		*EmbeddedPointer
		leftConflict
		rightConflict
		Visible int `json:"visible"`
		Ignored int `json:"-"`
		hidden  int
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{
		embeddedValue:   embeddedValue{Kept: 7, Ignored: 8, hidden: 9},
		EmbeddedPointer: &EmbeddedPointer{Other: 10, Ignored: 11, hidden: 12},
		leftConflict:    leftConflict{Conflict: 13},
		rightConflict:   rightConflict{Conflict: 14},
		Visible:         15,
		Ignored:         16,
		hidden:          17,
	}
	if err := decoder.Decode([]byte(`{"kept":1,"other":2,"visible":3}`), &got); err != nil {
		t.Fatal(err)
	}
	want := document{
		embeddedValue:   embeddedValue{Kept: 1},
		EmbeddedPointer: &EmbeddedPointer{Other: 2},
		Visible:         3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Replace retained state excluded from JSON:\n got  %#v\n want %#v", got, want)
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

func TestReplaceBreaksQuotedNumberPointerAliases(t *testing.T) {
	type document struct {
		First  *int `json:"first,string"`
		Second *int `json:"second,string"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := 7
	got := document{First: &shared, Second: &shared}
	if err := decoder.Decode([]byte(`{"first":"1","second":"2"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.First == nil || got.Second == nil || *got.First != 1 || *got.Second != 2 {
		t.Fatalf("Replace quoted aliased pointers decoded as %#v", got)
	}
	if got.First == got.Second {
		t.Fatalf("Replace retained quoted pointer alias %p", got.First)
	}
}

func TestReplaceBreaksQuotedBoolPointerAliases(t *testing.T) {
	type document struct {
		First  *bool `json:"first,string"`
		Second *bool `json:"second,string"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := true
	got := document{First: &shared, Second: &shared}
	if err := decoder.Decode([]byte(`{"first":"false","second":"true"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.First == nil || got.Second == nil || *got.First || !*got.Second {
		t.Fatalf("Replace quoted aliased bool pointers decoded as %#v", got)
	}
	if got.First == got.Second {
		t.Fatalf("Replace retained quoted bool pointer alias %p", got.First)
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

	got.replaceEmbeddedValue = &replaceEmbeddedValue{Value: 7}
	if err := decoder.Decode([]byte(`{"value":1}`), &got); err == nil {
		t.Fatal("Replace reused an unexported embedded pointer that is nil in a fresh destination")
	}
}

func TestReplaceReusesPresentEmbeddedPointerStorage(t *testing.T) {
	type Embedded struct {
		First []int `json:"first"`
	}
	type document struct {
		*Embedded
		Second []int `json:"second"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	first := make([]int, 1)
	second := make([]int, 1)
	got := document{
		Embedded: &Embedded{First: first},
		Second:   second,
	}
	src := []byte(`{"first":[1],"second":[2]}`)
	decode := func() {
		if err := decoder.Decode(src, &got); err != nil {
			panic(err)
		}
		if got.Embedded == nil ||
			len(got.First) != 1 || got.First[0] != 1 ||
			len(got.Second) != 1 || got.Second[0] != 2 {
			panic("unexpected Replace result")
		}
	}
	decode()
	embeddedPointer := got.Embedded
	firstPointer := &got.First[0]
	secondPointer := &got.Second[0]
	if allocs := testing.AllocsPerRun(100, decode); allocs != 0 {
		t.Fatalf("present embedded pointer reuse: %.2f allocs/decode, want 0", allocs)
	}
	if got.Embedded != embeddedPointer ||
		&got.First[0] != firstPointer ||
		&got.Second[0] != secondPointer {
		t.Fatal("Replace discarded unique storage behind a present embedded pointer")
	}
}

func TestReplaceBreaksAliasesAcrossEmbeddedPointerFields(t *testing.T) {
	type Embedded struct {
		First []int `json:"first"`
	}
	type document struct {
		*Embedded
		Second []int `json:"second"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := []int{7}
	got := document{
		Embedded: &Embedded{First: backing},
		Second:   backing,
	}
	if err := decoder.Decode([]byte(`{"first":[1],"second":[2]}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Embedded == nil ||
		!reflect.DeepEqual(got.First, []int{1}) ||
		!reflect.DeepEqual(got.Second, []int{2}) {
		t.Fatalf("Replace retained alias across embedded pointer fields: %#v", got)
	}
	if &got.First[0] == &got.Second[0] {
		t.Fatal("Replace kept shared storage across an embedded pointer")
	}
}

func TestReplaceResetsOnlyAbsentEmbeddedPointerPrefixes(t *testing.T) {
	type Inner struct {
		Leaf int `json:"leaf"`
	}
	type Middle struct {
		*Inner
		Branch int `json:"branch"`
	}
	type document struct {
		*Middle
		Top int `json:"top"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{
		Middle: &Middle{
			Inner:  &Inner{Leaf: 7},
			Branch: 8,
		},
		Top: 9,
	}
	middle := got.Middle
	if err := decoder.Decode([]byte(`{"branch":1,"top":2}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Middle != middle || got.Inner != nil || got.Branch != 1 || got.Top != 2 {
		t.Fatalf("Replace reset the wrong embedded pointer prefix: %#v", got)
	}

	got.Inner = &Inner{Leaf: 7}
	inner := got.Inner
	if err := decoder.Decode([]byte(`{"leaf":3,"top":4}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Middle != middle || got.Inner != inner ||
		got.Leaf != 3 || got.Branch != 0 || got.Top != 4 {
		t.Fatalf("Replace failed to preserve present embedded pointer prefixes: %#v", got)
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

func BenchmarkReplacePointerReuse(b *testing.B) {
	type document struct {
		Value *int `json:"value"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{Replace: true})
	if err != nil {
		b.Fatal(err)
	}
	src := []byte(`{"value":1}`)
	value := 0
	dst := document{Value: &value}
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for b.Loop() {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
	if dst.Value == nil || *dst.Value != 1 {
		b.Fatal("unexpected Replace result")
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

func TestReplaceWideStructReusesPresentLateReferences(t *testing.T) {
	decoder := mustCompileTestDecoder[replaceWideDocument](t, DecoderOptions{Replace: true})
	tail := &ReplaceWideTail{Nested: make([]int, 2, 4)}
	got := replaceWideDocument{
		ReplaceWideScalars: ReplaceWideScalars{F00: 7, F64: 9},
		ReplaceWideTail:    tail,
		Direct:             make([]int, 2, 4),
	}
	src := []byte(`{"nested":[1,2],"direct":[3,4]}`)
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	nestedBacking := unsafe.SliceData(got.Nested)
	directBacking := unsafe.SliceData(got.Direct)
	if got.ReplaceWideTail != tail || nestedBacking == nil || directBacking == nil {
		t.Fatalf("wide Replace warmup discarded present storage: %#v", got)
	}

	decode := func() {
		if err := decoder.Decode(src, &got); err != nil {
			panic(err)
		}
	}
	if allocs := testing.AllocsPerRun(100, decode); allocs != 0 {
		t.Fatalf("wide struct Replace allocated %.2f times/decode, want 0", allocs)
	}
	if got.F00 != 0 || got.F64 != 0 ||
		!reflect.DeepEqual(got.Nested, []int{1, 2}) ||
		!reflect.DeepEqual(got.Direct, []int{3, 4}) {
		t.Fatal("wide Replace result diverged from a fresh destination")
	}
	if got.ReplaceWideTail != tail ||
		unsafe.SliceData(got.Nested) != nestedBacking ||
		unsafe.SliceData(got.Direct) != directBacking {
		t.Fatal("wide Replace discarded a unique present pointer or slice backing")
	}
}

func TestReplaceDuplicateReferenceFieldStaysAllocationFree(t *testing.T) {
	type document struct {
		First  []int `json:"first"`
		Second []int `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{
		First:  make([]int, 1, 2),
		Second: make([]int, 1, 2),
	}
	src := []byte(`{"first":[1],"first":[2],"second":[3]}`)
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("duplicate Replace reference field allocated %.1f times per decode, want 0", allocs)
	}
	if !reflect.DeepEqual(got.First, []int{2}) || !reflect.DeepEqual(got.Second, []int{3}) {
		t.Fatalf("duplicate Replace reference field decoded as %#v", got)
	}
}

func TestReplaceReusesSliceStorageReleasedByNull(t *testing.T) {
	type document struct {
		First  []int `json:"first"`
		Second []int `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := make([]int, 1, 2)
	base := unsafe.SliceData(backing)
	got := document{First: backing, Second: backing}
	if err := decoder.Decode([]byte(`{"first":null,"second":[2]}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.First != nil || !reflect.DeepEqual(got.Second, []int{2}) {
		t.Fatalf("Replace released-slice decode = %#v", got)
	}
	if unsafe.SliceData(got.Second) != base {
		t.Fatal("Replace detached storage released by an earlier null field")
	}
}

func TestReplaceReusesSliceStorageReleasedByGrowth(t *testing.T) {
	type document struct {
		First  []int `json:"first"`
		Second []int `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := make([]int, 0, 1)
	base := unsafe.SliceData(backing)
	got := document{First: backing, Second: backing}
	if err := decoder.Decode([]byte(`{"first":[1,2],"second":[3]}`), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.First, []int{1, 2}) || !reflect.DeepEqual(got.Second, []int{3}) {
		t.Fatalf("Replace grown-slice decode = %#v", got)
	}
	if unsafe.SliceData(got.Second) != base {
		t.Fatal("Replace detached storage released when an earlier slice grew")
	}
}

func TestReplaceDuplicateNullReleasesPointerStorage(t *testing.T) {
	type document struct {
		First  *int `json:"first"`
		Second *int `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := 7
	original := &shared
	got := document{First: original, Second: original}
	if err := decoder.Decode([]byte(`{"first":1,"first":null,"second":2}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.First != nil || got.Second == nil || *got.Second != 2 {
		t.Fatalf("Replace duplicate-null pointer decode = %#v", got)
	}
	if got.Second != original {
		t.Fatal("Replace detached pointee released by a duplicate null field")
	}
}

func TestReplaceDuplicateQuotedNullReleasesPointerStorage(t *testing.T) {
	type document struct {
		First  *int `json:"first,string"`
		Second *int `json:"second,string"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := 7
	original := &shared
	got := document{First: original, Second: original}
	if err := decoder.Decode([]byte(`{"first":"1","first":"null","second":"2"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.First != nil || got.Second == nil || *got.Second != 2 {
		t.Fatalf("Replace duplicate quoted-null pointer decode = %#v", got)
	}
	if got.Second != original {
		t.Fatal("Replace detached pointee released by a duplicate quoted null field")
	}
}

func TestReplaceDuplicateNestedSliceReusesRelocatedElement(t *testing.T) {
	type document struct {
		Values [][]int `json:"values"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	inner := make([]int, 1, 2)
	innerBase := unsafe.SliceData(inner)
	outer := make([][]int, 1)
	outer[0] = inner
	got := document{Values: outer}
	src := []byte(`{"values":[[1],[2]],"values":[[3],[4]]}`)
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Values, [][]int{{3}, {4}}) {
		t.Fatalf("Replace duplicate nested-slice decode = %#v", got)
	}
	if unsafe.SliceData(got.Values[0]) != innerBase {
		t.Fatal("Replace detached a nested slice after its owner slot relocated")
	}
	allocs := testing.AllocsPerRun(100, func() {
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("duplicate nested Replace field allocated %.1f times per warm decode, want 0", allocs)
	}
}

func TestReplaceDuplicateStructNullReleasesNestedStorage(t *testing.T) {
	type nested struct {
		Values []int `json:"values"`
	}
	type document struct {
		First  nested `json:"first"`
		Second []int  `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := make([]int, 1, 2)
	base := unsafe.SliceData(backing)
	got := document{First: nested{Values: backing}, Second: backing}
	src := []byte(`{"first":{"values":[1]},"first":null,"second":[2]}`)
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	if got.First.Values != nil || !reflect.DeepEqual(got.Second, []int{2}) {
		t.Fatalf("Replace duplicate struct-null decode = %#v", got)
	}
	if unsafe.SliceData(got.Second) != base {
		t.Fatal("Replace detached nested storage released by a duplicate struct null")
	}
}

func TestReplaceDuplicateStructMissingFieldReleasesNestedStorage(t *testing.T) {
	type nested struct {
		Values []int `json:"values"`
		Spare  []int `json:"spare"`
	}
	type document struct {
		Nested nested `json:"nested"`
		Reuse  []int  `json:"reuse"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	src := []byte(`{"nested":{"values":[1]},"nested":{},"reuse":[2]}`)
	storage := make([]int, 1)
	var dst document
	decode := func() {
		dst.Nested.Values = storage[:1]
		dst.Nested.Spare = nil
		dst.Reuse = storage[:1]
		if err := decoder.Decode(src, &dst); err != nil {
			panic(err)
		}
		if dst.Nested.Values != nil || dst.Nested.Spare != nil ||
			len(dst.Reuse) != 1 || dst.Reuse[0] != 2 {
			panic("unexpected Replace result")
		}
	}
	decode()
	if allocs := testing.AllocsPerRun(100, decode); allocs != 0 {
		t.Fatalf("duplicate object missing a reference field: %.2f allocs/decode, want 0", allocs)
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

func TestReplaceBreaksPointerSliceAliases(t *testing.T) {
	type document struct {
		Slice []int `json:"slice"`
		Value *int  `json:"value"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := []int{7}
	got := document{Slice: backing, Value: &backing[0]}
	if err := decoder.Decode([]byte(`{"slice":[1],"value":2}`), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Slice, []int{1}) || got.Value == nil || *got.Value != 2 {
		t.Fatalf("Replace retained pointer/slice alias: %#v", got)
	}
	if got.Value == &got.Slice[0] {
		t.Fatal("Replace kept a pointer into sibling slice storage")
	}
}

func TestReplaceBreaksPointerAliasesIntoDestination(t *testing.T) {
	type document struct {
		Scalar int  `json:"scalar"`
		Value  *int `json:"value"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	check := func(t *testing.T, got *document) {
		t.Helper()
		if got.Scalar != 1 || got.Value == nil || *got.Value != 2 {
			t.Fatalf("Replace retained pointer into destination: %#v", *got)
		}
		if got.Value == &got.Scalar {
			t.Fatal("Replace kept a pointer into sibling destination storage")
		}
	}

	t.Run("Decode", func(t *testing.T) {
		got := document{Scalar: 7}
		got.Value = &got.Scalar
		if err := decoder.Decode([]byte(`{"scalar":1,"value":2}`), &got); err != nil {
			t.Fatal(err)
		}
		check(t, &got)
	})
	t.Run("DecodePrefix", func(t *testing.T) {
		got := document{Scalar: 7}
		got.Value = &got.Scalar
		n, err := decoder.DecodePrefix([]byte(`{"scalar":1,"value":2} trailing`), &got)
		if err != nil {
			t.Fatal(err)
		}
		if n != len(`{"scalar":1,"value":2}`) {
			t.Fatalf("DecodePrefix consumed %d bytes", n)
		}
		check(t, &got)
	})
	t.Run("DecodeArray", func(t *testing.T) {
		storage := make([]document, 1)
		storage[0].Scalar = 7
		storage[0].Value = &storage[0].Scalar
		got, err := decoder.DecodeArray([]byte(`[{"scalar":1,"value":2}]`), storage[:0])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("DecodeArray length = %d", len(got))
		}
		check(t, &got[0])
	})
}

func TestReplaceBreaksSliceAliasesIntoDestination(t *testing.T) {
	type document struct {
		Fixed  [2]int `json:"fixed"`
		Values []int  `json:"values"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{Fixed: [2]int{7, 7}}
	got.Values = got.Fixed[:]
	if err := decoder.Decode([]byte(`{"fixed":[1,2],"values":[3,4]}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Fixed != [2]int{1, 2} || !reflect.DeepEqual(got.Values, []int{3, 4}) {
		t.Fatalf("Replace retained slice backing inside destination: %#v", got)
	}
	if &got.Values[0] == &got.Fixed[0] {
		t.Fatal("Replace kept a slice backed by a sibling destination array")
	}
}

func TestReplaceBreaksPointerToObjectContainingDestination(t *testing.T) {
	decoder := mustCompileTestDecoder[replaceContainingInner](t, DecoderOptions{Replace: true})
	var storage replaceContainingOuter
	storage.Child.Marker = 7
	storage.Child.Parent = &storage
	if err := decoder.Decode(
		[]byte(`{"marker":1,"parent":{"prefix":2}}`),
		&storage.Child,
	); err != nil {
		t.Fatal(err)
	}
	got := storage.Child
	if got.Marker != 1 || got.Parent == nil || got.Parent.Prefix != 2 {
		t.Fatalf("Replace retained a pointer to the object containing dst: %#v", got)
	}
	if got.Parent == &storage {
		t.Fatal("Replace kept a pointer to the object containing its destination")
	}
}

func TestReplaceBreaksPointerAliasesInsideSliceElements(t *testing.T) {
	type element struct {
		Scalar int  `json:"scalar"`
		Value  *int `json:"value"`
	}

	decoder := mustCompileTestDecoder[[]element](t, DecoderOptions{Replace: true})
	got := make([]element, 1)
	got[0].Scalar = 7
	got[0].Value = &got[0].Scalar
	if err := decoder.Decode([]byte(`[{"scalar":1,"value":2}]`), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Scalar != 1 || got[0].Value == nil || *got[0].Value != 2 {
		t.Fatalf("Replace retained a pointer into slice element storage: %#v", got)
	}
	if got[0].Value == &got[0].Scalar {
		t.Fatal("Replace kept a pointer into its reused slice element")
	}
}

func TestReplaceBreaksPointerAliasesInsideReusedPointee(t *testing.T) {
	type nested struct {
		Scalar int  `json:"scalar"`
		Value  *int `json:"value"`
	}
	type document struct {
		Nested *nested `json:"nested"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	storage := &nested{Scalar: 7}
	storage.Value = &storage.Scalar
	got := document{Nested: storage}
	if err := decoder.Decode([]byte(`{"nested":{"scalar":1,"value":2}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Nested == nil || got.Nested.Scalar != 1 ||
		got.Nested.Value == nil || *got.Nested.Value != 2 {
		t.Fatalf("Replace retained a pointer into reused pointee storage: %#v", got)
	}
	if got.Nested != storage {
		t.Fatalf("Replace discarded unique outer pointee: got %p, want %p", got.Nested, storage)
	}
	if got.Nested.Value == &got.Nested.Scalar {
		t.Fatal("Replace kept a pointer into its reused parent pointee")
	}
}

func TestReplaceBreaksQuotedPointerAliasesIntoDestination(t *testing.T) {
	type document struct {
		Scalar int  `json:"scalar"`
		Value  *int `json:"value,string"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{Scalar: 7}
	got.Value = &got.Scalar
	if err := decoder.Decode([]byte(`{"scalar":1,"value":"2"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Scalar != 1 || got.Value == nil || *got.Value != 2 {
		t.Fatalf("Replace retained a quoted pointer into destination: %#v", got)
	}
	if got.Value == &got.Scalar {
		t.Fatal("Replace kept a quoted pointer into sibling destination storage")
	}
}

func TestReplaceBreaksQuotedBoolPointerAliasesIntoDestination(t *testing.T) {
	type document struct {
		Scalar bool  `json:"scalar"`
		Value  *bool `json:"value,string"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	got := document{Scalar: false}
	got.Value = &got.Scalar
	if err := decoder.Decode([]byte(`{"scalar":true,"value":"false"}`), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Scalar || got.Value == nil || *got.Value {
		t.Fatalf("Replace retained a quoted bool pointer into destination: %#v", got)
	}
	if got.Value == &got.Scalar {
		t.Fatal("Replace kept a quoted bool pointer into sibling destination storage")
	}
}

func TestReplaceBreaksQuotedPointerAliasesAcrossFields(t *testing.T) {
	type document struct {
		First  *int `json:"first,string"`
		Second *int `json:"second,string"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := 7
	got := document{First: &shared, Second: &shared}
	if err := decoder.Decode([]byte(`{"first":"1","second":"2"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.First == nil || *got.First != 1 || got.Second == nil || *got.Second != 2 {
		t.Fatalf("Replace retained quoted pointer alias across fields: %#v", got)
	}
	if got.First == got.Second {
		t.Fatal("Replace kept shared quoted-pointer storage")
	}
}

func TestReplaceBreaksOverlappingPointerRanges(t *testing.T) {
	type document struct {
		Whole *[2]int `json:"whole"`
		Tail  *int    `json:"tail"`
	}

	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := [2]int{7, 7}
	got := document{Whole: &backing, Tail: &backing[1]}
	if err := decoder.Decode([]byte(`{"whole":[1,2],"tail":3}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Whole == nil || *got.Whole != [2]int{1, 2} ||
		got.Tail == nil || *got.Tail != 3 {
		t.Fatalf("Replace retained overlapping pointer ranges: %#v", got)
	}
	if got.Tail == &got.Whole[1] {
		t.Fatal("Replace kept an interior pointer into sibling pointee storage")
	}
}

func TestReplaceBreaksEmptySliceCapacityAliases(t *testing.T) {
	type document struct {
		First  []int `json:"first"`
		Second []int `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	backing := make([]int, 0, 2)
	got := document{First: backing, Second: backing}
	if err := decoder.Decode([]byte(`{"first":[1],"second":[2]}`), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.First, []int{1}) || !reflect.DeepEqual(got.Second, []int{2}) {
		t.Fatalf("Replace retained empty-slice capacity alias: %#v", got)
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
