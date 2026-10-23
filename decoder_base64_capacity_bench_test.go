package vibejson

import (
	"encoding/json"
	"fmt"
	"testing"
)

func BenchmarkDecodeBase64Capacity(b *testing.B) {
	for _, size := range []int{32, 64, 96, 1024} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			src, err := json.Marshal(base64CapacityPayload(size))
			if err != nil {
				b.Fatal(err)
			}
			for _, mode := range []string{"exact", "warm", "roomy", "fresh"} {
				b.Run(mode, func(b *testing.B) {
					decoder := mustCompileTestDecoder[[]byte](b, DecoderOptions{})
					capacity := size
					if mode == "roomy" {
						capacity += 2
					}
					storage := make([]byte, size, capacity)
					dst := storage[:0]
					if err := decoder.Decode(src, &dst); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.SetBytes(int64(len(src)))
					b.ResetTimer()
					for b.Loop() {
						switch mode {
						case "exact":
							dst = storage[:0]
						case "fresh":
							dst = nil
						}
						if err := decoder.Decode(src, &dst); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
