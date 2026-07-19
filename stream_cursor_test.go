package simdjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// cursorToAny drains one value through the public cursor API into the same Go
// shapes Value.Any produces, so the two paths can be compared field by field.
func cursorToAny(c *ValueCursor) (any, error) {
	switch c.Kind() {
	case Null:
		if !c.Null() {
			return nil, fmt.Errorf("cursor: null literal not consumed")
		}
		return nil, nil
	case Bool:
		return c.Bool()
	case Number:
		s, err := c.NumberText()
		return json.Number(s), err
	case String:
		return c.Text()
	case Array:
		if err := c.BeginArray(); err != nil {
			return nil, err
		}
		out := make([]any, 0, 4)
		for {
			ok, err := c.NextElement()
			if err != nil {
				return nil, err
			}
			if !ok {
				return out, nil
			}
			v, err := cursorToAny(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	case Object:
		if err := c.BeginObject(); err != nil {
			return nil, err
		}
		out := make(map[string]any)
		for {
			key, ok, err := c.NextField()
			if err != nil {
				return nil, err
			}
			if !ok {
				return out, nil
			}
			v, err := cursorToAny(c)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
	default:
		return nil, fmt.Errorf("cursor: no value at cursor")
	}
}

// cursorDifferentialStreams is the shared correctness corpus: every scalar
// kind, escapes, unicode, duplicate and escaped keys, deep nesting, pretty
// printing, and multi-value streams.
func cursorDifferentialStreams() []string {
	deep := strings.Repeat("[", 100) + "1" + strings.Repeat("]", 100)
	return []string{
		`{}`, `[]`, `[[]]`, `{"a":{}}`, `[{},{}]`,
		`null`, `true`, `false`,
		`0`, `-0`, `123`, `-123.456e+7`, `1e-300`, `0.5`, `2.5e1`,
		`9223372036854775807`, `-9223372036854775808`, `18446744073709551615`,
		`1.7976931348623157e308`, `123456789012345678901234567890`,
		`""`, `"plain"`, `"Aé𝄞"`, `"tab\tnew\nline"`,
		`"quote\" backslash\\ slash\/"`,
		`"` + strings.Repeat("long string content across vector widths ", 4) + `"`,
		`"héllo wörld    🌍"`,
		`{"a\nb":1,"":2,"kéy":"v"}`,
		`{"k":1,"k":2}`,
		"{\n  \"pretty\": [\n    1,\n    {\"deep\": true}\n  ]\n}",
		deep,
		`{"a":1}` + "\n" + `{"a":2}` + "\n",
		"1 2 3",
		`"a""b"`,
		`[1,"two",3.5,true,null,{"x":[]}]` + "\n" + `false`,
		string(citmLikeJSON(3)),
		string(coordRingsJSON(5)),
		string(floatArrayJSON(16)),
		string(sciFloatArrayJSON(8)),
		string(intArrayJSON(16)),
	}
}

// TestValueCursorDifferential compares the cursor's observed values against
// Parse's for every stream in the corpus, delivered whole and torn into
// arbitrary chunks, and requires clean Finish and Skip walks on each value.
func TestValueCursorDifferential(t *testing.T) {
	for i, stream := range cursorDifferentialStreams() {
		for _, torn := range []bool{false, true} {
			var r *Reader
			if torn {
				r = NewReaderSize(&tornReader{data: []byte(stream), state: uint64(i)*2 + 1}, 64)
			} else {
				r = NewReaderSize(strings.NewReader(stream), 64)
			}
			values := 0
			for r.Next() {
				values++
				c := r.Cursor()
				got, err := cursorToAny(&c)
				if err != nil {
					t.Fatalf("stream %d torn=%v: cursor walk: %v\nstream: %.120q", i, torn, err, stream)
				}
				if err := c.Finish(); err != nil {
					t.Fatalf("stream %d torn=%v: Finish after full walk: %v", i, torn, err)
				}
				v, perr := Parse(r.Bytes())
				if perr != nil {
					t.Fatalf("stream %d torn=%v: Parse rejects a value Next accepted: %v", i, torn, perr)
				}
				if want := v.Any(); !reflect.DeepEqual(got, want) {
					t.Fatalf("stream %d torn=%v: cursor sees %#v, Parse sees %#v", i, torn, got, want)
				}
				skip := r.Cursor()
				if err := skip.Skip(); err != nil {
					t.Fatalf("stream %d torn=%v: Skip: %v", i, torn, err)
				}
				if err := skip.Finish(); err != nil {
					t.Fatalf("stream %d torn=%v: Finish after Skip: %v", i, torn, err)
				}
			}
			if r.Err() != nil {
				t.Fatalf("stream %d torn=%v: reader error: %v", i, torn, r.Err())
			}
			if values == 0 {
				t.Fatalf("stream %d torn=%v: no values framed", i, torn)
			}
		}
	}
}

// TestValueCursorScalarsDifferential checks the typed scalar readers against
// the tape's, element by element and bit for bit, over the number corpora.
func TestValueCursorScalarsDifferential(t *testing.T) {
	docs := [][]byte{floatArrayJSON(64), sciFloatArrayJSON(64), intArrayJSON(64)}
	for d, doc := range docs {
		r := NewReader(bytes.NewReader(doc))
		if !r.Next() {
			t.Fatalf("doc %d: %v", d, r.Err())
		}
		v, err := Parse(r.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		elems, _ := v.Array()

		c := r.Cursor()
		if err := c.BeginArray(); err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			ok, err := c.NextElement()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				if i != len(elems) {
					t.Fatalf("doc %d: cursor saw %d elements, tape %d", d, i, len(elems))
				}
				break
			}
			text, _ := elems[i].NumberText()
			wantF, _ := elems[i].Float64()
			gotF, err := c.Float64()
			if err != nil {
				t.Fatalf("doc %d elem %d (%s): Float64: %v", d, i, text, err)
			}
			if gotF != wantF {
				t.Fatalf("doc %d elem %d (%s): cursor Float64 %v, tape %v", d, i, text, gotF, wantF)
			}
		}
		if err := c.Finish(); err != nil {
			t.Fatal(err)
		}

		// Second pass: integer readers over the integer corpus.
		if d != 2 {
			continue
		}
		r = NewReader(bytes.NewReader(doc))
		r.Next()
		c = r.Cursor()
		c.BeginArray()
		for i := 0; ; i++ {
			ok, err := c.NextElement()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			text, _ := elems[i].NumberText()
			want, wantOK := elems[i].Int64()
			if !wantOK {
				t.Fatalf("elem %d (%s): tape Int64 failed", i, text)
			}
			if want >= 0 {
				got, err := c.Uint64()
				if err != nil || got != uint64(want) {
					t.Fatalf("elem %d (%s): cursor Uint64 %d err %v, want %d", i, text, got, err, want)
				}
			} else {
				got, err := c.Int64()
				if err != nil || got != want {
					t.Fatalf("elem %d (%s): cursor Int64 %d err %v, want %d", i, text, got, err, want)
				}
			}
		}
	}
}

// TestValueCursorMisuse pins the error behavior of wrong-kind reads and
// mis-driven walks.
func TestValueCursorMisuse(t *testing.T) {
	cursorAt := func(doc string) ValueCursor {
		r := NewReader(strings.NewReader(doc))
		if !r.Next() {
			t.Fatalf("Next failed for %q: %v", doc, r.Err())
		}
		return r.Cursor()
	}

	empty := (&Reader{}).Cursor()
	if empty.Kind() != Invalid {
		t.Fatalf("empty cursor Kind = %v, want Invalid", empty.Kind())
	}
	if _, err := empty.Bool(); err == nil {
		t.Fatal("empty cursor Bool succeeded")
	}
	if err := empty.BeginObject(); err == nil {
		t.Fatal("empty cursor BeginObject succeeded")
	}
	if err := empty.Skip(); err == nil {
		t.Fatal("empty cursor Skip succeeded")
	}

	c := cursorAt(`"text"`)
	if _, err := c.Int64(); err == nil {
		t.Fatal("Int64 on a string succeeded")
	}
	if _, err := c.Float64(); err == nil {
		t.Fatal("Float64 on a string succeeded")
	}
	if _, err := c.Bool(); err == nil {
		t.Fatal("Bool on a string succeeded")
	}
	if c.Null() {
		t.Fatal("Null on a string reported true")
	}
	if s, err := c.Text(); err != nil || s != "text" {
		t.Fatalf("Text after failed reads = %q, %v", s, err)
	}

	c = cursorAt(`null`)
	if _, err := c.Text(); err == nil {
		t.Fatal("Text on null succeeded")
	}
	if _, err := c.NumberText(); err == nil {
		t.Fatal("NumberText on null succeeded")
	}
	if !c.Null() {
		t.Fatal("Null on null reported false")
	}
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}

	c = cursorAt(`{"a":1}`)
	if _, _, err := c.NextField(); err == nil {
		t.Fatal("NextField before BeginObject succeeded")
	}
	c = cursorAt(`{"a":1}`)
	if err := c.BeginArray(); err == nil {
		t.Fatal("BeginArray on an object succeeded")
	}
	c = cursorAt(`{"a":1}`)
	if err := c.Finish(); err == nil {
		t.Fatal("Finish before consuming succeeded")
	}
	c = cursorAt(`-5`)
	if _, err := c.Uint64(); err == nil {
		t.Fatal("Uint64 on a negative number succeeded")
	}
	c = cursorAt(`3.5`)
	if _, err := c.Int64(); err == nil {
		t.Fatal("Int64 on a fraction succeeded")
	}
}

// TestValueCursorSteadyStateAllocs requires the cursor walk to add nothing to
// the reader's own steady-state allocations on escape-free records.
func TestValueCursorSteadyStateAllocs(t *testing.T) {
	data := eventStreamNDJSON(400)
	var sink walkSums
	allocs := testing.AllocsPerRun(20, func() {
		r := NewReaderSize(bytes.NewReader(data), 64<<10)
		for r.Next() {
			c := r.Cursor()
			if err := cursorWalkValue(&c, &sink); err != nil {
				t.Fatal(err)
			}
		}
		if r.Err() != nil {
			t.Fatal(r.Err())
		}
	})
	// One reader buffer plus bookkeeping per run; nothing per value.
	if allocs > 4 {
		t.Fatalf("allocations per full stream = %v, want a constant few", allocs)
	}
}

// checkValueCursorDifferential holds the cursor to Parse's answers for inputs
// within the cursor campaign's work budget. Larger inputs and streams with
// more than 256 values remain the stream oracle's responsibility.
func checkValueCursorDifferential(t *testing.T, data []byte, seed uint64) {
	t.Helper()
	if len(data) > 4<<10 {
		return
	}
	var whole []any
	r := NewReaderSize(bytes.NewReader(data), 64)
	for r.Next() {
		if len(whole) == 256 {
			return
		}
		c := r.Cursor()
		got, err := cursorToAny(&c)
		if err != nil {
			t.Fatalf("cursor rejects a value Next accepted: %v (value %.120q)", err, r.Bytes())
		}
		if err := c.Finish(); err != nil {
			t.Fatalf("Finish after full walk: %v (value %.120q)", err, r.Bytes())
		}
		v, perr := Parse(r.Bytes())
		if perr != nil {
			t.Fatalf("Parse rejects a value Next accepted: %v", perr)
		}
		want := v.Any()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cursor sees %#v, Parse sees %#v (value %.120q)", got, want, r.Bytes())
		}
		skip := r.Cursor()
		if err := skip.Skip(); err != nil {
			t.Fatalf("Skip: %v (value %.120q)", err, r.Bytes())
		}
		if err := skip.Finish(); err != nil {
			t.Fatalf("Finish after Skip: %v (value %.120q)", err, r.Bytes())
		}
		whole = append(whole, want)
	}
	wholeErr := r.Err()

	torn := NewReaderSize(&tornReader{data: data, state: seed | 1}, 64)
	i := 0
	for torn.Next() {
		c := torn.Cursor()
		got, err := cursorToAny(&c)
		if err != nil {
			t.Fatalf("torn cursor walk: %v", err)
		}
		if i >= len(whole) || !reflect.DeepEqual(got, whole[i]) {
			t.Fatalf("value %d depends on framing", i)
		}
		i++
	}
	if (wholeErr == nil) != (torn.Err() == nil) || i != len(whole) {
		t.Fatalf("stream outcome depends on framing: whole %d values err %v, torn %d values err %v",
			len(whole), wholeErr, i, torn.Err())
	}
}

// walkSums is the order-sensitive digest both walkers compute; identical
// traversal and identical scalar kernels must produce identical digests.
type walkSums struct {
	numbers  float64
	strBytes int
	trues    int
	nulls    int
	fields   int
	elems    int
}

// cursorWalkValue consumes one value through the cursor, reading every scalar
// once in document order.
func cursorWalkValue(c *ValueCursor, s *walkSums) error {
	switch c.Kind() {
	case Object:
		if err := c.BeginObject(); err != nil {
			return err
		}
		for {
			key, ok, err := c.NextField()
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			s.fields++
			s.strBytes += len(key)
			if err := cursorWalkValue(c, s); err != nil {
				return err
			}
		}
	case Array:
		if err := c.BeginArray(); err != nil {
			return err
		}
		for {
			ok, err := c.NextElement()
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			s.elems++
			if err := cursorWalkValue(c, s); err != nil {
				return err
			}
		}
	case String:
		text, err := c.Text()
		if err != nil {
			return err
		}
		s.strBytes += len(text)
		return nil
	case Number:
		f, err := c.Float64()
		if err != nil {
			return err
		}
		s.numbers += f
		return nil
	case Bool:
		b, err := c.Bool()
		if err != nil {
			return err
		}
		if b {
			s.trues++
		}
		return nil
	case Null:
		if !c.Null() {
			return fmt.Errorf("null literal not consumed")
		}
		s.nulls++
		return nil
	default:
		return fmt.Errorf("no value at cursor")
	}
}

// nodeWalkValue is the tape-navigated twin of cursorWalkValue.
func nodeWalkValue(n Node, s *walkSums) error {
	switch n.Kind() {
	case Object:
		it, _ := n.ObjectIter()
		for {
			key, value, ok := it.Next()
			if !ok {
				return nil
			}
			s.fields++
			if kb, ok := key.StringBytes(); ok {
				s.strBytes += len(kb)
			} else {
				decoded, _ := key.AppendText(nil)
				s.strBytes += len(decoded)
			}
			if err := nodeWalkValue(value, s); err != nil {
				return err
			}
		}
	case Array:
		it, _ := n.ArrayIter()
		for {
			elem, ok := it.Next()
			if !ok {
				return nil
			}
			s.elems++
			if err := nodeWalkValue(elem, s); err != nil {
				return err
			}
		}
	case String:
		if b, ok := n.StringBytes(); ok {
			s.strBytes += len(b)
		} else {
			decoded, _ := n.AppendText(nil)
			s.strBytes += len(decoded)
		}
		return nil
	case Number:
		f, ok := n.Float64()
		if !ok {
			return fmt.Errorf("tape Float64 failed")
		}
		s.numbers += f
		return nil
	case Bool:
		b, _ := n.Bool()
		if b {
			s.trues++
		}
		return nil
	case Null:
		s.nulls++
		return nil
	default:
		return fmt.Errorf("invalid node")
	}
}

// TestValueCursorWalkMatchesNodeWalk pins the two benchmark walkers to each
// other on the benchmark corpora, so the benchmark's equality assertion is
// known to hold before any timing runs.
func TestValueCursorWalkMatchesNodeWalk(t *testing.T) {
	for _, corpus := range streamWalkCorpora() {
		var nodeSums walkSums
		r := NewReader(bytes.NewReader(corpus.data))
		for r.Next() {
			v, err := ParseOptions(r.Bytes(), Options{ZeroCopy: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := nodeWalkValue(v.Node(), &nodeSums); err != nil {
				t.Fatal(err)
			}
		}
		if r.Err() != nil {
			t.Fatal(r.Err())
		}

		var cursorSums walkSums
		r = NewReader(bytes.NewReader(corpus.data))
		for r.Next() {
			c := r.Cursor()
			if err := cursorWalkValue(&c, &cursorSums); err != nil {
				t.Fatal(err)
			}
		}
		if r.Err() != nil {
			t.Fatal(r.Err())
		}
		if cursorSums != nodeSums {
			t.Fatalf("%s: cursor %+v, tape %+v", corpus.name, cursorSums, nodeSums)
		}
	}
}

// Benchmark corpora: NDJSON with corpus-shaped records.

// eventStreamNDJSON mirrors citm_catalog's event records one per line:
// integer ids and timestamps, a price, a short name, a flag, and a small
// integer array.
func eventStreamNDJSON(lines int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, lines*128)
	for i := 0; i < lines; i++ {
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendInt(dst, 100_000_000+int64(rng.Intn(900_000_000)), 10)
		dst = append(dst, `,"start":`...)
		dst = strconv.AppendInt(dst, 1_500_000_000_000+int64(rng.Intn(100_000_000)), 10)
		dst = append(dst, `,"price":`...)
		dst = strconv.AppendFloat(dst, float64(rng.Intn(50000))/100, 'f', 2, 64)
		dst = append(dst, `,"seats":`...)
		dst = strconv.AppendInt(dst, int64(rng.Intn(1000)), 10)
		dst = append(dst, `,"name":"event-`...)
		dst = strconv.AppendInt(dst, int64(i), 10)
		dst = append(dst, `","soldOut":`...)
		dst = strconv.AppendBool(dst, i&1 == 0)
		dst = append(dst, `,"sections":[`...)
		for s := 0; s < 4; s++ {
			if s != 0 {
				dst = append(dst, ',')
			}
			dst = strconv.AppendInt(dst, int64(rng.Intn(500)), 10)
		}
		dst = append(dst, "]}\n"...)
	}
	return dst
}

// fhirStreamNDJSON mirrors a FHIR Observation feed: nested objects dominated
// by short strings, one resource per line.
func fhirStreamNDJSON(lines int) []byte {
	rng := numberCorpusRand()
	codes := []struct{ code, display, unit string }{
		{"8867-4", "Heart rate", "beats/minute"},
		{"8480-6", "Systolic blood pressure", "mm[Hg]"},
		{"9279-1", "Respiratory rate", "breaths/minute"},
		{"8310-5", "Body temperature", "Cel"},
	}
	dst := make([]byte, 0, lines*384)
	for i := 0; i < lines; i++ {
		c := codes[i%len(codes)]
		dst = append(dst, `{"resourceType":"Observation","id":"obs-`...)
		dst = strconv.AppendInt(dst, int64(100000+i), 10)
		dst = append(dst, `","status":"final","category":"vital-signs","code":{"coding":[{"system":"http://loinc.org","code":"`...)
		dst = append(dst, c.code...)
		dst = append(dst, `","display":"`...)
		dst = append(dst, c.display...)
		dst = append(dst, `"}],"text":"`...)
		dst = append(dst, c.display...)
		dst = append(dst, `"},"subject":{"reference":"Patient/`...)
		dst = strconv.AppendInt(dst, int64(rng.Intn(1_000_000)), 10)
		dst = append(dst, `"},"effectiveDateTime":"2026-03-14T09:`...)
		dst = append(dst, byte('0'+rng.Intn(6)), byte('0'+rng.Intn(10)))
		dst = append(dst, `:53Z","valueQuantity":{"value":`...)
		dst = strconv.AppendFloat(dst, float64(rng.Intn(20000))/100, 'f', 2, 64)
		dst = append(dst, `,"unit":"`...)
		dst = append(dst, c.unit...)
		dst = append(dst, `","system":"http://unitsofmeasure.org"}}`...)
		dst = append(dst, '\n')
	}
	return dst
}

// pointStreamNDJSON mirrors a GeoJSON feature feed: long-mantissa coordinate
// floats inside small structural objects, one feature per line.
func pointStreamNDJSON(lines int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, lines*176)
	for i := 0; i < lines; i++ {
		dst = append(dst, `{"type":"Feature","id":`...)
		dst = strconv.AppendInt(dst, int64(i), 10)
		dst = append(dst, `,"geometry":{"type":"Point","coordinates":[`...)
		dst = fmtLongFloat(dst, rng, -180, 180)
		dst = append(dst, ',')
		dst = fmtLongFloat(dst, rng, -90, 90)
		dst = append(dst, `]},"properties":{"speed":`...)
		dst = strconv.AppendFloat(dst, float64(rng.Intn(4000))/100, 'f', 2, 64)
		dst = append(dst, `,"heading":`...)
		dst = strconv.AppendFloat(dst, float64(rng.Intn(36000))/100, 'f', 2, 64)
		dst = append(dst, `,"accuracy":`...)
		dst = fmtLongFloat(dst, rng, 0, 30)
		dst = append(dst, "}}\n"...)
	}
	return dst
}

type streamWalkCorpus struct {
	name string
	data []byte
}

func streamWalkCorpora() []streamWalkCorpus {
	return []streamWalkCorpus{
		{"events", eventStreamNDJSON(512)},
		{"fhir", fhirStreamNDJSON(256)},
		{"points", pointStreamNDJSON(512)},
	}
}

// BenchmarkStreamDynamicWalk measures single-pass dynamic consumption of an
// NDJSON stream: every field of every value is read once, in order.
//
//	Cursor            Reader.Next + forward cursor (this file)
//	ParseTape         Reader.Next + Parse + tape walk (owned copy)
//	ParseTapeZeroCopy Reader.Next + ParseOptions(ZeroCopy) + tape walk
//	DecodeAnyMap      DecodeNext into map[string]any
func BenchmarkStreamDynamicWalk(b *testing.B) {
	for _, corpus := range streamWalkCorpora() {
		var want walkSums
		{
			r := NewReader(bytes.NewReader(corpus.data))
			for r.Next() {
				c := r.Cursor()
				if err := cursorWalkValue(&c, &want); err != nil {
					b.Fatal(err)
				}
			}
			if r.Err() != nil {
				b.Fatal(r.Err())
			}
		}

		b.Run(corpus.name+"/Cursor", func(b *testing.B) {
			b.SetBytes(int64(len(corpus.data)))
			b.ReportAllocs()
			for range b.N {
				var s walkSums
				r := NewReader(bytes.NewReader(corpus.data))
				for r.Next() {
					c := r.Cursor()
					if err := cursorWalkValue(&c, &s); err != nil {
						b.Fatal(err)
					}
				}
				if r.Err() != nil {
					b.Fatal(r.Err())
				}
				if s != want {
					b.Fatalf("walk digest mismatch: %+v vs %+v", s, want)
				}
			}
		})

		b.Run(corpus.name+"/ParseTape", func(b *testing.B) {
			b.SetBytes(int64(len(corpus.data)))
			b.ReportAllocs()
			for range b.N {
				var s walkSums
				r := NewReader(bytes.NewReader(corpus.data))
				for r.Next() {
					v, err := Parse(r.Bytes())
					if err != nil {
						b.Fatal(err)
					}
					if err := nodeWalkValue(v.Node(), &s); err != nil {
						b.Fatal(err)
					}
				}
				if r.Err() != nil {
					b.Fatal(r.Err())
				}
				if s != want {
					b.Fatalf("walk digest mismatch: %+v vs %+v", s, want)
				}
			}
		})

		b.Run(corpus.name+"/ParseTapeZeroCopy", func(b *testing.B) {
			b.SetBytes(int64(len(corpus.data)))
			b.ReportAllocs()
			for range b.N {
				var s walkSums
				r := NewReader(bytes.NewReader(corpus.data))
				for r.Next() {
					v, err := ParseOptions(r.Bytes(), Options{ZeroCopy: true})
					if err != nil {
						b.Fatal(err)
					}
					if err := nodeWalkValue(v.Node(), &s); err != nil {
						b.Fatal(err)
					}
				}
				if r.Err() != nil {
					b.Fatal(r.Err())
				}
				if s != want {
					b.Fatalf("walk digest mismatch: %+v vs %+v", s, want)
				}
			}
		})

		b.Run(corpus.name+"/DecodeAnyMap", func(b *testing.B) {
			dec, err := CompileDecoder[map[string]any](DecoderOptions{ZeroCopy: true})
			if err != nil {
				b.Skipf("map[string]any decoder unavailable: %v", err)
			}
			b.SetBytes(int64(len(corpus.data)))
			b.ReportAllocs()
			sink := 0
			for range b.N {
				r := NewReader(bytes.NewReader(corpus.data))
				var m map[string]any
				for DecodeNext(r, dec, &m) {
					sink += len(m)
				}
				if r.Err() != nil {
					b.Fatal(r.Err())
				}
			}
			if sink == 0 {
				b.Fatal("no fields decoded")
			}
		})
	}
}

// BenchmarkStreamPartialRead measures the sparse-consumer case: two fields
// used per record, everything else skipped.
func BenchmarkStreamPartialRead(b *testing.B) {
	data := eventStreamNDJSON(512)

	var wantIDs int64
	var wantPrice float64
	{
		r := NewReader(bytes.NewReader(data))
		for r.Next() {
			v, err := ParseOptions(r.Bytes(), Options{ZeroCopy: true})
			if err != nil {
				b.Fatal(err)
			}
			if id, ok := v.Get("id"); ok {
				n, _ := id.Int64()
				wantIDs += n
			}
			if price, ok := v.Get("price"); ok {
				f, _ := price.Float64()
				wantPrice += f
			}
		}
	}

	b.Run("Cursor", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for range b.N {
			var ids int64
			var price float64
			r := NewReader(bytes.NewReader(data))
			for r.Next() {
				c := r.Cursor()
				if err := c.BeginObject(); err != nil {
					b.Fatal(err)
				}
				for {
					key, ok, err := c.NextField()
					if err != nil {
						b.Fatal(err)
					}
					if !ok {
						break
					}
					switch key {
					case "id":
						n, err := c.Int64()
						if err != nil {
							b.Fatal(err)
						}
						ids += n
					case "price":
						f, err := c.Float64()
						if err != nil {
							b.Fatal(err)
						}
						price += f
					default:
						if err := c.Skip(); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
			if r.Err() != nil {
				b.Fatal(r.Err())
			}
			if ids != wantIDs || price != wantPrice {
				b.Fatalf("digest mismatch: %d/%v vs %d/%v", ids, price, wantIDs, wantPrice)
			}
		}
	})

	b.Run("ParseTapeZeroCopy", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for range b.N {
			var ids int64
			var price float64
			r := NewReader(bytes.NewReader(data))
			for r.Next() {
				v, err := ParseOptions(r.Bytes(), Options{ZeroCopy: true})
				if err != nil {
					b.Fatal(err)
				}
				if id, ok := v.Get("id"); ok {
					n, _ := id.Int64()
					ids += n
				}
				if p, ok := v.Get("price"); ok {
					f, _ := p.Float64()
					price += f
				}
			}
			if r.Err() != nil {
				b.Fatal(r.Err())
			}
			if ids != wantIDs || price != wantPrice {
				b.Fatalf("digest mismatch: %d/%v vs %d/%v", ids, price, wantIDs, wantPrice)
			}
		}
	})
}
