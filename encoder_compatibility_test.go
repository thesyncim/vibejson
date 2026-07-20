package simdjson

// Encoder compatibility contracts compare Marshal and compiled AppendJSON
// against encoding/json on surfaces not covered by the core encoder tests.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// checkEncoderParity encodes v with stdlib and both simdjson entry points and reports
// any acceptance or byte divergence.
func checkEncoderParity[T any](t *testing.T, label string, v T) {
	t.Helper()
	want, wantErr := json.Marshal(&v)

	got, gotErr := Marshal(&v)
	if (gotErr == nil) != (wantErr == nil) {
		t.Errorf("%s: Marshal acceptance differs: simdjson=%v stdlib=%v", label, gotErr, wantErr)
		return
	}
	if gotErr == nil && !bytes.Equal(got, want) {
		t.Errorf("%s: Marshal bytes differ:\nsimdjson %s\nstdlib   %s", label, got, want)
	}

	encoder, compileErr := CompileEncoder[T](EncoderOptions{})
	if compileErr != nil {
		if wantErr == nil {
			t.Errorf("%s: CompileEncoder failed (%v) but stdlib accepts", label, compileErr)
		}
		return
	}
	got2, gotErr2 := encoder.AppendJSON(nil, &v)
	if (gotErr2 == nil) != (wantErr == nil) {
		t.Errorf("%s: AppendJSON acceptance differs: simdjson=%v stdlib=%v", label, gotErr2, wantErr)
		return
	}
	if gotErr2 == nil && !bytes.Equal(got2, want) {
		t.Errorf("%s: AppendJSON bytes differ:\nsimdjson %s\nstdlib   %s", label, got2, want)
	}
}

// `,string` tags on types with custom marshalers. stdlib sets the quoted
// flag from the field's Kind but the marshaler encoders ignore it, so the
// custom output is emitted unquoted.

type contractQuotedJSONMarshaler int

func (q contractQuotedJSONMarshaler) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "%d", int(q)), nil
}

type contractQuotedTextMarshaler int

func (q contractQuotedTextMarshaler) MarshalText() ([]byte, error) {
	return fmt.Appendf(nil, "t%d", int(q)), nil
}

func TestStringOptionOnMarshalers(t *testing.T) {
	type jm struct {
		Q contractQuotedJSONMarshaler `json:"q,string"`
	}
	checkEncoderParity(t, "json marshaler with ,string", jm{Q: 7})

	type tm struct {
		Q contractQuotedTextMarshaler `json:"q,string"`
	}
	checkEncoderParity(t, "text marshaler with ,string", tm{Q: 7})

	type jmOmit struct {
		Q contractQuotedJSONMarshaler `json:"q,string,omitempty"`
	}
	checkEncoderParity(t, "json marshaler ,string,omitempty zero", jmOmit{})
	checkEncoderParity(t, "json marshaler ,string,omitempty nonzero", jmOmit{Q: 3})
}

// Map keys through TextMarshaler: nil pointer keys, string-kind keys that
// also implement TextMarshaler, ordering by marshaled form, key errors.

type contractPtrTextKey struct{ N int }

func (k *contractPtrTextKey) MarshalText() ([]byte, error) {
	return fmt.Appendf(nil, "p%d", k.N), nil
}

type contractErrKey struct{}

func (contractErrKey) MarshalText() ([]byte, error) { return nil, fmt.Errorf("key boom") }

func marshalBothWithRecover[T any](t *testing.T, v T) (got []byte, gotErr error, gotPanic any, want []byte, wantErr error, wantPanic any) {
	t.Helper()
	func() {
		defer func() { wantPanic = recover() }()
		want, wantErr = json.Marshal(&v)
	}()
	func() {
		defer func() { gotPanic = recover() }()
		got, gotErr = Marshal(&v)
	}()
	return
}

func TestNilPointerTextMarshalerMapKey(t *testing.T) {
	v := map[*contractPtrTextKey]int{nil: 1, {N: 2}: 3}
	got, gotErr, gotPanic, want, wantErr, wantPanic := marshalBothWithRecover(t, v)
	if (gotPanic != nil) != (wantPanic != nil) {
		t.Errorf("nil ptr text key: panic differs: simdjson=%v stdlib=%v", gotPanic, wantPanic)
		return
	}
	if (gotErr == nil) != (wantErr == nil) {
		t.Errorf("nil ptr text key: acceptance differs: simdjson=%v stdlib=%v", gotErr, wantErr)
		return
	}
	if gotErr == nil && !bytes.Equal(got, want) {
		t.Errorf("nil ptr text key:\nsimdjson %s\nstdlib   %s", got, want)
	}
}

func TestMapKeyEdges(t *testing.T) {
	// Single entry: with multiple entries both libraries emit duplicate
	// "SHOULD-NOT-BE-USED" keys in nondeterministic order under jsonv2.
	checkEncoderParity(t, "string-kind key implementing TextMarshaler",
		map[stringTextKey]int{"raw": 5})
	checkEncoderParity(t, "text keys sorted by marshaled form",
		map[textKey]int{{A: 10, B: 0}: 1, {A: 2, B: 0}: 2, {A: 1, B: 99}: 3})
	checkEncoderParity(t, "int8 keys with negatives",
		map[int8]int{-128: 1, -1: 2, 0: 3, 127: 4, 10: 5, 2: 6})
	checkEncoderParity(t, "uintptr keys",
		map[uintptr]string{0: "a", 18446744073709551615: "b", 7: "c"})
	checkEncoderParity(t, "erroring text key", map[contractErrKey]int{{}: 1})
	checkEncoderParity(t, "NaN value under sorted keys", map[string]float64{"a": 1, "b": math.NaN()})
	checkEncoderParity(t, "+Inf map value", map[string]float64{"x": math.Inf(1)})
	checkEncoderParity(t, "text key with invalid utf8", map[textKey]int{{A: -1, B: -2}: 9})
}

// []T where T's underlying kind is byte but T has its own marshaler:
// stdlib only base64-encodes byte slices whose element type has no
// Marshaler/TextMarshaler methods.

type contractCustomByte uint8

func (b contractCustomByte) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "%d", int(b)+1), nil
}

type contractCustomTextByte uint8

func (b contractCustomTextByte) MarshalText() ([]byte, error) {
	return fmt.Appendf(nil, "x%d", int(b)), nil
}

func TestCustomByteSliceElements(t *testing.T) {
	type doc struct {
		B []contractCustomByte `json:"b"`
	}
	checkEncoderParity(t, "custom-marshaler byte slice", doc{B: []contractCustomByte{1, 2}})

	type tdoc struct {
		B []contractCustomTextByte `json:"b"`
	}
	checkEncoderParity(t, "custom-text byte slice", tdoc{B: []contractCustomTextByte{1, 2}})

	type adoc struct {
		B [2]byte `json:"b"`
	}
	checkEncoderParity(t, "byte array stays numeric", adoc{B: [2]byte{1, 2}})

	type named struct {
		B namedBlob `json:"b"`
	}
	checkEncoderParity(t, "plain named byte slice stays base64", named{B: namedBlob{1, 2}})
}

// omitempty over every kind, including the zero-length array quirk.

type contractZeroArray struct {
	A [0]int         `json:"a,omitempty"`
	B [2]int         `json:"b,omitempty"`
	C struct{}       `json:"c,omitempty"`
	D any            `json:"d,omitempty"`
	E *int           `json:"e,omitempty"`
	F json.Number    `json:"f,omitempty"`
	G map[string]int `json:"g,omitempty"`
	H []int          `json:"h,omitempty"`
	I uintptr        `json:"i,omitempty"`
	J bool           `json:"j,string,omitempty"`
	K string         `json:"k,string,omitempty"`
	L float32        `json:"l,omitempty"`
}

func TestOmitemptyKinds(t *testing.T) {
	zero := 0
	checkEncoderParity(t, "all zero omitempty", contractZeroArray{})
	checkEncoderParity(t, "zero pointer target kept", contractZeroArray{E: &zero})
	checkEncoderParity(t, "interface holding zero kept", contractZeroArray{D: 0})
	checkEncoderParity(t, "empty-but-non-nil map omitted", contractZeroArray{G: map[string]int{}})
	checkEncoderParity(t, "empty slice omitted", contractZeroArray{H: []int{}})
	checkEncoderParity(t, "quoted string non-empty", contractZeroArray{K: "x"})
	checkEncoderParity(t, "negative zero float32 omitted", contractZeroArray{L: float32(math.Copysign(0, -1))})

	type omitTime struct {
		T time.Time `json:"t,omitempty"`
	}
	checkEncoderParity(t, "zero time not omitted", omitTime{})

	type omitIface struct {
		S fmt.Stringer `json:"s,omitempty"`
	}
	checkEncoderParity(t, "nil non-empty interface omitted", omitIface{})
}

// Struct shape edge cases: unexported embedded pointers and promoted
// marshalers.

// Embedded pointer to unexported struct type: encode reads through it.
type contractUnexpPtrEmbed struct {
	*hidden
	Top int `json:"top"`
}

func TestUnexportedPointerEmbed(t *testing.T) {
	checkEncoderParity(t, "nil unexported embedded pointer", contractUnexpPtrEmbed{Top: 1})
	checkEncoderParity(t, "set unexported embedded pointer", contractUnexpPtrEmbed{hidden: &hidden{Inner: "i"}, Top: 2})
}

// Promoted MarshalJSON from an embedded type takes over the whole struct.
type contractEmbedsTime struct {
	time.Time
	Ignored int `json:"ignored"`
}

func TestPromotedMarshalerTakesOver(t *testing.T) {
	checkEncoderParity(t, "struct embedding time.Time", contractEmbedsTime{
		Time:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Ignored: 42,
	})
}

// Deep non-cyclic pointer nesting: stdlib Marshal has no depth limit,
// only cycle detection over identical pointers.

type contractChain struct {
	Next *contractChain `json:"next,omitempty"`
	V    int            `json:"v,omitempty"`
}

func buildContractChain(depth int) *contractChain {
	head := &contractChain{V: 1}
	for range depth {
		head = &contractChain{Next: head}
	}
	return head
}

func TestDeepPointerNesting(t *testing.T) {
	for _, depth := range []int{500, 9000, 12000} {
		v := buildContractChain(depth)
		want, wantErr := json.Marshal(v)
		got, gotErr := Marshal(v)
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("depth %d: acceptance differs: simdjson=%v stdlib=%v", depth, gotErr, wantErr)
			continue
		}
		if gotErr == nil && !bytes.Equal(got, want) {
			t.Errorf("depth %d: bytes differ (len %d vs %d)", depth, len(got), len(want))
		}
	}

	// Actual cycle: both must error rather than hang.
	a := &contractChain{}
	a.Next = a
	_, gotErr := Marshal(a)
	_, wantErr := json.Marshal(a)
	if (gotErr == nil) != (wantErr == nil) {
		t.Errorf("cycle: acceptance differs: simdjson=%v stdlib=%v", gotErr, wantErr)
	}
}

// MarshalJSON output shapes: nil, empty, whitespace-padded, null, invalid
// JSON, invalid UTF-8 (documented strictness carve-out), U+2028 raw bytes,
// HTML specials; TextMarshaler with invalid UTF-8; panicking marshalers.

type contractRawOut struct{ Out string }

func (r contractRawOut) MarshalJSON() ([]byte, error) {
	if r.Out == "<nil>" {
		return nil, nil
	}
	return []byte(r.Out), nil
}

type contractTextOut struct{ Out string }

func (r contractTextOut) MarshalText() ([]byte, error) { return []byte(r.Out), nil }

type contractPanicMarshaler struct{}

func (contractPanicMarshaler) MarshalJSON() ([]byte, error) { panic("marshaler exploded") }

func TestMarshalerOutputShapes(t *testing.T) {
	outputs := []string{
		"<nil>",                          // nil slice
		"",                               // empty
		"null",                           // bare null passthrough
		" null ",                         // padded null
		` "x" `,                          // padded string
		"{\"a\" : 1,\n\"b\":[ 1 , 2 ] }", // needs compaction
		`{"a":1}`,                        // already compact
		`"<&>"`,                          // HTML re-escape
		"\"\u2028\u2029\"",               // raw line separators re-escape
		"\"\\u2028\"",                    // pre-escaped passthrough
		`{`,                              // invalid
		`123 456`,                        // trailing data
		"[1,2,]",                         // trailing comma
		"\t[1]\n",                        // whitespace around array
		`"tab	char"`,                     // raw tab inside string: invalid JSON both ways
	}
	for _, out := range outputs {
		type doc struct {
			V contractRawOut `json:"v"`
		}
		checkEncoderParity(t, fmt.Sprintf("marshaler output %q", out), doc{V: contractRawOut{Out: out}})
	}
}

func TestMarshalerInvalidUTF8CarveOut(t *testing.T) {
	// Documented strictness divergence: simdjson validates MarshalJSON output
	// as strict JSON including UTF-8; stdlib's compact() does not examine
	// string contents. Both must at least be deterministic; record whichever
	// way each library goes so the divergence stays exactly the carve-out.
	type doc struct {
		V contractRawOut `json:"v"`
	}
	v := doc{V: contractRawOut{Out: "\"\xff\""}}
	want, wantErr := json.Marshal(&v)
	got, gotErr := Marshal(&v)
	if wantErr != nil {
		t.Fatalf("stdlib unexpectedly rejects invalid UTF-8 from MarshalJSON: %v", wantErr)
	}
	if gotErr == nil {
		if !bytes.Equal(got, want) {
			t.Errorf("invalid UTF-8 passthrough differs:\nsimdjson %s\nstdlib   %s", got, want)
		}
		t.Logf("note: simdjson accepted invalid UTF-8 marshaler output (carve-out says reject)")
	}
}

// TestMarshalerLoneSurrogateRejected pins the deliberate encode/decode
// symmetry documented in the README: a MarshalJSON or json.RawMessage emitting
// a lone \uXXXX surrogate is rejected, because simdjson also rejects that byte
// sequence on decode. stdlib passes it through (and substitutes U+FFFD when it
// reads it back). Emitting it here would produce JSON simdjson cannot itself
// consume, so rejection keeps the round trip consistent.
func TestMarshalerLoneSurrogateRejected(t *testing.T) {
	type doc struct {
		V contractRawOut `json:"v"`
	}
	for _, out := range []string{`"\ud83d"`, `"\udc00"`, `"a\ud800b"`} {
		v := doc{V: contractRawOut{Out: out}}
		if _, err := Marshal(&v); err == nil {
			t.Errorf("Marshal accepted lone-surrogate marshaler output %q; expected rejection (encode/decode symmetry)", out)
		}
		// The same bytes must indeed be rejected on decode, proving symmetry.
		if err := Unmarshal([]byte(out), new(string)); err == nil {
			t.Errorf("decode of %q unexpectedly accepted; symmetry claim is wrong", out)
		}
	}
}

func TestTextMarshalerInvalidUTF8(t *testing.T) {
	type doc struct {
		V contractTextOut `json:"v"`
	}
	for _, out := range []string{"\xff", "a\xffb", "\xed\xa0\x80", "ok\xc3"} {
		checkEncoderParity(t, fmt.Sprintf("text marshaler output %q", out), doc{V: contractTextOut{Out: out}})
	}
}

func TestPanickingMarshalerPropagates(t *testing.T) {
	type doc struct {
		V contractPanicMarshaler `json:"v"`
	}
	v := doc{}
	_, _, gotPanic, _, _, wantPanic := marshalBothWithRecover(t, v)
	if (gotPanic != nil) != (wantPanic != nil) {
		t.Errorf("panic propagation differs: simdjson=%v stdlib=%v", gotPanic, wantPanic)
	}
}

// json.Number literal acceptance parity.

func TestNumberLiterals(t *testing.T) {
	literals := []string{
		"", "0", "-0", "1", "-1", "0.5", "1.", ".5", "-", "+1", "01", "0123",
		"1e", "1e+", "1E5", "1e-0", "1e309", "1e999", "-1.5e-300",
		"9007199254740993", "18446744073709551616", "1e+21", "  1", "1 ",
		"NaN", "Infinity", "0x10", "1_000",
	}
	type doc struct {
		N json.Number `json:"n"`
	}
	for _, lit := range literals {
		checkEncoderParity(t, fmt.Sprintf("number literal %q", lit), doc{N: json.Number(lit)})
	}
	// json.Number inside any and as map value.
	for _, lit := range []string{"5.5", "1e", ""} {
		checkEncoderParity(t, fmt.Sprintf("any number %q", lit), any(json.Number(lit)))
		checkEncoderParity(t, fmt.Sprintf("map number %q", lit), map[string]json.Number{"k": json.Number(lit)})
	}
	type qdoc struct {
		N json.Number `json:"n,string"`
	}
	checkEncoderParity(t, "quoted empty number", qdoc{})
	checkEncoderParity(t, "quoted number", qdoc{N: "5.5"})
}

// Long-string escape parity: specials at every offset around SIMD chunk
// boundaries, in both HTML modes, against the stdlib Encoder for the
// no-escape mode.

func stdlibNoHTML(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	requireNoTestError(t, enc.Encode(v))
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func TestLongStringSpecialOffsets(t *testing.T) {
	specials := []string{"\"", "\\", "\x00", "\x1f", "\n", "\x7f", "<", ">", "&",
		" ", " ", "\xff", "\xed\xa0\x80", "é", "日"}
	lengths := []int{17, 31, 32, 33, 47, 63, 64, 65, 100, 127, 128, 129}
	encPlain := mustCompileTestEncoder[string](t, EncoderOptions{})
	encNoHTML := mustCompileTestEncoder[string](t, EncoderOptions{DisableHTMLEscaping: true})
	for _, n := range lengths {
		base := strings.Repeat("a", n)
		for pos := 0; pos <= n; pos += 1 {
			for _, sp := range specials {
				if pos > n {
					continue
				}
				s := base[:pos] + sp + base[pos:]
				want, wantErr := json.Marshal(s)
				got, gotErr := encPlain.AppendJSON(nil, &s)
				if (gotErr == nil) != (wantErr == nil) || !bytes.Equal(got, want) {
					t.Fatalf("html mode len=%d pos=%d special=%q:\nsimdjson %q err=%v\nstdlib   %q err=%v",
						n, pos, sp, got, gotErr, want, wantErr)
				}
				wantRaw := stdlibNoHTML(t, s)
				gotRaw, gotErrRaw := encNoHTML.AppendJSON(nil, &s)
				if gotErrRaw != nil || !bytes.Equal(gotRaw, wantRaw) {
					t.Fatalf("raw mode len=%d pos=%d special=%q:\nsimdjson %q err=%v\nstdlib   %q",
						n, pos, sp, gotRaw, gotErrRaw, wantRaw)
				}
			}
		}
	}
}

func TestControlBytesAllValues(t *testing.T) {
	enc := mustCompileTestEncoder[string](t, EncoderOptions{})
	for c := 0; c < 0x20; c++ {
		s := "pad-pad-pad-pad-pad" + string(rune(c)) + "tail-tail-tail-tail"
		want, _ := json.Marshal(s)
		got, gotErr := enc.AppendJSON(nil, &s)
		if gotErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("control byte %#x: simdjson %q err=%v, stdlib %q", c, got, gotErr, want)
		}
	}
}

// any/interface contents: RawMessage compaction, typed nils, exotic
// nesting.

type contractValueMarshalerStruct struct{}

func (contractValueMarshalerStruct) MarshalJSON() ([]byte, error) { return []byte(`"vm"`), nil }

func TestAnyContents(t *testing.T) {
	checkEncoderParity(t, "raw message inside any compacts", any(json.RawMessage("{\"x\" : 1 }")))
	checkEncoderParity(t, "nil raw message inside any", any(json.RawMessage(nil)))
	checkEncoderParity(t, "empty raw message inside any", any(json.RawMessage{}))
	checkEncoderParity(t, "typed nil plain pointer", any((*int)(nil)))
	checkEncoderParity(t, "typed nil pointer with value-receiver marshaler", any((*contractValueMarshalerStruct)(nil)))
	checkEncoderParity(t, "typed nil pointer with pointer-receiver marshaler", any((*retainingCustomReceiver)(nil)))
	checkEncoderParity(t, "typed nil map", any(map[string]int(nil)))
	checkEncoderParity(t, "typed nil slice", any([]int(nil)))
	checkEncoderParity(t, "nil interface in slice", []any{nil, 1, "x"})
	checkEncoderParity(t, "float32 inside any", any(float32(1.5)))
	checkEncoderParity(t, "uint64 max inside any", any(uint64(math.MaxUint64)))
	checkEncoderParity(t, "map with exotic keys inside any", any(map[string]any{
		" ": 1, "<&>": 2, "": 3, "\x01": 4, "é": 5,
	}))
	checkEncoderParity(t, "text-marshaler map inside any", any(map[textKey]bool{{A: 1, B: 2}: true}))
	checkEncoderParity(t, "raw message nested in map in any", any(map[string]any{
		"r": json.RawMessage(" [ 1 ,2] "),
	}))
	checkEncoderParity(t, "chan inside nested any errors", any(map[string]any{"c": make(chan int)}))
}

// time.Time parity across zones, precision, and error acceptance.

func TestTimeParity(t *testing.T) {
	zones := []*time.Location{
		time.UTC,
		time.FixedZone("plus", 5*3600+1800),
		time.FixedZone("minus", -(9*3600 + 45*60)),
	}
	base := []time.Time{
		{},
		time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(2026, 7, 14, 1, 2, 3, 999999999, time.UTC),
		time.Date(2026, 7, 14, 1, 2, 3, 4000, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(-1, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 14, 1, 2, 3, 0, time.FixedZone("secoff", 3661)),
	}
	type doc struct {
		T time.Time `json:"t"`
	}
	for _, loc := range zones {
		for _, ts := range base {
			v := doc{T: ts.In(loc)}
			checkEncoderParity(t, fmt.Sprintf("time %v in %v", ts, loc), v)
		}
	}
	// Monotonic-clock-carrying time.
	checkEncoderParity(t, "time.Now monotonic", struct {
		T time.Time `json:"t"`
	}{T: time.Now()})
}

// Float spellings at documented thresholds (exact spot checks on top of
// the random differential suites).

func TestFloatThresholds(t *testing.T) {
	values := []float64{
		1e20, 1e21, 9.999999999999997e20, 1.0000000000000001e21,
		1e-6, 1e-7, 9.999999999999999e-7,
		5e-324, math.SmallestNonzeroFloat64, -5e-324,
		math.MaxFloat64, -math.MaxFloat64,
		float64(math.MaxFloat32), float64(math.SmallestNonzeroFloat32),
		0.1, 2.2250738585072014e-308, // smallest normal
	}
	type doc struct {
		F float64 `json:"f"`
		G float32 `json:"g"`
	}
	for _, f := range values {
		g := float32(f)
		if math.IsInf(float64(g), 0) {
			g = 0
		}
		checkEncoderParity(t, fmt.Sprintf("float %g", f), doc{F: f, G: g})
	}
	checkEncoderParity(t, "NaN top-level field errors", struct {
		F float64 `json:"f"`
	}{F: math.NaN()})
	checkEncoderParity(t, "float32 quoted threshold", struct {
		G float32 `json:"g,string"`
	}{G: 1e21})
}

// AppendJSON error-path contract: length-unchanged result, prefix intact.

func TestAppendJSONErrorPathPreservesPrefix(t *testing.T) {
	type doc struct {
		A string  `json:"a"`
		F float64 `json:"f"`
	}
	enc := mustCompileTestEncoder[doc](t, EncoderOptions{})
	prefix := []byte(`{"keep":true}`)
	storage := make([]byte, 0, 256)
	storage = append(storage, prefix...)
	v := doc{A: "text", F: math.NaN()}
	out, encErr := enc.AppendJSON(storage, &v)
	if encErr == nil {
		t.Fatal("expected NaN error")
	}
	if len(out) != len(prefix) {
		t.Fatalf("error path changed length: %d, want %d", len(out), len(prefix))
	}
	if !bytes.Equal(out, prefix) {
		t.Fatalf("error path corrupted prefix: %q", out)
	}
}

// Pinpoint the encoder depth threshold for pointer chains and show the
// decode->encode asymmetry: a document simdjson decodes cannot be re-encoded.

func TestDepthThresholdAndRoundTrip(t *testing.T) {
	// Each list node costs two depth units in the encoder (pointer + struct).
	for _, tc := range []struct{ nodes int }{{4999}, {5000}, {5001}, {6000}} {
		v := buildContractChain(tc.nodes - 1) // total nodes = tc.nodes
		_, wantErr := json.Marshal(v)
		_, gotErr := Marshal(v)
		t.Logf("nodes=%d simdjson err=%v stdlib err=%v", tc.nodes, gotErr != nil, wantErr != nil)
	}

	// Build JSON nested 6000 objects deep. simdjson decodes it (6000 < 10000
	// containers) — can it re-encode its own decode?
	depth := 6000
	var sb strings.Builder
	for range depth {
		sb.WriteString(`{"next":`)
	}
	sb.WriteString(`{"v":1}`)
	for range depth {
		sb.WriteString(`}`)
	}
	src := []byte(sb.String())
	var head contractChain
	if err := Unmarshal(src, &head); err != nil {
		t.Fatalf("simdjson failed to decode depth-%d doc: %v", depth, err)
	}
	_, gotErr := Marshal(&head)
	var stdHead contractChain
	if err := json.Unmarshal(src, &stdHead); err != nil {
		t.Fatalf("stdlib failed to decode: %v", err)
	}
	_, wantErr := json.Marshal(&stdHead)
	if (gotErr == nil) != (wantErr == nil) {
		t.Errorf("re-encode of own decode at depth %d: simdjson err=%v, stdlib err=%v", depth, gotErr != nil, wantErr)
	}
}

// DisableHTMLEscaping parity for field names and NaN inside any.

func TestDisableHTMLEscapingFieldNames(t *testing.T) {
	type doc struct {
		A string `json:"a<b"`
		B string `json:"c&d"`
	}
	v := doc{A: "<x>", B: "&y"}
	want := stdlibNoHTML(t, &v)
	enc := mustCompileTestEncoder[doc](t, EncoderOptions{DisableHTMLEscaping: true})
	got, err := enc.AppendJSON(nil, &v)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("no-escape field names: simdjson %s err=%v, stdlib %s", got, err, want)
	}
}

func TestNaNInsideAny(t *testing.T) {
	checkEncoderParity(t, "NaN inside any", any(math.NaN()))
	checkEncoderParity(t, "Inf inside []any", []any{1.0, math.Inf(-1)})
	checkEncoderParity(t, "NaN float32 field", struct {
		F float32 `json:"f"`
	}{F: float32(math.NaN())})
}

// Top-level scalars and containers via the generic entry point.

func TestTopLevelValues(t *testing.T) {
	checkEncoderParity(t, "top-level string with specials", "a\"b\\c\ncontrol\x01<&> end")
	checkEncoderParity(t, "top-level negative zero", math.Copysign(0, -1))
	checkEncoderParity(t, "top-level nil byte slice", []byte(nil))
	checkEncoderParity(t, "top-level empty byte slice", []byte{})
	checkEncoderParity(t, "top-level byte slice", []byte{0, 1, 254, 255})
	checkEncoderParity(t, "top-level nil map", map[string]int(nil))
	checkEncoderParity(t, "top-level bool", true)
	checkEncoderParity(t, "top-level json.RawMessage", json.RawMessage(` {"a": 1} `))
	checkEncoderParity(t, "top-level pointer to pointer", func() **int { x := 5; p := &x; return &p }())
}
