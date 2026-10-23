package vibejson

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type decodeErrorPathChain struct {
	Next  *decodeErrorPathChain `json:"next"`
	Value int                   `json:"value"`
}

func decodeErrorPathInput(depth int) []byte {
	return []byte(strings.Repeat(`{"next":`, depth-1) + `{"value":true}` + strings.Repeat(`}`, depth-1))
}

func checkDeepDecodeError(t *testing.T, err error, path string, offset int) *DecodeError {
	t.Helper()
	failure, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("error = %T; want directly assertable *DecodeError", err)
	}
	if failure.Path != path || failure.Offset != offset || failure.Type != reflect.TypeFor[int]() || failure.Reason == "" {
		t.Fatalf("error = %+v; want path length %d, offset %d and int destination", failure, len(path), offset)
	}
	return failure
}

func TestDecoderDeepErrorPathPublicBoundaries(t *testing.T) {
	for _, depth := range []int{80, DefaultMaxDepth - 1} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			src := decodeErrorPathInput(depth)
			path := strings.Repeat("next.", depth-1) + "value"
			offset := bytes.Index(src, []byte("true"))
			decoder := mustCompileTestDecoder[decodeErrorPathChain](t, DecoderOptions{})
			var dst decodeErrorPathChain
			checkDeepDecodeError(t, decoder.Decode(src, &dst), path, offset)
			_, err := decoder.DecodePrefix(append(bytes.Clone(src), []byte(" 7")...), &dst)
			checkDeepDecodeError(t, err, path, offset)
			array := append(append([]byte{'['}, src...), ']')
			_, err = decoder.DecodeArray(array, nil)
			checkDeepDecodeError(t, err, "[0]."+path, offset+1)
			checkDeepDecodeError(t, Unmarshal(src, &dst), path, offset)
			reader := NewReader(bytes.NewReader(src))
			if DecodeNext(reader, decoder, &dst) {
				t.Fatal("reader accepted an incompatible value")
			}
			var streamed *DecodeError
			if !errors.As(reader.Err(), &streamed) || streamed.Path != path || streamed.Offset != offset {
				t.Fatalf("reader error = %v", reader.Err())
			}
		})
	}
}

func TestDecoderDeepErrorPathOwnsMapKeys(t *testing.T) {
	const depth = 100
	key := strings.Repeat("dynamic.key", 40)
	src := append([]byte(`{"`+key+`":`), decodeErrorPathInput(depth)...)
	src = append(src, '}')
	decoder := mustCompileTestDecoder[map[string]decodeErrorPathChain](t, DecoderOptions{ZeroCopy: true})
	var dst map[string]decodeErrorPathChain
	err := decoder.Decode(src, &dst)
	want := key + "." + strings.Repeat("next.", depth-1) + "value"
	failure := checkDeepDecodeError(t, err, want, bytes.Index(src, []byte("true")))
	for i := range src {
		src[i] = '!'
	}
	for range 3 {
		requireNoTestError(t, decoder.Decode([]byte(`{"other":{"value":1}}`), &dst))
	}
	if failure.Path != want {
		t.Fatal("error path changed after input and scratch reuse")
	}
}

func TestDecodeErrorPathMatchesPrependRules(t *testing.T) {
	fields := []string{"", "x", "[key]", "a.b", "日本語", strings.Repeat("long", 90)}
	for _, start := range fields {
		cause := &DecodeError{Path: start, Offset: 17, TypeName: "destination", Reason: "failure"}
		var err error = cause
		want := start
		for i := range 400 {
			segment := fields[i%len(fields)]
			if i%3 == 0 {
				segment = "[" + strconv.Itoa(i) + "]"
				err = prependDecodePathIndex(err, i)
			} else {
				err = prependDecodePathField(err, segment)
			}
			if want != "" && want[0] != '[' {
				want = segment + "." + want
			} else {
				want = segment + want
			}
			if i%97 == 0 {
				expected := *cause
				expected.Path = want
				if got := err.Error(); got != expected.Error() {
					t.Fatalf("step %d: formatted error differs", i)
				}
			}
		}
		if finished := finishDecodeError(err); finished != cause || cause.Path != want {
			t.Fatal("final error changed identity or path")
		}
	}
}

type decodePathNativeLeaf struct{ failure error }

func (v *decodePathNativeLeaf) UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error) {
	if err := c.Skip(); err != nil {
		return c, err
	}
	return c, v.failure
}

type decodePathNativeChain struct {
	Next  *decodePathNativeChain `json:"next"`
	Value decodePathNativeLeaf   `json:"value"`
}

func TestDecoderDeepNativeErrorPreservesIdentity(t *testing.T) {
	const depth = 80
	for _, dynamic := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			initialPath := strings.Repeat("inner.", 60) + "leaf"
			cause := &DecodeError{Offset: 17, Path: initialPath, TypeName: "nativeValue", Reason: "failure"}
			var failure error = cause
			if wrapped {
				failure = fmt.Errorf("hook context: %w", cause)
			}
			var dst decodePathNativeChain
			tail := &dst
			for range depth - 1 {
				tail.Next = &decodePathNativeChain{}
				tail = tail.Next
			}
			tail.Value.failure = failure
			src := decodeErrorPathInput(depth)
			var err error
			if dynamic {
				decoder := mustCompileTestDecoder[any](t, DecoderOptions{})
				var value any = &dst
				err = decoder.Decode(src, &value)
			} else {
				decoder := mustCompileTestDecoder[decodePathNativeChain](t, DecoderOptions{})
				err = decoder.Decode(src, &dst)
			}
			want := initialPath
			if !wrapped {
				want = strings.Repeat("next.", depth-1) + "value." + initialPath
			}
			if err != failure || cause.Path != want || cause.Offset != 17 || cause.Type != nil || cause.TypeName != "nativeValue" || cause.Reason != "failure" {
				t.Fatalf("dynamic=%v wrapped=%v: changed native error identity or metadata: %v", dynamic, wrapped, err)
			}
		}
	}
}

func TestDecodeBufferedErrorRetag(t *testing.T) {
	cause := &DecodeError{Offset: 17, Path: strings.Repeat("leaf", 100), TypeName: "original", Reason: "failure"}
	err := prependDecodePathField(cause, "child")
	err = retagCompiledError(err, reflect.TypeFor[int64]())
	err = prependDecodePathField(err, "root")
	if finishDecodeError(err) != cause || cause.Path != "root.child."+strings.Repeat("leaf", 100) || cause.Type != reflect.TypeFor[int64]() || cause.TypeName != "" || cause.Offset != 17 {
		t.Fatal("retagging changed the path, identity, or offset")
	}
}

func BenchmarkDecodeErrorPath(b *testing.B) {
	for _, depth := range []int{1, 8, 128, 1024, DefaultMaxDepth} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			src := decodeErrorPathInput(depth)
			decoder, err := CompileDecoder[decodeErrorPathChain](DecoderOptions{})
			if err != nil {
				b.Fatal(err)
			}
			var dst decodeErrorPathChain
			if err := decoder.Decode(src, &dst); err == nil {
				b.Fatal("missing type error")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := decoder.Decode(src, &dst); err == nil {
					b.Fatal("missing type error")
				}
			}
		})
	}
}
