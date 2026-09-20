package vibejson

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"
)

func TestNumberRejectionParity(t *testing.T) {
	r := rand.New(rand.NewSource(0xBADF00D))
	alphabet := []byte("0123456789+-.eE ")
	for i := 0; i < testIterations(400_000, 4_000); i++ {
		n := 1 + r.Intn(12)
		b := make([]byte, n)
		for j := range b {
			b[j] = alphabet[r.Intn(len(alphabet))]
		}
		s := string(b)

		var wf float64
		wErr := json.Unmarshal(b, &wf)
		var gf float64
		gErr := Unmarshal(b, &gf)
		if (wErr == nil) != (gErr == nil) {
			t.Fatalf("float64 accept parity %q: stdlib err=%v ours err=%v", s, wErr, gErr)
		}
		if wErr == nil && gf != wf && !(math.IsNaN(gf) && math.IsNaN(wf)) {
			t.Fatalf("float64 value %q: stdlib=%v ours=%v", s, wf, gf)
		}

		var wi, gi int64
		wiErr := json.Unmarshal(b, &wi)
		giErr := Unmarshal(b, &gi)
		if (wiErr == nil) != (giErr == nil) {
			t.Fatalf("int64 accept parity %q: stdlib err=%v ours err=%v", s, wiErr, giErr)
		}
		if wiErr == nil && gi != wi {
			t.Fatalf("int64 value %q: stdlib=%v ours=%v", s, wi, gi)
		}

		var wu, gu uint64
		wuErr := json.Unmarshal(b, &wu)
		guErr := Unmarshal(b, &gu)
		if (wuErr == nil) != (guErr == nil) {
			t.Fatalf("uint64 accept parity %q: stdlib err=%v ours err=%v", s, wuErr, guErr)
		}
		if wuErr == nil && gu != wu {
			t.Fatalf("uint64 value %q: stdlib=%v ours=%v", s, wu, gu)
		}

		var wa any
		waErr := json.Unmarshal(b, &wa)
		_, gaErr := unmarshalAnyForTest(b)
		if (waErr == nil) != (gaErr == nil) {
			t.Fatalf("Unmarshal any accept parity %q: stdlib err=%v ours err=%v", s, waErr, gaErr)
		}
	}
}
