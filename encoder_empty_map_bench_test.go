package vibejson

import (
	"strconv"
	"testing"
)

func emptyMapBenchValue(size int) map[string]int {
	if size < 0 {
		return nil
	}
	value := make(map[string]int, size)
	for i := range size {
		value["field_"+strconv.Itoa(i)] = i
	}
	return value
}

func benchmarkEmptyMapEncode[T any](b *testing.B, warm, value T) {
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	buffer, err := encoder.AppendJSON(nil, &warm)
	if err != nil {
		b.Fatal(err)
	}
	buffer, err = encoder.AppendJSON(buffer[:0], &value)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buffer, err = encoder.AppendJSON(buffer[:0], &value)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeEmptyMap(b *testing.B) {
	for _, scenario := range []struct {
		name string
		warm int
		size int
	}{
		{"nil", -1, -1},
		{"empty", 0, 0},
		{"one", 1, 1},
		{"eight", 8, 8},
		{"empty_after_large", 4096, 0},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			warm := emptyMapBenchValue(scenario.warm)
			value := emptyMapBenchValue(scenario.size)
			b.Run("typed", func(b *testing.B) {
				benchmarkEmptyMapEncode(b, warm, value)
			})
			b.Run("interface", func(b *testing.B) {
				benchmarkEmptyMapEncode[any](b, warm, value)
			})
		})
	}
}
