package slopjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"unsafe"
)

type encoderPackedNames struct {
	First  int64  `json:"abcdefghijklm"`
	Second int64  `json:"nopqrstuvwxy"`
	Third  int64  `json:"qrstuvwxyzabcd"`
	Fourth string `json:"x<y"`
	Last   bool   `json:"z"`
}

func TestEncoderPackedNamesDoNotWritePastResult(t *testing.T) {
	encoder, err := CompileEncoder[encoderPackedNames](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !encoder.root.encSimple {
		t.Fatal("test type did not compile as a simple struct")
	}
	fields := encoder.root.encodeProgram.encFields
	packed := 0
	for _, field := range fields {
		if field.encNameLen != 0 {
			packed++
			if int(field.encNameLen) != len(field.encName) || len(field.encName) > 16 {
				t.Fatalf("invalid packed metadata for %q: len=%d encoded=%d", field.encName, field.encNameLen, len(field.encName))
			}
		}
	}
	if packed < 3 {
		t.Fatalf("only %d names used the packed path", packed)
	}
	if last := fields[len(fields)-1]; last.encNameLen != 0 {
		t.Fatalf("short tail name %q unexpectedly uses a wide store", last.encName)
	}

	value := encoderPackedNames{First: -1, Second: 2, Third: 3, Fourth: "<&>", Last: true}
	wantJSON, err := json.Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("pre:")
	for extra := 0; extra <= 32; extra++ {
		resultLen := len(prefix) + len(wantJSON)
		storage := bytes.Repeat([]byte{0xa5}, resultLen+extra+32)
		copy(storage, prefix)
		dst := storage[: len(prefix) : resultLen+extra]
		got, gotErr := encoder.AppendJSON(dst, &value)
		want := append(append([]byte(nil), prefix...), wantJSON...)
		if gotErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("AppendJSON(extra=%d) = %q, %v, want %q", extra, got, gotErr, want)
		}
		for i, b := range storage[len(got):] {
			if b != 0xa5 {
				t.Fatalf("AppendJSON(extra=%d) wrote past result at byte %d", extra, len(got)+i)
			}
		}
	}
}

type encoderPairLeaf struct {
	N int64 `json:"n"`
	// The omitempty member keeps this leaf outside nested-struct fusion so
	// the matrix still exercises the Struct pair opcodes; the zero value
	// is omitted by slopjson and encoding/json alike.
	Z int64 `json:"z,omitempty"`
}

type encoderPairMarshaler string

func (value encoderPairMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(value))
}

type encoderPairMatrix struct {
	A00 string               `json:"a00"`
	B00 string               `json:"b00"`
	A01 []int64              `json:"a01"`
	B01 string               `json:"b01"`
	A02 []int64              `json:"a02"`
	B02 encoderPairLeaf      `json:"b02"`
	A03 []int64              `json:"a03"`
	B03 []int64              `json:"b03"`
	A04 encoderPairLeaf      `json:"a04"`
	B04 encoderPairLeaf      `json:"b04"`
	A05 encoderPairMarshaler `json:"a05"`
	B05 encoderPairMarshaler `json:"b05"`
	A06 encoderPairLeaf      `json:"a06"`
	B06 []int64              `json:"b06"`
	A07 string               `json:"a07"`
	B07 []int64              `json:"b07"`
	A08 encoderPairMarshaler `json:"a08"`
	B08 encoderPairLeaf      `json:"b08"`
	A09 encoderPairMarshaler `json:"a09"`
	B09 string               `json:"b09"`
	A10 encoderPairLeaf      `json:"a10"`
	B10 string               `json:"b10"`
	A11 string               `json:"a11"`
	B11 encoderPairLeaf      `json:"b11"`
	A12 float64              `json:"a12"`
	B12 int64                `json:"b12"`
	A13 uint64               `json:"a13"`
	B13 uint64               `json:"b13"`
	A14 string               `json:"a14"`
	B14 float64              `json:"b14"`
	A15 encoderPairLeaf      `json:"a15"`
	B15 int64                `json:"b15"`
	A16 int64                `json:"a16"`
	B16 int64                `json:"b16"`
	A17 int64                `json:"a17"`
	B17 string               `json:"b17"`
	A18 string               `json:"a18"`
	B18 int64                `json:"b18"`
	A19 int64                `json:"a19"`
	B19 []int64              `json:"b19"`
	A20 []int64              `json:"a20"`
	B20 int64                `json:"b20"`
	A21 []int64              `json:"a21"`
	B21 any                  `json:"b21"`
	A22 any                  `json:"a22"`
	B22 []int64              `json:"b22"`
	A23 any                  `json:"a23"`
	B23 any                  `json:"b23"`
	A24 any                  `json:"a24"`
	B24 int64                `json:"b24"`
	A25 map[string]int64     `json:"a25"`
	B25 map[string]int64     `json:"b25"`
}

func TestEncoderPairMatrixMatchesStdlib(t *testing.T) {
	encoder, err := CompileEncoder[encoderPairMatrix](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const pairCount = 26
	wantOps := map[int]typedEncPairOp{
		0:  typedEncPairStringString,
		16: typedEncPairInt64Int64,
		17: typedEncPairInt64String,
		18: typedEncPairStringInt64,
		20: typedEncPairSliceInt64,
		25: typedEncPairMapMap,
	}
	fields := encoder.root.encodeProgram.encFields
	if !encoder.root.encSimple || len(fields) != pairCount*2 {
		t.Fatalf("unexpected pair plan: simple=%v fields=%d", encoder.root.encSimple, len(fields))
	}
	for i := range pairCount {
		want := wantOps[i] // Missing entries deliberately use typedEncPairFallback.
		if got := fields[i*2].pairOp; got != want {
			t.Fatalf("pair %d opcode = %d, want %d", i, got, want)
		}
	}

	value := encoderPairMatrix{
		A00: "left", B00: "right",
		A01: []int64{1, -2}, B01: "slice-string",
		A02: []int64{}, B02: encoderPairLeaf{N: 2},
		A03: nil, B03: []int64{3},
		A04: encoderPairLeaf{N: 4}, B04: encoderPairLeaf{N: -4},
		A05: "marshal-a", B05: "marshal-b",
		A06: encoderPairLeaf{N: 6}, B06: []int64{6, 7},
		A07: "string-slice", B07: []int64{},
		A08: "marshal-struct", B08: encoderPairLeaf{N: 8},
		A09: "marshal-string", B09: "plain",
		A10: encoderPairLeaf{N: 10}, B10: "struct-string",
		A11: "string-struct", B11: encoderPairLeaf{N: 11},
		A12: -12.5, B12: -12,
		A13: math.MaxUint64, B13: 13,
		A14: "string-float", B14: 14.25,
		A15: encoderPairLeaf{N: 15}, B15: -15,
		A16: math.MinInt64, B16: math.MaxInt64,
		A17: 17, B17: "int-string",
		A18: "string-int", B18: -18,
		A19: 19, B19: []int64{19},
		A20: []int64{20}, B20: -20,
		A21: []int64{21}, B21: "slice-any",
		A22: int64(22), B22: []int64{22},
		A23: true, B23: 23.5,
		A24: "any-int", B24: 24,
		A25: map[string]int64{"z": 25, "a": -25}, B25: map[string]int64{},
	}
	wantJSON, err := json.Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	for _, capacity := range []int{0, 1, len(wantJSON) - 1, len(wantJSON), len(wantJSON) + 32} {
		storage := bytes.Repeat([]byte{0xa5}, capacity+32)
		dst := storage[:0:capacity]
		got, gotErr := encoder.AppendJSON(dst, &value)
		if gotErr != nil || !bytes.Equal(got, wantJSON) {
			t.Fatalf("AppendJSON(cap=%d) = %s, %v, want %s", capacity, got, gotErr, wantJSON)
		}
		for i, b := range storage[capacity:] {
			if b != 0xa5 {
				t.Fatalf("AppendJSON(cap=%d) wrote past capacity at byte %d", capacity, capacity+i)
			}
		}
		if capacity >= len(got) {
			for i, b := range storage[len(got):capacity] {
				if b != 0xa5 {
					t.Fatalf("AppendJSON(cap=%d) wrote past result at byte %d", capacity, len(got)+i)
				}
			}
		}
	}
}

type encoderPairFirstFloatError struct {
	First  float64 `json:"first"`
	Second int64   `json:"second"`
}

type encoderPairSecondFloatError struct {
	First  string  `json:"first"`
	Second float64 `json:"second"`
}

type encoderPairAnyError struct {
	First  any `json:"first"`
	Second any `json:"second"`
}

type encoderPairBadMarshaler struct {
	Bad bool
}

func (value encoderPairBadMarshaler) MarshalJSON() ([]byte, error) {
	if value.Bad {
		return nil, errors.New("pair marshaler failed")
	}
	return []byte(`"ok"`), nil
}

type encoderPairMarshalerError struct {
	First  encoderPairBadMarshaler `json:"first"`
	Second encoderPairBadMarshaler `json:"second"`
}

func TestEncoderPairErrorsReportCorrectField(t *testing.T) {
	requireEncoderErrorPath(t, &encoderPairFirstFloatError{First: math.NaN()}, "first")
	requireEncoderErrorPath(t, &encoderPairSecondFloatError{Second: math.Inf(1)}, "second")
	requireEncoderErrorPath(t, &encoderPairAnyError{First: "ok", Second: make(chan int)}, "second")
	requireEncoderErrorPath(t, &encoderPairMarshalerError{Second: encoderPairBadMarshaler{Bad: true}}, "second")
}

func requireEncoderErrorPath[T any](t *testing.T, value *T, want string) {
	t.Helper()
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = encoder.AppendJSON(nil, value)
	var encodeErr *EncodeError
	if !errors.As(err, &encodeErr) {
		t.Fatalf("AppendJSON error = %v, want *EncodeError", err)
	}
	if encodeErr.Path != want {
		t.Fatalf("AppendJSON error path = %q, want %q (%v)", encodeErr.Path, want, err)
	}
}

// TestShortStringWordPathMatchesStdlib walks every string length the
// word-at-a-time fast path handles, with every flagged byte at every
// position, plus the clean case, in both HTML modes and at buffer
// capacities that force and skip the fast path. Failures here point at
// appendShortCleanJSONString's overlapped loads, padding mask, or
// unconditional word stores.
func TestShortStringWordPathMatchesStdlib(t *testing.T) {
	specials := []byte{'"', '\\', 0x00, 0x1F, '\n', '<', '>', '&', 0x80, 0xE2}
	var cases []string
	for n := 1; n <= 16; n++ {
		clean := strings.Repeat("x", n)
		cases = append(cases, clean)
		for pos := 0; pos < n; pos++ {
			for _, c := range specials {
				b := []byte(clean)
				b[pos] = c
				cases = append(cases, string(b))
			}
		}
	}
	cases = append(cases, "", "héllo", "日本語", "a b")
	for _, escapeHTML := range []bool{true, false} {
		for _, s := range cases {
			var want []byte
			if escapeHTML {
				buf, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				want = buf
			} else {
				var sb bytes.Buffer
				enc := json.NewEncoder(&sb)
				enc.SetEscapeHTML(false)
				if err := enc.Encode(s); err != nil {
					t.Fatal(err)
				}
				want = bytes.TrimSuffix(sb.Bytes(), []byte("\n"))
			}
			roomy := appendEncodedJSONString(make([]byte, 0, 64), s, escapeHTML)
			if string(roomy) != string(want) {
				t.Fatalf("appendEncodedJSONString(%q, html=%v) = %q, want %q", s, escapeHTML, roomy, want)
			}
			tight := appendEncodedJSONString(nil, s, escapeHTML)
			if string(tight) != string(want) {
				t.Fatalf("tight appendEncodedJSONString(%q, html=%v) = %q, want %q", s, escapeHTML, tight, want)
			}
			// The fast path stores whole words into slack; bytes past the
			// returned length must not disturb previously written content.
			prefix := []byte(`{"k":`)
			padded := append(make([]byte, 0, 64), prefix...)
			padded = appendEncodedJSONString(padded, s, escapeHTML)
			if string(padded[:len(prefix)]) != string(prefix) || string(padded[len(prefix):]) != string(want) {
				t.Fatalf("prefixed appendEncodedJSONString(%q, html=%v) = %q", s, escapeHTML, padded)
			}
		}
	}
}

type fusionInner struct {
	A int64  `json:"a"`
	B string `json:"b"`
}

type fusionEmpty struct{}

type fusionMid struct {
	Name  string      `json:"name"`
	In    fusionInner `json:"in"`
	Empty fusionEmpty `json:"empty"`
	Tail  fusionInner `json:"tail"`
}

type fusionOuter struct {
	First fusionInner `json:"first"` // fused child in first position
	Mid   fusionMid   `json:"mid"`   // two levels of fusion
	N     int64       `json:"n"`
	Last  fusionInner `json:"last"` // fused child in last position
}

func TestNestedStructFusionMatchesStdlib(t *testing.T) {
	enc, err := CompileEncoder[fusionOuter](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The plan must actually be flat: no struct-typed entries survive for
	// fusable children, and the close carries the fused braces.
	program := enc.root.encodeProgram
	for i := range program.encFields {
		if program.encFields[i].encOp == typedOpStruct {
			t.Fatalf("field %d still struct-typed after fusion", i)
		}
	}
	if string(program.encClose) != "}}" {
		t.Fatalf("encClose = %q", program.encClose)
	}

	v := fusionOuter{
		First: fusionInner{A: 1, B: "one <&> \"quoted\""},
		Mid: fusionMid{
			Name: "mid",
			In:   fusionInner{A: -2, B: "two"},
			Tail: fusionInner{A: 3, B: "three"},
		},
		N:    42,
		Last: fusionInner{A: 4, B: "four"},
	}
	got, err := enc.AppendJSON(nil, &v)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("fused output differs\n got %s\nwant %s", got, want)
	}

	// Errors inside fused children must report dotted paths.
	type withBad struct {
		In struct {
			F float64 `json:"f"`
		} `json:"in"`
	}
	bad := withBad{}
	bad.In.F = math.Inf(1)
	if _, err := Marshal(&bad); err == nil || !strings.Contains(err.Error(), "in.f") {
		t.Fatalf("fused error path = %v", err)
	}

	// Slices of fused structs run through the hoisted pair loop.
	rows := []fusionOuter{v, v, v}
	gotRows, err := Marshal(&rows)
	if err != nil {
		t.Fatal(err)
	}
	wantRows, _ := json.Marshal(rows)
	if string(gotRows) != string(wantRows) {
		t.Fatalf("fused slice output differs\n got %s\nwant %s", gotRows, wantRows)
	}
}

func TestNestedStructFusionDepthLimit(t *testing.T) {
	// A fused static level still counts against the depth limit exactly as
	// the recursive walk counted it: wrapping a two-level fused struct in
	// slices up to the limit must fail at the same nesting as before. Stable
	// encoding/json v1 has no encoder nesting limit, so its build must retain
	// the same fused structure without applying this guard.
	type leaf struct {
		N int64 `json:"n"`
	}
	type box struct {
		L leaf `json:"l"`
	}
	enc, err := CompileEncoder[box](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if enc.root.encodeProgram.encFusedExtra != 1 {
		t.Fatalf("encFusedExtra = %d, want 1", enc.root.encodeProgram.encFusedExtra)
	}
	var v box
	if !encoderHasDepthLimit {
		e := encodeState{dst: nil, escapeHTML: true, depth: defaultMaxDepth + 1}
		if err := e.encodeStruct(enc.root, unsafe.Pointer(&v)); err != nil {
			t.Fatalf("stable encoder applied a nesting limit: %v", err)
		}
		return
	}
	e := encodeState{dst: nil, escapeHTML: true}
	e.depth = defaultMaxDepth - 1
	if err := e.encodeStruct(enc.root, unsafe.Pointer(&v)); err == nil {
		t.Fatal("expected depth error: box at max-1 puts leaf past the limit")
	}
	e = encodeState{dst: nil, escapeHTML: true}
	e.depth = defaultMaxDepth - 2
	if err := e.encodeStruct(enc.root, unsafe.Pointer(&v)); err != nil {
		t.Fatalf("box at max-2 must fit exactly as the recursive walk did: %v", err)
	}
}
