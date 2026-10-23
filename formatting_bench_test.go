package vibejson

import (
	"strings"
	"testing"
)

func BenchmarkAppendIndent(b *testing.B) {
	for _, document := range []struct {
		name string
		src  string
	}{
		{"flat", `{"a":1,"b":true,"c":"text"}`},
		{"depth128", strings.Repeat("[", 128) + "0" + strings.Repeat("]", 128)},
		{"depth1024", strings.Repeat("[", 1024) + "0" + strings.Repeat("]", 1024)},
	} {
		for _, format := range []struct {
			name   string
			indent string
		}{
			{"empty", ""},
			{"two-spaces", "  "},
		} {
			b.Run(document.name+"/"+format.name, func(b *testing.B) {
				src := []byte(document.src)
				dst, err := AppendIndent(nil, src, "", format.indent)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(src)))
				b.ResetTimer()
				for b.Loop() {
					dst, err = AppendIndent(dst[:0], src, "", format.indent)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
