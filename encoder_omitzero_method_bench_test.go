package vibejson

import (
	"testing"
	"time"
)

type benchOmitZeroValue struct {
	Number int
	Label  string
}

func (v benchOmitZeroValue) IsZero() bool { return v.Number == 0 }

type benchOmitZeroPointer struct{ Number int }

func (v *benchOmitZeroPointer) IsZero() bool { return v.Number == 0 }

type benchOmitZeroRecord[T any] struct {
	Value T `json:"value,omitzero"`
}

type benchOmitZeroFourTimes struct {
	A time.Time `json:"a,omitzero"`
	B time.Time `json:"b,omitzero"`
	C time.Time `json:"c,omitzero"`
	D time.Time `json:"d,omitzero"`
}

func benchmarkEncodeOmitZeroMethod[T any](b *testing.B, value T) {
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	dst, err := encoder.AppendJSON(nil, &value)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		dst, err = encoder.AppendJSON(dst[:0], &value)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeOmitZeroMethod(b *testing.B) {
	instant := time.Date(2026, 10, 23, 12, 34, 56, 0, time.UTC)
	b.Run("time/zero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[time.Time]{})
	})
	b.Run("time/nonzero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[time.Time]{instant})
	})
	b.Run("value/zero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[benchOmitZeroValue]{})
	})
	b.Run("value/nonzero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[benchOmitZeroValue]{benchOmitZeroValue{7, "value"}})
	})
	b.Run("four_times/zero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroFourTimes{})
	})
	b.Run("four_times/nonzero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroFourTimes{instant, instant, instant, instant})
	})
	b.Run("pointer/nil", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[*benchOmitZeroPointer]{})
	})
	b.Run("pointer/zero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[*benchOmitZeroPointer]{&benchOmitZeroPointer{}})
	})
	b.Run("pointer/nonzero", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[*benchOmitZeroPointer]{&benchOmitZeroPointer{7}})
	})
	b.Run("interface/nil", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[isZeroer]{})
	})
	b.Run("interface/value", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[isZeroer]{benchOmitZeroValue{7, "value"}})
	})
	b.Run("interface/pointer", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[isZeroer]{&benchOmitZeroPointer{7}})
	})
	b.Run("primitive/control", func(b *testing.B) {
		benchmarkEncodeOmitZeroMethod(b, benchOmitZeroRecord[int]{7})
	})
}

func checkOmitZeroMethodAllocations[T any](t *testing.T, value T) {
	t.Helper()
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 0, 256)
	if dst, err = encoder.AppendJSON(dst, &value); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		dst, err = encoder.AppendJSON(dst[:0], &value)
		if err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("omitzero value method allocated %.0f times per encode, want 0", allocs)
	}
}

func TestOmitZeroValueMethodAllocations(t *testing.T) {
	t.Run("time", func(t *testing.T) {
		checkOmitZeroMethodAllocations(t, benchOmitZeroRecord[time.Time]{})
	})
	t.Run("value", func(t *testing.T) {
		checkOmitZeroMethodAllocations(t, benchOmitZeroRecord[benchOmitZeroValue]{})
	})
	t.Run("four_times", func(t *testing.T) {
		checkOmitZeroMethodAllocations(t, benchOmitZeroFourTimes{})
	})
}
