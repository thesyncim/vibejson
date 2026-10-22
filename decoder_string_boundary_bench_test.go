package vibejson

import (
	"encoding/json"
	"strings"
	"testing"
)

func BenchmarkDecodeStringBoundary(b *testing.B) {
	longPrefix := strings.Repeat("a", 4096)
	cases := []struct {
		name string
		src  []byte
	}{
		{"ascii_short", []byte(`"hello world"`)},
		{"unicode_short", []byte(`"héllo β世界"`)},
		{"ascii_long", []byte(`"` + longPrefix + `"`)},
		{"unicode_late", []byte(`"` + longPrefix + `β"`)},
		{"escape_late", []byte(`"` + longPrefix + `\n"`)},
		{"unicode_escape", []byte(`"β` + longPrefix + `\n"`)},
	}
	for _, mode := range []string{"owned_reused", "owned_fresh", "zero_copy"} {
		b.Run(mode, func(b *testing.B) {
			decoder := mustCompileTestDecoder[string](b, DecoderOptions{ZeroCopy: mode == "zero_copy"})
			for _, test := range cases {
				b.Run(test.name, func(b *testing.B) {
					var dst, want string
					if err := json.Unmarshal(test.src, &want); err != nil {
						b.Fatal(err)
					}
					if err := decoder.Decode(test.src, &dst); err != nil || dst != want {
						b.Fatalf("Decode = %q, %v; want %q", dst, err, want)
					}
					b.ReportAllocs()
					b.SetBytes(int64(len(test.src)))
					b.ResetTimer()
					for b.Loop() {
						if mode == "owned_fresh" {
							dst = ""
						}
						if err := decoder.Decode(test.src, &dst); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
