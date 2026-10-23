package vibejson

import (
	"strconv"
	"testing"
)

type inlineScratchPointers struct {
	Extra map[string]*uint64 `json:",inline"`
}

func inlineScratchPointerMap(size int) inlineScratchPointers {
	value := inlineScratchPointers{Extra: make(map[string]*uint64, size)}
	for i := range size {
		number := uint64(i)
		value.Extra["field_"+strconv.Itoa(i)] = &number
	}
	return value
}

func BenchmarkInlineEncodeScratch(b *testing.B) {
	for _, scenario := range []struct {
		name string
		warm int
		size int
	}{
		{"small", 1, 1},
		{"small_after_large", 8192, 1},
		{"large", 8192, 8192},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			encoder := mustInlineEncoder[inlineScratchPointers](b, EncoderOptions{InlineFields: true})
			if encoder.root.inlineMap.encScratchLimit < scenario.warm {
				b.Fatal("warm map exceeds retained scratch limit")
			}
			warm := inlineScratchPointerMap(scenario.warm)
			value := inlineScratchPointerMap(scenario.size)
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
		})
	}
}
