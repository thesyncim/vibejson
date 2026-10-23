package vibejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEncoderStringPreservesUnusedCapacity(t *testing.T) {
	values := []string{"a\"b", "a\\b", "\x00", "é", "<>&", "\xff"}
	for n := 0; n <= 17; n++ {
		values = append(values, "abcdefghijklmnopq"[:n])
	}
	for _, escapeHTML := range []bool{true, false} {
		encoder, err := CompileEncoder[string](EncoderOptions{DisableHTMLEscaping: !escapeHTML})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			var expected bytes.Buffer
			std := json.NewEncoder(&expected)
			std.SetEscapeHTML(escapeHTML)
			if err := std.Encode(value); err != nil {
				t.Fatal(err)
			}
			encoded := bytes.TrimSuffix(expected.Bytes(), []byte{'\n'})
			for _, prefix := range []string{"", "pre:"} {
				for _, extra := range []int{-1, 0, 1, 8, 32} {
					t.Run(fmt.Sprintf("html=%v/value=%q/prefix=%d/extra=%d", escapeHTML, value, len(prefix), extra), func(t *testing.T) {
						capacity := len(prefix) + len(encoded) + extra
						storage := bytes.Repeat([]byte{0xa5}, capacity+16)
						copy(storage, prefix)
						got, err := encoder.AppendJSON(storage[:len(prefix):capacity], &value)
						want := append([]byte(prefix), encoded...)
						if err != nil || !bytes.Equal(got, want) {
							t.Fatalf("AppendJSON = %q, %v; want %q", got, err, want)
						}
						tail := capacity
						if &got[0] == &storage[0] {
							tail = len(got)
						}
						for i, b := range storage[tail:] {
							if b != 0xa5 {
								t.Fatalf("wrote beyond result or destination capacity at byte %d", tail+i)
							}
						}
					})
				}
			}
		}
	}
}

func BenchmarkEncodeShortString(b *testing.B) {
	encoder, err := CompileEncoder[string](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{1, 3, 7, 8, 15} {
		for _, extra := range []int{0, 32} {
			b.Run(fmt.Sprintf("length=%d/spare=%d", n, extra), func(b *testing.B) {
				value := strings.Repeat("a", n)
				dst := make([]byte, 0, n+2+extra)
				b.ReportAllocs()
				for b.Loop() {
					dst, err = encoder.AppendJSON(dst[:0], &value)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
