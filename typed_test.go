package simdjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

type typedTestRecord struct {
	ID     int         `json:"id"`
	OK     bool        `json:"ok"`
	Name   string      `json:"name"`
	Scores [3]float64  `json:"scores"`
	Number json.Number `json:"number"`
}

func TestTypedDecoderCursorStaysCompact(t *testing.T) {
	// 64 bytes is one cache line on the target architectures. The decode hot
	// loop threads a decoderCursor by value through every recursive call, so
	// keeping it within a single line avoids a straddling load per level of
	// nesting. A field added past this cap must be justified against that cost,
	// which is why the bound is asserted rather than left to drift.
	if size := unsafe.Sizeof(decoderCursor{}); size > 64 {
		t.Fatalf("typed decoder cursor size = %d bytes, want <= 64", size)
	}
	// typedNode is the immutable program walked by every compiled decode and
	// encode. Six cache lines preserves its established density as uncommon
	// operation scratch is added beside the hot program.
	if size := unsafe.Sizeof(typedNode{}); size > 384 {
		t.Fatalf("typed plan node size = %d bytes, want <= 384", size)
	}
	// typedEncField is stored one-per-field in the compiled encoder program and
	// walked linearly while encoding; 40 bytes keeps the field table dense so
	// the walk stays cache-friendly on wide structs.
	if size := unsafe.Sizeof(typedEncField{}); size > 40 {
		t.Fatalf("typed encoder field size = %d bytes, want <= 40", size)
	}
}

func TestTypedCompilerSeparatesDirectionPrograms(t *testing.T) {
	typ := reflect.TypeFor[typedEdgeValue]()
	compiler := newTypedCompiler(typedCompileDecode)
	root, err := compiler.compile(typ, typ.String())
	if err != nil {
		t.Fatal(err)
	}
	if compiler.encHasMap || compiler.encBackingSlots != 0 || len(compiler.encScratchTypes) != 0 {
		t.Fatalf("decode compilation reserved encoder scratch: maps=%v backings=%d types=%d",
			compiler.encHasMap, compiler.encBackingSlots, len(compiler.encScratchTypes))
	}

	seen := make(map[*typedNode]bool)
	var visit func(*typedNode)
	visit = func(node *typedNode) {
		if node == nil || seen[node] {
			return
		}
		seen[node] = true
		if node.encFields != nil || node.encNameData != nil || node.encClose != nil || node.encPaths != nil {
			t.Fatalf("decode node %s retained an encoder field program", node.name)
		}
		if node.encKind != typedInvalid || node.encNonAddrKind != typedInvalid || node.encOp != typedOpInvalid {
			t.Fatalf("decode node %s retained encoder dispatch", node.name)
		}
		if node.encScratch != -1 || node.encMapKey != -1 || node.encBacking != noEncoderBackingSlot || node.encScratchLimit != 0 {
			t.Fatalf("decode node %s retained encoder scratch metadata", node.name)
		}
		visit(node.elem)
		for i := range node.fields {
			visit(node.fields[i].node)
		}
		if node.inlineMap != nil {
			visit(node.inlineMap.elem)
		}
	}
	visit(root)

	encoderCompiler := newTypedCompiler(typedCompileEncode)
	encoderRoot, err := encoderCompiler.compile(typ, typ.String())
	if err != nil {
		t.Fatal(err)
	}
	seen = make(map[*typedNode]bool)
	visit = func(node *typedNode) {
		if node == nil || seen[node] {
			return
		}
		seen[node] = true
		if node.fields != nil || node.fieldTable != nil || node.hopResets != nil || node.reset != nil {
			t.Fatalf("encode node %s retained a decoder field or reset program", node.name)
		}
		if node.kind != typedInvalid || node.op != typedOpInvalid {
			t.Fatalf("encode node %s retained decoder dispatch", node.name)
		}
		if node.decShape != typedDecShapeNone || node.structuralFast || node.decBuiltinSlice ||
			node.emptySliceData != nil || node.decHasReceiver || node.decMapScratch != 0 || node.allSet != 0 {
			t.Fatalf("encode node %s retained decoder execution metadata", node.name)
		}
		visit(node.elem)
		for i := range node.encFields {
			visit(node.encFields[i].node)
		}
		if node.inlineMap != nil {
			visit(node.inlineMap.elem)
		}
	}
	visit(encoderRoot)
}

type typedTestDocument struct {
	Items []typedTestRecord `json:"items"`
	Count uint16            `json:"count"`
	Next  *typedTestRecord  `json:"next"`
}

type typedEdgePointer *typedEdgeValue

type typedEdgeInt int

type typedEdgeEmbedded struct {
	Promoted int    `json:"promoted"`
	Shadowed string `json:"shadowed"`
}

type typedEdgeValue struct {
	typedEdgeEmbedded
	Shadowed string           `json:"shadowed"` // outer wins over the embedded field
	ID       int              `json:"id"`
	Long     string           `json:"long_field_name"`
	Escaped  string           `json:"escaped"`
	Values   []int            `json:"values"`
	Fixed    [3]typedEdgeInt  `json:"fixed"`
	Next     typedEdgePointer `json:"next"`
	Extra    map[string]int   `json:"extra"`
	Meta     any              `json:"meta"`
	When     *time.Time       `json:"when"`
}

func TestTypedDecoderMatchesStdlib(t *testing.T) {
	src := []byte(`{"items":[{"id":1,"ok":true,"name":"one","scores":[1,2.5,-3e4],"number":1234567890123456},{"id":2,"ok":false,"name":"two","scores":[4,5,6],"number":2}],"count":2,"next":{"id":3,"ok":true,"name":"three","scores":[7,8,9],"number":3},"unknown":{"nested":[1,2,3]}}`)
	var want typedTestDocument
	stdDecoder := json.NewDecoder(bytes.NewReader(src))
	stdDecoder.UseNumber()
	if err := stdDecoder.Decode(&want); err != nil {
		t.Fatal(err)
	}

	decoder, err := CompileDecoder[typedTestDocument](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	var got typedTestDocument
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed decoder = %#v, want %#v", got, want)
	}
}

func TestTypedDecoderReuseAndAllocations(t *testing.T) {
	src := []byte(`{"items":[{"id":1,"ok":true,"name":"one","scores":[1,2.5,-3e4],"number":1},{"id":2,"ok":false,"name":"two","scores":[4,5,6],"number":2}],"count":2,"next":null}`)
	decoder, err := CompileDecoder[typedTestDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	dst := typedTestDocument{Items: make([]typedTestRecord, 0, 4)}
	if err := decoder.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	base := &dst.Items[0]
	allocs := testing.AllocsPerRun(1000, func() {
		if err := decoder.Decode(src, &dst); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("typed decoder reuse allocs = %v, want 0", allocs)
	}
	if &dst.Items[0] != base {
		t.Fatal("typed decoder did not reuse destination slice")
	}
}

func TestTypedDecoderOptionsAndUnsupportedTypes(t *testing.T) {
	strict, err := CompileDecoder[typedTestRecord](DecoderOptions{DisallowUnknownFields: true})
	if err != nil {
		t.Fatal(err)
	}
	var record typedTestRecord
	if err := strict.Decode([]byte(`{"unknown":1}`), &record); err == nil {
		t.Fatal("typed decoder accepted unknown field")
	}
	if _, err := CompileDecoder[struct{ Value func() }](DecoderOptions{}); err == nil {
		t.Fatal("typed decoder accepted func field")
	}
}

func TestTypedDecoderReplacementAndFieldFallbacks(t *testing.T) {
	decoder, err := CompileDecoder[typedEdgeValue](DecoderOptions{ZeroCopy: true, Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	dst := typedEdgeValue{
		ID:      99,
		Long:    "stale",
		Escaped: "stale",
		Values:  make([]int, 3, 8),
		Fixed:   [3]typedEdgeInt{9, 9, 9},
		Next:    typedEdgePointer(&typedEdgeValue{ID: 99}),
	}
	src := []byte(`{"long_field_name":"first","ID":7,"\u0065scaped":"escaped","fixed":[1,2],"id":8}`)
	if err := decoder.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.ID != 8 || dst.Long != "first" || dst.Escaped != "escaped" {
		t.Fatalf("field fallback result = %#v", dst)
	}
	if dst.Fixed != [3]typedEdgeInt{1, 2, 0} {
		t.Fatalf("fixed array = %v, want [1 2 0]", dst.Fixed)
	}
	if dst.Values != nil {
		// Replace decodes as if into a fresh destination, so an absent slice
		// field becomes its zero value (nil), not a retained empty slice.
		t.Fatalf("missing slice = %#v, want nil", dst.Values)
	}
	if dst.Next != nil {
		t.Fatalf("missing pointer = %#v, want nil", dst.Next)
	}

	if err := decoder.Decode([]byte(`{"values":null}`), &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Values != nil {
		t.Fatalf("null slice = %#v, want nil", dst.Values)
	}
	if err := decoder.Decode([]byte(`{"values":[]}`), &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Values == nil || len(dst.Values) != 0 {
		t.Fatalf("empty slice = %#v, want non-nil empty", dst.Values)
	}
}

func TestTypedDecoderNestedErrorType(t *testing.T) {
	decoder, err := CompileDecoder[typedEdgeValue](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var dst typedEdgeValue
	err = decoder.Decode([]byte(`{"fixed":[1,1.5]}`), &dst)
	var typedErr *DecodeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %v, want DecodeError", err)
	}
	if typedErr.Type != reflect.TypeFor[typedEdgeInt]() {
		t.Fatalf("error type = %v, want %v", typedErr.Type, reflect.TypeFor[typedEdgeInt]())
	}
}

func TestTypedDecoderSliceGrowthAndNamedPointer(t *testing.T) {
	decoder, err := CompileDecoder[typedEdgeValue](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	dst := typedEdgeValue{Values: []int{99}}
	src := []byte(`{"values":[0,1,2,3,4,5,6,7,8,9],"next":{"id":42}}`)
	if err := decoder.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dst.Values, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("grown slice = %v", dst.Values)
	}
	if dst.Next == nil || dst.Next.ID != 42 {
		t.Fatalf("named pointer = %#v", dst.Next)
	}
	if err := decoder.Decode([]byte(`{"next":null}`), &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Next != nil {
		t.Fatalf("null named pointer = %#v, want nil", dst.Next)
	}
}

func TestTypedDecoderEmptySliceSentinelIsNonNilAndIsolated(t *testing.T) {
	type document struct {
		Values []int `json:"values"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}

	var first, second document
	for name, dst := range map[string]*document{"first": &first, "second": &second} {
		if err := decoder.Decode([]byte(`{"values":[]}`), dst); err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		if dst.Values == nil || len(dst.Values) != 0 || cap(dst.Values) != 0 {
			t.Fatalf("%s empty slice = %#v (len=%d cap=%d), want non-nil len=cap=0", name, dst.Values, len(dst.Values), cap(dst.Values))
		}
	}

	first.Values = append(first.Values, 1)
	if len(second.Values) != 0 || cap(second.Values) != 0 {
		t.Fatalf("append to first contaminated second: %#v", second.Values)
	}
	if err := decoder.Decode([]byte(`{"values":[]}`), &first); err != nil {
		t.Fatal(err)
	}
	if first.Values == nil || len(first.Values) != 0 || cap(first.Values) != 0 {
		t.Fatalf("reused empty slice = %#v (len=%d cap=%d), want non-nil len=cap=0", first.Values, len(first.Values), cap(first.Values))
	}
}

func TestTypedDecoderDecodeArray(t *testing.T) {
	decoder, err := CompileDecoder[typedEdgeValue](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]typedEdgeValue, 0, 4)
	base := unsafe.SliceData(dst[:cap(dst)])
	dst, err = decoder.DecodeArray([]byte(`[{"id":1},{"id":2}]`), dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(dst) != 2 || dst[0].ID != 1 || dst[1].ID != 2 {
		t.Fatalf("decoded array = %#v", dst)
	}
	if unsafe.SliceData(dst) != base {
		t.Fatal("DecodeArray did not reuse destination capacity")
	}
	dst, err = decoder.DecodeArray([]byte(`null`), dst)
	if err != nil {
		t.Fatal(err)
	}
	if dst != nil {
		t.Fatalf("null top-level array = %#v, want nil", dst)
	}
	dst, err = decoder.DecodeArray([]byte(`[]`), dst)
	if err != nil {
		t.Fatal(err)
	}
	if dst == nil || len(dst) != 0 {
		t.Fatalf("empty top-level array = %#v, want non-nil empty", dst)
	}
}

func TestTypedDecoderSmallDecodeAllocations(t *testing.T) {
	decoder, err := CompileDecoder[struct {
		ID   int    `json:"id"`
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"id":1,"ok":true,"name":"x"}`)
	var dst struct {
		ID   int    `json:"id"`
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := decoder.Decode(src, &dst); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("small typed Decode allocs = %v, want 0", allocs)
	}
}

// checkTypedEdgeValueMatchesStdlib preserves the former typed-edge fuzz
// target's focused oracle inside FuzzDecodeTrust. It deliberately keeps that
// target's Valid-only domain and 16 KiB budget independent of the trust sink.
func checkTypedEdgeValueMatchesStdlib(t *testing.T, decoder Decoder[typedEdgeValue], src []byte) {
	t.Helper()
	if len(src) > 1<<14 || !Valid(src) {
		return
	}
	var got, want typedEdgeValue
	gotErr := decoder.Decode(src, &got)
	wantErr := json.Unmarshal(src, &want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("acceptance differs: simdjson=%v stdlib=%v", gotErr, wantErr)
	}
	if gotErr == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded value differs: simdjson=%#v stdlib=%#v", got, want)
	}
}

func TestDecodeErrorReportsPath(t *testing.T) {
	decoder, err := CompileDecoder[typedTestDocument](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		src  string
		path string
	}{
		{
			name: "nested array element",
			src:  `{"items":[{"id":1,"ok":true,"name":"a","scores":[1,2,3],"number":1},{"id":2,"ok":true,"name":"b","scores":[1,"x",3],"number":2}],"count":1}`,
			path: "items[1].scores[1]",
		},
		{
			name: "scalar field",
			src:  `{"items":[{"id":"nope"}],"count":1}`,
			path: "items[0].id",
		},
		{
			name: "container mismatch",
			src:  `{"items":[7],"count":1}`,
			path: "items[0]",
		},
		{
			name: "pointer target field",
			src:  `{"count":1,"next":{"id":1,"ok":"broken"}}`,
			path: "next.ok",
		},
		{
			name: "top level",
			src:  `[1]`,
			path: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dst typedTestDocument
			err := decoder.Decode([]byte(tc.src), &dst)
			var decodeErr *DecodeError
			if !errors.As(err, &decodeErr) {
				t.Fatalf("Decode(%q) error = %v, want *DecodeError", tc.src, err)
			}
			if decodeErr.Path != tc.path {
				t.Fatalf("Decode(%q) path = %q, want %q", tc.src, decodeErr.Path, tc.path)
			}
		})
	}
}

func TestUnmarshalMatchesCompiledDecoder(t *testing.T) {
	src := []byte(`{"items":[{"ID":1,"ok":true,"name":"one","scores":[1,2.5,-3e4],"number":9}],"count":7}`)
	var want typedTestDocument
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var got typedTestDocument
		if err := Unmarshal(src, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Unmarshal = %#v, want %#v", got, want)
		}
	}

	var invalid func()
	err := Unmarshal([]byte(`1`), &invalid)
	var unsupported *UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Unmarshal into func = %v, want *UnsupportedTypeError", err)
	}

	// Owned strings must survive input mutation.
	input := []byte(`{"items":[{"id":1,"ok":true,"name":"keepsake","scores":[1,2,3],"number":1}],"count":1}`)
	var owned typedTestDocument
	if err := Unmarshal(input, &owned); err != nil {
		t.Fatal(err)
	}
	for i := range input {
		input[i] = 'x'
	}
	if owned.Items[0].Name != "keepsake" {
		t.Fatalf("Unmarshal string aliases input: %q", owned.Items[0].Name)
	}
}

// TestFieldOrderPermutationsMatchStdlib exercises adaptive expected-field
// matching: every member order, with and without unknown members, duplicate
// keys, and case-folded keys, must decode exactly like encoding/json.
func TestFieldOrderPermutationsMatchStdlib(t *testing.T) {
	members := []string{
		`"id":42`,
		`"ok":true`,
		`"name":"perm"`,
		`"scores":[1,2.5,3]`,
		`"number":7`,
	}
	extras := [][]string{
		nil,
		{`"unknown":{"nested":[1,2]}`},
		{`"id":43`},         // duplicate key, last wins
		{`"NAME":"folded"`}, // case-insensitive fallback, last wins
	}
	decoder, err := CompileDecoder[typedTestRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var permute func(rest, current []string)
	permute = func(rest, current []string) {
		if len(rest) == 0 {
			for _, extra := range extras {
				for insert := 0; insert <= len(current); insert += 2 {
					ordered := make([]string, 0, len(current)+len(extra))
					ordered = append(ordered, current[:insert]...)
					ordered = append(ordered, extra...)
					ordered = append(ordered, current[insert:]...)
					src := []byte("{" + strings.Join(ordered, ",") + "}")

					var got, want typedTestRecord
					gotErr := decoder.Decode(src, &got)
					wantErr := json.Unmarshal(src, &want)
					if (gotErr == nil) != (wantErr == nil) {
						t.Fatalf("%s: acceptance differs: simdjson=%v stdlib=%v", src, gotErr, wantErr)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: simdjson=%#v stdlib=%#v", src, got, want)
					}
				}
			}
			return
		}
		for i := range rest {
			next := append(append([]string{}, rest[:i]...), rest[i+1:]...)
			permute(next, append(current, rest[i]))
		}
	}
	permute(members, nil)
}

func TestCompiledFieldTableCollisionsMatchStdlib(t *testing.T) {
	type collisionRecord struct {
		Field0 int `json:"field0"`
		Field1 int `json:"field1"`
		Field2 int `json:"field2"`
		Field3 int `json:"field3"`
		Field4 int `json:"field4"`
		Field5 int `json:"field5"`
		Field6 int `json:"field6"`
		Field7 int `json:"field7"`
		Field8 int `json:"field8"`
		Field9 int `json:"field9"`
	}
	// These names deliberately share the same initial table slot.
	src := []byte(`{"field9":9,"field0":0,"unknown":[1,2],"field8":8,"field1":1,"field7":7,"field2":2,"field6":6,"field3":3,"FIELD4":4,"field5":5}`)
	decoder, err := CompileDecoder[collisionRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got, want collisionRecord
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("collision decode = %#v, want %#v", got, want)
	}
}

func TestCaseFoldedKeysMatchStdlib(t *testing.T) {
	type untagged struct {
		ID     int
		Name   string
		OK     bool
		Id     string // folds with ID: exact matches must win for both
		Corner int    `json:"x_y"`
	}
	decoder, err := CompileDecoder[untagged](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sources := []string{
		`{"id":1,"name":"a","ok":true}`,
		`{"ID":2,"NAME":"b","OK":false}`,
		`{"Id":"exact","ID":3}`,
		`{"iD":4,"Name":"c"}`,
		`{"x_y":5,"X_y":6,"x_Y":7}`,
		`{"id":8,"Id":"both"}`,
	}
	for _, src := range sources {
		var got, want untagged
		gotErr := decoder.Decode([]byte(src), &got)
		wantErr := json.Unmarshal([]byte(src), &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("%s: acceptance differs: simdjson=%v stdlib=%v", src, gotErr, wantErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: simdjson=%#v stdlib=%#v", src, got, want)
		}
	}
}

// TestDecodeLocalDestinationAllocationBound pins the deliberate cost of
// keeping a local destination visible to escape analysis while compiled
// reflection operations can reference it. The one operation-lifetime
// allocation must not grow with the number of decoded fields.
func TestDecodeLocalDestinationAllocationBound(t *testing.T) {
	decoder, err := CompileDecoder[typedTestRecord](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"id":1,"ok":true,"name":"sim","scores":[1,2,3],"number":4}`)
	var sink int
	allocs := testing.AllocsPerRun(200, func() {
		var value typedTestRecord
		if err := decoder.Decode(src, &value); err != nil {
			panic(err)
		}
		sink += value.ID
	})
	_ = sink
	if allocs > 1 {
		t.Fatalf("Decode into a local destination allocated %v times per run, want <=1", allocs)
	}
}

// mergeFixture pre-populates every kind of destination state so differential
// decodes expose any divergence from encoding/json's merge semantics.
func mergeFixture() typedTestDocument {
	next := typedTestRecord{ID: 42, OK: true, Name: "keep", Scores: [3]float64{7, 8, 9}, Number: json.Number("11")}
	return typedTestDocument{
		Items: []typedTestRecord{
			{ID: 1, OK: true, Name: "stale-a", Scores: [3]float64{1, 2, 3}, Number: json.Number("5")},
			{ID: 2, Name: "stale-b", Scores: [3]float64{4, 5, 6}},
		},
		Count: 9,
		Next:  &next,
	}
}

func TestMergeSemanticsMatchStdlib(t *testing.T) {
	decoder, err := CompileDecoder[typedTestDocument](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sources := []string{
		`{}`,
		`{"count":1}`,
		`{"items":[{"id":100}]}`,
		`{"items":[{"id":100},{"name":"only-name"},{"ok":true}]}`,
		`{"next":{"scores":[50]}}`,
		`{"next":null,"count":null}`,
		`{"items":null}`,
		`{"items":[{"scores":null,"number":null,"name":null}]}`,
		`{"next":{"id":null,"ok":null}}`,
	}
	for _, src := range sources {
		got := mergeFixture()
		want := mergeFixture()
		gotErr := decoder.Decode([]byte(src), &got)
		wantErr := json.Unmarshal([]byte(src), &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("%s: acceptance differs: simdjson=%v stdlib=%v", src, gotErr, wantErr)
		}
		if gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\nsimdjson %#v\nstdlib   %#v", src, got, want)
		}
	}
}

func TestReplaceSemantics(t *testing.T) {
	decoder, err := CompileDecoder[typedTestDocument](DecoderOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	dst := mergeFixture()
	if err := decoder.Decode([]byte(`{"count":3}`), &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Count != 3 || dst.Items != nil && len(dst.Items) != 0 || dst.Next != nil {
		t.Fatalf("replace decode kept stale state: %#v", dst)
	}
}

func FuzzMergeSemanticsMatchStdlib(f *testing.F) {
	f.Add([]byte(`{"items":[{"id":7},{"name":"x"}],"count":null}`))
	f.Add([]byte(`{"next":{"scores":[1]},"items":null}`))
	decoder, err := CompileDecoder[typedTestDocument](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<13 || !Valid(src) {
			return
		}
		got := mergeFixture()
		want := mergeFixture()
		gotErr := decoder.Decode(src, &got)
		wantErr := json.Unmarshal(src, &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("acceptance differs: simdjson=%v stdlib=%v", gotErr, wantErr)
		}
		if gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("merge decode differs:\nsimdjson %#v\nstdlib   %#v", got, want)
		}
	})
}

func TestDecodeEightDigitOverflowNarrowInts(t *testing.T) {
	// Exactly eight digits skipped the limit check on the SWAR path, so
	// values truncated silently into 8- and 16-bit destinations.
	type narrow struct {
		A int8   `json:"a"`
		B int16  `json:"b"`
		C uint8  `json:"c"`
		D uint16 `json:"d"`
	}
	for _, src := range []string{
		`{"a":12345678}`, `{"b":12345678}`, `{"c":12345678}`, `{"d":12345678}`,
		`{"a":-12345678}`, `{"b":-12345678}`,
	} {
		var got narrow
		err := Unmarshal([]byte(src), &got)
		var want narrow
		stdErr := json.Unmarshal([]byte(src), &want)
		if (err == nil) != (stdErr == nil) {
			t.Fatalf("%s: err=%v, encoding/json err=%v", src, err, stdErr)
		}
		if err == nil {
			t.Fatalf("%s: expected overflow error, decoded %+v", src, got)
		}
	}
	// Boundary neighbours must still decode.
	var ok16 struct {
		D uint16 `json:"d"`
	}
	if err := Unmarshal([]byte(`{"d":65535}`), &ok16); err != nil || ok16.D != 65535 {
		t.Fatalf("in-range decode failed: %v %+v", err, ok16)
	}
}

func TestDecodeIntegerDigitRunSweep(t *testing.T) {
	// The word-at-a-time short-run parser must agree with encoding/json for
	// every digit count, terminator, and destination width, including the
	// overflow boundaries of each width.
	terminators := []string{"}", ",\"x\":0}", " }", ".5}", "e2}", "E2}"}
	values := []string{
		"1", "12", "123", "1234", "12345", "123456", "1234567", "12345678",
		"123456789", "1234567890123456", "12345678901234567", "0",
		"127", "128", "255", "256", "32767", "32768", "65535", "65536",
		"2147483647", "2147483648", "4294967295", "4294967296",
		"9223372036854775807", "9223372036854775808",
		"18446744073709551615", "18446744073709551616",
	}
	check := func(src string, dec func([]byte) error, std func([]byte) error) {
		errA := dec([]byte(src))
		errB := std([]byte(src))
		if (errA == nil) != (errB == nil) {
			t.Fatalf("%s: simdjson err=%v, encoding/json err=%v", src, errA, errB)
		}
	}
	for _, v := range values {
		for _, sign := range []string{"", "-"} {
			for _, term := range terminators {
				src := `{"a":` + sign + v + term
				var a8 struct {
					A int8 `json:"a"`
				}
				var a16 struct {
					A int16 `json:"a"`
				}
				var a32 struct {
					A int32 `json:"a"`
				}
				var a64 struct {
					A int64 `json:"a"`
				}
				var u8 struct {
					A uint8 `json:"a"`
				}
				var u64v struct {
					A uint64 `json:"a"`
				}
				check(src, func(b []byte) error { return Unmarshal(b, &a8) }, func(b []byte) error { return json.Unmarshal(b, &a8) })
				check(src, func(b []byte) error { return Unmarshal(b, &a16) }, func(b []byte) error { return json.Unmarshal(b, &a16) })
				check(src, func(b []byte) error { return Unmarshal(b, &a32) }, func(b []byte) error { return json.Unmarshal(b, &a32) })
				check(src, func(b []byte) error { return Unmarshal(b, &a64) }, func(b []byte) error { return json.Unmarshal(b, &a64) })
				check(src, func(b []byte) error { return Unmarshal(b, &u8) }, func(b []byte) error { return json.Unmarshal(b, &u8) })
				check(src, func(b []byte) error { return Unmarshal(b, &u64v) }, func(b []byte) error { return json.Unmarshal(b, &u64v) })
				// Value equality when both succeed.
				var want, got struct {
					A int64 `json:"a"`
				}
				if json.Unmarshal([]byte(src), &want) == nil {
					if err := Unmarshal([]byte(src), &got); err != nil || got.A != want.A {
						t.Fatalf("%s: got %d (%v), want %d", src, got.A, err, want.A)
					}
				}
			}
		}
	}
}

type reuseMapDoc struct {
	E map[string]int `json:"e"`
}

type reuseSliceDoc struct {
	D []int `json:"d"`
	A int   `json:"a"`
}

type reuseMixedDoc struct {
	M map[string]int `json:"m"`
	S []string       `json:"s"`
}

type reuseElem struct {
	X int    `json:"x"`
	Y string `json:"y"`
}

// replaceEqualsFresh asserts that decoding a sequence of documents into one
// reused destination under Replace yields exactly what decoding the last
// document into a fresh destination does — the defining property of Replace.
func replaceEqualsFresh[T any](t *testing.T, docs ...string) {
	t.Helper()
	dec, err := CompileDecoder[T](DecoderOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	var reused T
	for _, doc := range docs {
		if err := dec.Decode([]byte(doc), &reused); err != nil {
			t.Fatal(err)
		}
	}
	var fresh T
	if err := dec.Decode([]byte(docs[len(docs)-1]), &fresh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reused, fresh) {
		t.Fatalf("Replace reused != fresh for %v:\n reused %#v\n fresh  %#v", docs, reused, fresh)
	}
}

// TestReusedDestinationSemantics pins the fixes for three reused-destination
// bugs found by differential hunting: default merge must match encoding/json,
// and Replace must decode a reused destination identically to a fresh one.
func TestReusedDestinationSemantics(t *testing.T) {
	// Default merge: an empty array drops the reused backing like
	// encoding/json's MakeSlice(T,0,0); keeping it would expose stale elements
	// when a later, longer array reused the retained capacity.
	t.Run("merge empty array vs stdlib", func(t *testing.T) {
		var got, want []reuseElem
		for _, doc := range []string{`[{"x":1},{"y":"leak"}]`, `[]`, `[{"x":9},{"x":0}]`} {
			if err := Unmarshal([]byte(doc), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(doc), &want); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("merge empty-array leaked stale data:\n simdjson %#v\n stdlib   %#v", got, want)
		}
	})

	t.Run("replace map replaced not merged", func(t *testing.T) {
		replaceEqualsFresh[reuseMapDoc](t, `{"e":{"a":1,"b":2}}`, `{"e":{"c":3}}`)
	})
	t.Run("replace absent slice becomes nil", func(t *testing.T) {
		replaceEqualsFresh[reuseSliceDoc](t, `{"d":[1,2,3,4,5]}`, `{"a":2}`)
	})
	t.Run("replace shrinking map and slice", func(t *testing.T) {
		replaceEqualsFresh[reuseMixedDoc](t, `{"m":{"a":1,"b":2,"c":3},"s":["x","y","z"]}`, `{"m":{"a":9},"s":["q"]}`)
	})
}
