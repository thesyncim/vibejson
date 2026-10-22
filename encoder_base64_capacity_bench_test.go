package vibejson

import (
	"encoding/base64"
	"fmt"
	"testing"
)

func BenchmarkEncodeBase64Capacity(b *testing.B) {
	encoder, err := CompileEncoder[[]byte](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range []int{0, 1, 32, 1024} {
		value := base64CapacityPayload(size)
		for _, mode := range []string{"fresh", "reused", "exact", "tinyPrefix"} {
			b.Run(fmt.Sprintf("%d/%s", size, mode), func(b *testing.B) {
				var dst []byte
				storage := make([]byte, 0, base64.StdEncoding.EncodedLen(size)+2)
				if mode == "tinyPrefix" {
					storage = []byte("pre")
					storage = storage[:len(storage):len(storage)]
				}
				if mode == "reused" {
					var err error
					dst, err = encoder.AppendJSON(nil, &value)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for b.Loop() {
					switch mode {
					case "fresh":
						dst = nil
					case "reused":
						dst = dst[:0]
					case "exact", "tinyPrefix":
						dst = storage
					}
					var err error
					dst, err = encoder.AppendJSON(dst, &value)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
