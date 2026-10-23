package vibejson

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestEncodeErrorPathMatchesPrependRules(t *testing.T) {
	fields := []string{"", "x", "[key]", "a.b", "日本語", strings.Repeat("long", 90)}
	for _, start := range fields {
		t.Run(fmt.Sprintf("start=%q", start), func(t *testing.T) {
			cause := &EncodeError{Path: start, Reason: "failure"}
			var err error = cause
			want := start
			for i := 0; i < 400; i++ {
				segment := fields[i%len(fields)]
				if i%3 == 0 {
					segment = "[" + strconv.Itoa(i) + "]"
					err = prependEncodePathIndex(err, i)
				} else {
					err = prependEncodePathField(err, segment)
				}
				if want != "" && want[0] != '[' {
					want = segment + "." + want
				} else {
					want = segment + want
				}
				if i%97 == 0 {
					// Formatting during unwind must not disturb later prepends.
					expected := (&EncodeError{Path: want, Reason: cause.Reason}).Error()
					if got := err.Error(); got != expected {
						t.Fatalf("step %d: formatted error differs", i)
					}
				}
			}
			if finished := finishEncodeError(err); finished != cause || cause.Path != want {
				t.Fatalf("final error = %T; path differs = %v", finished, cause.Path != want)
			}
		})
	}
}

type errorPathChain struct {
	Next  *errorPathChain `json:"next,omitempty"`
	Value float64         `json:"value,omitempty"`
}

func makeErrorPathChain(depth int) errorPathChain {
	var value errorPathChain
	tail := &value
	for i := 1; i < depth; i++ {
		tail.Next = &errorPathChain{}
		tail = tail.Next
	}
	tail.Value = math.NaN()
	return value
}

func TestEncoderDeepErrorPath(t *testing.T) {
	value := makeErrorPathChain(DefaultMaxDepth)
	encoder, err := CompileEncoder[errorPathChain](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := encoder.AppendJSON([]byte("prefix:"), &value)
	failure, ok := err.(*EncodeError)
	if !ok {
		t.Fatalf("error = %T; want directly assertable *EncodeError", err)
	}
	want := strings.Repeat("next.", DefaultMaxDepth-1) + "value"
	if failure.Path != want || !strings.Contains(failure.Reason, "NaN") || string(got) != "prefix:" {
		t.Fatalf("incorrect error path, reason, or destination (path length=%d, reason=%q)", len(failure.Path), failure.Reason)
	}
}

func TestEncoderDeepNumericMapErrorPathOwnership(t *testing.T) {
	value := makeErrorPathChain(100)
	bad := map[int]errorPathChain{1234567: value}
	encoder, err := CompileEncoder[map[int]errorPathChain](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = encoder.AppendJSON(nil, &bad)
	failure, ok := err.(*EncodeError)
	if !ok {
		t.Fatalf("error = %T; want *EncodeError", err)
	}
	want := "1234567." + strings.Repeat("next.", 99) + "value"
	good := map[int]errorPathChain{7654321: {Value: 1}}
	for range 20 {
		if _, err := encoder.AppendJSON(nil, &good); err != nil {
			t.Fatal(err)
		}
	}
	if failure.Path != want {
		t.Fatal("numeric-map error path changed after scratch reuse")
	}
}

func BenchmarkEncodeErrorPath(b *testing.B) {
	for _, depth := range []int{1, 8, 128, 1024, DefaultMaxDepth} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			value := makeErrorPathChain(depth)
			encoder, err := CompileEncoder[errorPathChain](EncoderOptions{})
			if err != nil {
				b.Fatal(err)
			}
			buffer := make([]byte, 0, depth*16)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := encoder.AppendJSON(buffer[:0], &value); err == nil {
					b.Fatal("missing NaN error")
				}
			}
		})
	}
}
