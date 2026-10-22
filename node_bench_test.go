package vibejson

import (
	"fmt"
	"testing"
)

// Measure the public lookup path across positions so early-exit changes also
// retain visibility into first-key and missing-key scan costs.
func BenchmarkNodeGetFlatPlain(b *testing.B) {
	for _, width := range []int{1, 8, 32, 128} {
		src := flatObjectDoc(width)
		for _, query := range []struct {
			name string
			key  string
			want bool
		}{
			{"first", "f0000", true},
			{"middle", fmt.Sprintf("f%04d", width/2), true},
			{"last", fmt.Sprintf("f%04d", width-1), true},
			{"miss", "x0000", false},
		} {
			b.Run(fmt.Sprintf("width=%d/%s", width, query.name), func(b *testing.B) {
				benchmarkIndexGet(b, src, query.key, query.want, false)
			})
		}
	}
}
