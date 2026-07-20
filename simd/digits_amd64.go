//go:build go1.27 && !go1.28 && goexperiment.simd && amd64

package simd

func store16Digits(dst *[16]byte, value uint64) {
	store16DigitsScalar(dst, value)
}

func storeDateTimeParts(dst *[20]byte, year, month, day, hour, minute, second uint32) {
	storeDateTimePartsScalar(dst, year, month, day, hour, minute, second)
}

func formatBackend() string {
	return "scalar"
}

func formatVectorBytes() int {
	return 0
}
