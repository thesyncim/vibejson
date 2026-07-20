//go:build goexperiment.simd && arm64

package simdjson

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestStructuralFloatArray3MatchesStrconv(t *testing.T) {
	var texts []string
	for d0 := 0; d0 <= 9; d0++ {
		texts = append(texts, strconv.Itoa(d0), "-"+strconv.Itoa(d0))
		for d1 := 0; d1 <= 9; d1++ {
			texts = append(texts,
				strconv.Itoa(d0)+"."+strconv.Itoa(d1),
				"-"+strconv.Itoa(d0)+"."+strconv.Itoa(d1),
				strconv.Itoa(d0)+"e"+strconv.Itoa(d1),
				strconv.Itoa(d0)+"E-"+strconv.Itoa(d1),
				strconv.Itoa(d0)+"e+"+strconv.Itoa(d1),
			)
			for d2 := 0; d2 <= 9; d2++ {
				texts = append(texts,
					strconv.Itoa(d0)+"."+strconv.Itoa(d1)+strconv.Itoa(d2),
					"-"+strconv.Itoa(d0)+"."+strconv.Itoa(d1)+strconv.Itoa(d2),
				)
			}
		}
	}
	for value := 10; value <= 99; value++ {
		texts = append(texts, strconv.Itoa(value), "-"+strconv.Itoa(value))
	}
	type document struct {
		Values [3]float64 `json:"values"`
		Pad    string     `json:"pad"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 5000)
	for offset := 0; offset < len(texts); offset += 3 {
		parts := [3]string{"0", "0", "0"}
		var want [3]float64
		for index := range parts {
			if offset+index < len(texts) {
				parts[index] = texts[offset+index]
			}
			want[index], err = strconv.ParseFloat(parts[index], 64)
			if err != nil {
				t.Fatal(err)
			}
		}
		src := []byte("{\n\"values\":[" + parts[0] + "," + parts[1] + "," + parts[2] + "],\"pad\":\"" + pad + "\"\n}")
		if len(src) < 4096 || !decoderStructuralWorthwhile(src) {
			t.Fatal("float tuple did not select structural decoding")
		}
		var got document
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatalf("Decode(%q): %v", src, err)
		}
		for index := range want {
			if math.Float64bits(got.Values[index]) != math.Float64bits(want[index]) {
				t.Fatalf("Decode(%q)[%d] = %.17g (%#x), want %.17g (%#x)",
					src, index, got.Values[index], math.Float64bits(got.Values[index]),
					want[index], math.Float64bits(want[index]))
			}
		}
	}
}

func TestStructuralFloatArray3RejectsMalformed(t *testing.T) {
	type document struct {
		Values [3]float64 `json:"values"`
		Pad    string     `json:"pad"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 5000)
	for _, token := range []string{
		"1x", "1 x", "2.5x", "-3e4x", "01", "1.", "1e", "1e+", "1\v", "1\x00",
	} {
		src := []byte("{\n\"values\":[" + token + ",2.5,-3e4],\"pad\":\"" + pad + "\"\n}")
		var got document
		if err := decoder.Decode(src, &got); err == nil {
			t.Fatalf("Decode accepted malformed tuple number %q", token)
		}
	}
}

type structuralRecordInt struct {
	ID      int64      `json:"id"`
	Active  bool       `json:"active"`
	Name    string     `json:"name"`
	Message string     `json:"message"`
	Scores  [3]float64 `json:"scores"`
}

func structuralRecordIntJSON(token string) []byte {
	return []byte("{\n\"id\":" + token +
		",\"active\":true,\"name\":\"record\",\"message\":\"" + strings.Repeat("x", 5000) +
		"\",\"scores\":[1,2.5,-3e4]\n}")
}

func TestStructuralRecordIntegerFastPathAndFallback(t *testing.T) {
	decoder, err := CompileDecoder[structuralRecordInt](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if decoder.root.decShape != typedDecShapeRecord {
		t.Fatalf("record shape = %d, want retained record specialization", decoder.root.decShape)
	}
	if !decoder.structural {
		t.Fatal("record shape was not marked for structural decoding")
	}

	for _, want := range []int64{0, 9, 10, 99, 100, 999, 1000, 9999, -1, 10000, 1 << 40} {
		src := structuralRecordIntJSON(strconv.FormatInt(want, 10))
		if !decoderStructuralWorthwhile(src) {
			t.Fatalf("input for %d did not select structural decoding", want)
		}
		var got structuralRecordInt
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatalf("Decode(%d): %v", want, err)
		}
		if got.ID != want || !got.Active || got.Name != "record" || len(got.Message) != 5000 ||
			got.Scores != [3]float64{1, 2.5, -3e4} {
			t.Fatalf("Decode(%d) produced %+v", want, got)
		}
	}

	for _, token := range []string{"00", "01", "1x", "+1", ".1", "--1", "10000x", "9223372036854775808"} {
		var got structuralRecordInt
		if err := decoder.Decode(structuralRecordIntJSON(token), &got); err == nil {
			t.Fatalf("Decode accepted malformed or overflowing integer %q", token)
		}
	}

	malformedBool := []byte(strings.Replace(
		string(structuralRecordIntJSON("1")), `"active":true`, `"active":truex`, 1,
	))
	var got structuralRecordInt
	if err := decoder.Decode(malformedBool, &got); err == nil {
		t.Fatal("Decode accepted malformed boolean continuation")
	}
}

func TestStructuralDecodeMatchesRawCursor(t *testing.T) {
	compact := benchRecordsJSON(256)
	indented, err := Indent(compact, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !decoder.structural {
		t.Fatal("fully compiled document was not marked structural")
	}

	for _, test := range []struct {
		name string
		src  []byte
	}{
		{name: "compact", src: compact},
		{name: "indented", src: indented},
	} {
		t.Run(test.name, func(t *testing.T) {
			if len(test.src) < decoderStructuralMinBytes || !decoderStructuralWorthwhile(test.src) {
				t.Fatal("test document did not select the structural decoder")
			}
			var structural, raw benchDocument
			if err := decoder.Decode(test.src, &structural); err != nil {
				t.Fatalf("structural decode: %v", err)
			}
			consumed, err := decoder.DecodePrefix(test.src, &raw)
			if err != nil {
				t.Fatalf("raw cursor decode: %v", err)
			}
			if consumed != len(test.src) {
				t.Fatalf("raw cursor consumed %d of %d bytes", consumed, len(test.src))
			}
			if !reflect.DeepEqual(structural, raw) {
				t.Fatal("structural and raw cursor decodes differ")
			}
		})
	}
}

func TestStructuralDecodeRejectsHiddenScalarTails(t *testing.T) {
	type document struct {
		OK  bool   `json:"ok"`
		Pad string `json:"pad"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("a", 5000)
	for _, token := range []string{"truex", "falsex", "nullx"} {
		src := []byte("{\n  \"ok\": " + token + ",\n  \"pad\": \"" + pad + "\"\n}")
		if !decoderStructuralWorthwhile(src) {
			t.Fatalf("%s did not select the structural decoder", token)
		}
		var dst document
		if err := decoder.Decode(src, &dst); err == nil {
			t.Fatalf("Decode accepted malformed scalar %q", token)
		}
	}
}

func TestStructuralDecodeRejectsHiddenArrayScalarTails(t *testing.T) {
	type document struct {
		Values []bool `json:"values"`
		Pad    string `json:"pad"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte("{\n  \"values\": [truex, false],\n  \"pad\": \"" + strings.Repeat("a", 5000) + "\"\n}")
	var dst document
	if err := decoder.Decode(src, &dst); err == nil {
		t.Fatal("Decode accepted malformed scalar continuation in array")
	}
}

func TestStructuralDecodeCompactColonValidation(t *testing.T) {
	type document struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat(" ", 5000)
	valid := [][]byte{
		[]byte("{\n  \"id\" : 42,\n  \"name\"\t:\r\n\"ok\"\n}" + pad),
		[]byte("{\n\"id\":42,\n\"name\":\"ok\"\n}" + pad),
	}
	for _, src := range valid {
		var got document
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatalf("valid input rejected: %v", err)
		}
		if got != (document{ID: 42, Name: "ok"}) {
			t.Fatalf("unexpected result: %+v", got)
		}
	}

	invalid := [][]byte{
		[]byte("{\n  \"id\" 42,\n  \"name\": \"ok\"\n}" + pad),
		[]byte("{\n  \"id\":: 42,\n  \"name\": \"ok\"\n}" + pad),
		[]byte("{\n  \"id\" x: 42,\n  \"name\": \"ok\"\n}" + pad),
	}
	for _, src := range invalid {
		var got document
		if err := decoder.Decode(src, &got); err == nil {
			t.Fatalf("malformed colon gap accepted: %q", src[:48])
		}
	}
}

func TestStructuralDecodeCrossesWindows(t *testing.T) {
	type document struct {
		Text string `json:"text"`
		N    int    `json:"n"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("0123456789", 700) + `\nquoted: \"yes\"`
	src := []byte("{\n  \"text\": \"" + text + "\",\n  \"n\": 42\n}")
	var got document
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	want := document{Text: strings.Repeat("0123456789", 700) + "\nquoted: \"yes\"", N: 42}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded value differs: N=%d text length=%d", got.N, len(got.Text))
	}
}

func TestStructuralDecodeSynchronizesEmptyMap(t *testing.T) {
	type document struct {
		Values map[string]int `json:"values"`
		Empty  map[string]int `json:"empty"`
		Next   int            `json:"next"`
		Pad    string         `json:"pad"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if decoder.structural {
		t.Fatal("map-heavy document should retain the raw decoder route")
	}
	src := []byte("{\n  \"values\": {\"a\": 1},\n  \"empty\": {},\n  \"next\": 7,\n  \"pad\": \"" + strings.Repeat("x", 5000) + "\"\n}")
	if !decoderStructuralWorthwhile(src) {
		t.Fatal("test document did not select the structural decoder")
	}
	var got document
	if err := decoder.decodeStructural(src, &got); err != nil {
		t.Fatal(err)
	}
	if got.Next != 7 || len(got.Values) != 1 || len(got.Empty) != 0 || len(got.Pad) != 5000 {
		t.Fatalf("unexpected decode: next=%d values=%v empty=%v pad=%d", got.Next, got.Values, got.Empty, len(got.Pad))
	}
}
