//go:build go1.27 && !go1.28 && goexperiment.simd && amd64

package simd

// Parse16Digits reduces sixteen ASCII decimal digits without validating them.
// The portable two-word SWAR reduction is faster than the current amd64 SIMD
// reduction on Go tip and avoids a runtime CPU-feature branch on every call.
// Call All16Digits first when the input is not already known to be digits.
func Parse16Digits(digits *[16]byte) uint64 {
	return parse16DigitsScalar(digits)
}

// Parse16DigitsChecked validates and reduces sixteen ASCII decimal digits in
// one operation. It returns false and zero when any byte is not a digit.
func Parse16DigitsChecked(digits *[16]byte) (uint64, bool) {
	if !All16Digits(digits) {
		return 0, false
	}
	return parse16DigitsScalar(digits), true
}

func store16Digits(dst *[16]byte, value uint64) {
	store16DigitsScalar(dst, value)
}

func storeDateTimeParts(dst *[20]byte, year, month, day, hour, minute, second uint32) {
	storeDateTimePartsScalar(dst, year, month, day, hour, minute, second)
}

func parseBackend() string {
	return "scalar"
}

func parseVectorBytes() int {
	return 0
}

func formatBackend() string {
	return "scalar"
}

func formatVectorBytes() int {
	return 0
}
