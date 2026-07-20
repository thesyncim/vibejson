package simd_test

import (
	"fmt"

	"github.com/thesyncim/simdjson/simd"
)

func ExampleParse16Digits() {
	digits := [16]byte{'1', '2', '3', '4', '5', '6', '7', '8', '9', '0', '1', '2', '3', '4', '5', '6'}
	if simd.All16Digits(&digits) {
		fmt.Println(simd.Parse16Digits(&digits))
	}
	// Output: 1234567890123456
}
