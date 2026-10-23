package vibejson

import (
	"bytes"
	"encoding/json"
	"testing"
)

func benchmarkNestedStructEncoder[T any](b *testing.B, value *T) {
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

func BenchmarkEncodeStructFusion(b *testing.B) {
	b.Run("scalars", func(b *testing.B) {
		value := fusionOuter{
			First: fusionInner{A: 1, B: "one"},
			Mid: fusionMid{
				Name: "mid",
				In:   fusionInner{A: 2, B: "two"},
				Tail: fusionInner{A: 3, B: "three"},
			},
			N:    42,
			Last: fusionInner{A: 4, B: "four"},
		}
		benchmarkNestedStructEncoder(b, &value)
	})
	b.Run("slice", func(b *testing.B) {
		type inner struct {
			Values []int `json:"values"`
		}
		type outer struct {
			Inner inner `json:"inner"`
		}
		value := outer{Inner: inner{Values: []int{1, 2, 3, 4}}}
		benchmarkNestedStructEncoder(b, &value)
	})
	b.Run("pointer", func(b *testing.B) {
		type inner struct {
			Next *benchSmall `json:"next"`
		}
		type outer struct {
			Inner inner `json:"inner"`
		}
		value := outer{Inner: inner{Next: &benchSmall{ID: 1, OK: true, Name: "small"}}}
		benchmarkNestedStructEncoder(b, &value)
	})
}
