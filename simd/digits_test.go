package simd

import (
	"fmt"
	"strconv"
	"testing"
)

func TestDigitKernels(t *testing.T) {
	state := uint64(0x9e3779b97f4a7c15)
	for range 100000 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17

		var text16 [16]byte
		copy(text16[:], strconv.FormatUint(1_000_000_000_000_000+state%9_000_000_000_000_000, 10))
		if !All16Digits(&text16) {
			t.Fatalf("All16Digits rejected %q", text16)
		}
		var text8 [8]byte
		copy(text8[:], fmt.Sprintf("%08d", state%1e8))
		if !All8Digits(&text8) {
			t.Fatalf("All8Digits rejected %q", text8)
		}
		want8, _ := strconv.ParseUint(string(text8[:]), 10, 64)
		if got := Parse8Digits(&text8); got != want8 {
			t.Fatalf("Parse8Digits(%q) = %d, want %d", text8, got, want8)
		}
	}
}

func TestDigitClassifiersRejectEveryByte(t *testing.T) {
	valid8 := [8]byte{'0', '1', '2', '3', '4', '5', '6', '7'}
	valid16 := [16]byte{'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '0', '1', '2', '3', '4', '5'}
	for value := range 256 {
		want := '0' <= byte(value) && byte(value) <= '9'
		for i := range valid8 {
			candidate := valid8
			candidate[i] = byte(value)
			if got := All8Digits(&candidate); got != want {
				t.Fatalf("All8Digits position %d byte %#02x = %v, want %v", i, value, got, want)
			}
		}
		for i := range valid16 {
			candidate := valid16
			candidate[i] = byte(value)
			if got := All16Digits(&candidate); got != want {
				t.Fatalf("All16Digits position %d byte %#02x = %v, want %v", i, value, got, want)
			}
		}
	}
}

func TestStore16Digits(t *testing.T) {
	values := []uint64{0, 1, 9, 10, 99, 100, 9999, 10000, 99999999, 100000000, 9999999999999999}
	state := uint64(0x243f6a8885a308d3)
	for range 100000 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		values = append(values, state%1e16)
	}
	for _, value := range values {
		var got [16]byte
		Store16Digits(&got, value)
		want := fmt.Sprintf("%016d", value)
		if string(got[:]) != want {
			t.Fatalf("Store16Digits(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestStore8Digits(t *testing.T) {
	values := []uint64{0, 1, 9, 10, 99, 100, 9999, 10000, 99999999}
	state := uint64(0x13198a2e03707344)
	for range 100000 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		values = append(values, state%1e8)
	}
	for _, value := range values {
		var got [8]byte
		Store8Digits(&got, value)
		want := fmt.Sprintf("%08d", value)
		if string(got[:]) != want {
			t.Fatalf("Store8Digits(%d) = %q, want %q", value, got, want)
		}
	}
}

func BenchmarkStore16Digits(b *testing.B) {
	values := [...]uint64{1234567890123456, 9999999999999999}
	var dst [16]byte
	b.Run("selected", func(b *testing.B) {
		for i := range b.N {
			Store16Digits(&dst, values[i&1])
		}
	})
	b.Run("scalar", func(b *testing.B) {
		for i := range b.N {
			store16DigitsScalar(&dst, values[i&1])
		}
	})
	b.Run("strconv", func(b *testing.B) {
		buf := dst[:0]
		for i := range b.N {
			buf = strconv.AppendUint(buf[:0], values[i&1], 10)
		}
	})
}

func BenchmarkStore8Digits(b *testing.B) {
	values := [...]uint64{12345678, 99999999}
	var dst [8]byte
	b.Run("selected", func(b *testing.B) {
		for i := range b.N {
			Store8Digits(&dst, values[i&1])
		}
	})
	b.Run("strconv", func(b *testing.B) {
		buf := dst[:0]
		for i := range b.N {
			buf = strconv.AppendUint(buf[:0], values[i&1], 10)
		}
	})
}
