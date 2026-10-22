package vibejson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func benchmarkJSONMarshaler[T any](b *testing.B, value *T) {
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	want, err := json.Marshal(value)
	if err != nil {
		b.Fatal(err)
	}
	buffer, err := encoder.AppendJSON(make([]byte, 0, len(want)), value)
	if err != nil || !bytes.Equal(buffer, want) {
		b.Fatalf("encode = %s, %v; want %s", buffer, err, want)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buffer, err = encoder.AppendJSON(buffer[:0], value)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeJSONMarshaler(b *testing.B) {
	// RawMessage implements MarshalJSON without allocating its result, so
	// these measurements expose validation/compaction and depth-check costs.
	for _, payload := range []struct {
		name string
		json string
	}{
		{"small_compact", `{"value":[1,true,"clean"]}`},
		{"small_whitespace", " { \"value\" : [ 1, true, \"clean\" ] }\n"},
		{"large_compact", `{"payload":"` + strings.Repeat("x", 32<<10) + `"}`},
	} {
		b.Run(payload.name, func(b *testing.B) {
			b.Run("root", func(b *testing.B) {
				value := json.RawMessage(payload.json)
				benchmarkJSONMarshaler(b, &value)
			})
			b.Run("nested", func(b *testing.B) {
				value := struct {
					Raw json.RawMessage `json:"raw"`
				}{Raw: json.RawMessage(payload.json)}
				benchmarkJSONMarshaler(b, &value)
			})
		})
	}
}
