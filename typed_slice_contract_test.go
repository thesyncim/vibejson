package simdjson

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// TestFusedInt64Slice checks the fused []int64 decoder against
// encoding/json across adversarial delimiter, null, and whitespace framings.
func TestFusedInt64Slice(t *testing.T) {
	cases := []string{
		`[]`, `[ ]`, `[1]`, `[1,2,3]`, `[ 1 , 2 , 3 ]`,
		`[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]`, // grow past initial cap
		`[0,-0,-1,9223372036854775807,-9223372036854775808]`,
		`[1,null,2]`, `[null]`, `[null,null,null]`,
		"[1,\n2,\t3,\r4]", `[ 1,2 ,3, 4 ]`,
		`[1 , 2]`, "[1\t,\t2]",
	}
	for _, s := range cases {
		diffInt64Slice(t, s)
	}
}

func diffInt64Slice(t *testing.T, s string) {
	t.Helper()
	var want []int64
	wantErr := json.Unmarshal([]byte(s), &want)
	var got []int64
	gotErr := Unmarshal([]byte(s), &got)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("[]int64 %q: error mismatch: stdlib=%v ours=%v", s, wantErr, gotErr)
	}
	if wantErr == nil && !reflect.DeepEqual(want, got) {
		t.Fatalf("[]int64 %q: value mismatch: stdlib=%v ours=%v", s, want, got)
	}
}

func diffFloat64Slice(t *testing.T, s string) {
	t.Helper()
	var want []float64
	wantErr := json.Unmarshal([]byte(s), &want)
	var got []float64
	gotErr := Unmarshal([]byte(s), &got)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("[]float64 %q: error mismatch: stdlib=%v ours=%v", s, wantErr, gotErr)
	}
	if wantErr == nil && !reflect.DeepEqual(want, got) {
		t.Fatalf("[]float64 %q: value mismatch: stdlib=%v ours=%v", s, want, got)
	}
}

func diffUint64Slice(t *testing.T, s string) {
	t.Helper()
	var want []uint64
	wantErr := json.Unmarshal([]byte(s), &want)
	var got []uint64
	gotErr := Unmarshal([]byte(s), &got)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("[]uint64 %q: error mismatch: stdlib=%v ours=%v", s, wantErr, gotErr)
	}
	if wantErr == nil && !reflect.DeepEqual(want, got) {
		t.Fatalf("[]uint64 %q: value mismatch: stdlib=%v ours=%v", s, want, got)
	}
}

func testFusedLargeScalarSliceAllocs[T any](t *testing.T, src []byte) {
	t.Helper()
	decoder, err := CompileDecoder[[]T](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	var warm []T
	if err := decoder.Decode(src, &warm); err != nil {
		t.Fatal(err)
	}
	total := 0
	allocs := testing.AllocsPerRun(500, func() {
		var got []T
		if err := decoder.Decode(src, &got); err != nil {
			panic(err)
		}
		total += len(got)
	})
	if total == 0 {
		t.Fatal("decoded no scalar elements")
	}
	if allocs > 2 {
		t.Fatalf("fresh large scalar slice allocated %.1f times per decode, want <=2", allocs)
	}
}

func TestFusedLargeScalarSliceAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds bookkeeping allocations")
	}
	t.Run("int64", func(t *testing.T) {
		testFusedLargeScalarSliceAllocs[int64](t, intArrayJSON(8192))
	})
	t.Run("float64", func(t *testing.T) {
		testFusedLargeScalarSliceAllocs[float64](t, floatArrayJSON(8192))
	})
	t.Run("uint64", func(t *testing.T) {
		src := []byte(`[` + strings.Repeat("1,", 8191) + `1]`)
		testFusedLargeScalarSliceAllocs[uint64](t, src)
	})
}

func TestFusedNamedScalarSliceUsesGeneralGrowth(t *testing.T) {
	type scalar int64
	type scalars []scalar
	decoder, err := CompileDecoder[scalars](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got scalars
	if err := decoder.Decode([]byte(`[1,-2,null,4]`), &got); err != nil {
		t.Fatal(err)
	}
	want := scalars{1, -2, 0, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Decode() = %v, want %v", got, want)
	}
}

func TestFusedLargeScalarSliceDoesNotReserveForString(t *testing.T) {
	for _, src := range []string{
		`["` + strings.Repeat("value,", 1024) + `"]`,
		`[0,"` + strings.Repeat("value,", 1024) + `"]`,
	} {
		decoder, err := CompileDecoder[[]float64](DecoderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var got []float64
		if err := decoder.Decode([]byte(src), &got); err == nil {
			t.Fatal("string decoded into []float64")
		}
		if cap(got) > 4 {
			t.Fatalf("invalid string array reserved capacity %d, want <=4", cap(got))
		}
	}
}

func TestFusedLargeScalarSliceBoundsInvalidReservation(t *testing.T) {
	src := []byte(`[0,[` + strings.Repeat("0,", 4096) + `0]]`)
	decoder, err := CompileDecoder[[]float64](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	if err := decoder.Decode(src, &got); err == nil {
		t.Fatal("nested array decoded into []float64")
	}
	if limit := (len(src) + 1) / 2; cap(got) > limit {
		t.Fatalf("invalid nested array reserved capacity %d, want <=%d", cap(got), limit)
	}
}

// TestFusedSliceReuse decodes into a reused destination and checks the
// result equals a fresh decode: no stale elements survive from the prior value.
func TestFusedSliceReuse(t *testing.T) {
	dec, err := CompileDecoder[[]int64](DecoderOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	seqs := []string{
		`[1,2,3,4,5,6,7,8,9,10]`,
		`[11,12]`,
		`[]`,
		`[100]`,
		`[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20]`,
		`[]`,
		`[7,7,7]`,
	}
	var reused []int64
	for _, s := range seqs {
		if err := dec.Decode([]byte(s), &reused); err != nil {
			t.Fatalf("reused decode %q: %v", s, err)
		}
		var fresh []int64
		if err := Unmarshal([]byte(s), &fresh); err != nil {
			t.Fatalf("fresh decode %q: %v", s, err)
		}
		if !reflect.DeepEqual(reused, fresh) {
			t.Fatalf("reuse divergence for %q: reused=%v fresh=%v", s, reused, fresh)
		}
		// Compare against stdlib too.
		var std []int64
		if err := json.Unmarshal([]byte(s), &std); err != nil {
			t.Fatalf("stdlib %q: %v", s, err)
		}
		if !reflect.DeepEqual(reused, std) {
			t.Fatalf("stdlib divergence for %q: reused=%v std=%v", s, reused, std)
		}
	}
}

// TestFusedSliceFuzz random-differentials the three fused scalar slice
// decoders against encoding/json with adversarial spacing and delimiters.
func TestFusedSliceFuzz(t *testing.T) {
	r := rand.New(rand.NewSource(0x5CA1))
	spaces := []string{"", " ", "  ", "\t", "\n", "\r\n", " \t "}
	sp := func() string { return spaces[r.Intn(len(spaces))] }
	for i := 0; i < testIterations(300_000, 3_000); i++ {
		n := r.Intn(25)
		var buf []byte
		buf = append(buf, '[')
		buf = append(buf, sp()...)
		kind := r.Intn(3)
		for j := 0; j < n; j++ {
			if j > 0 {
				buf = append(buf, sp()...)
				buf = append(buf, ',')
				buf = append(buf, sp()...)
			}
			switch {
			case r.Intn(12) == 0:
				buf = append(buf, "null"...)
			case kind == 0:
				buf = append(buf, fmt.Sprintf("%d", int64(r.Uint64()))...)
			case kind == 1:
				buf = append(buf, fmt.Sprintf("%d", r.Uint64())...)
			default:
				buf = append(buf, fmt.Sprintf("%v", math1(r))...)
			}
		}
		buf = append(buf, sp()...)
		buf = append(buf, ']')
		s := string(buf)
		switch kind {
		case 0:
			diffInt64Slice(t, s)
		case 1:
			diffUint64Slice(t, s)
		default:
			diffFloat64Slice(t, s)
		}
	}
}

func math1(r *rand.Rand) float64 {
	return float64(int64(r.Uint64())) / float64(1+r.Intn(1000))
}
