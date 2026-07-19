// Package scanner contains the JSON byte-scanning implementation used by the
// root package and the checked public SIMD facade.
package scanner

import "unsafe"

// IndexStringSpecial returns the first quote, backslash, control, or non-ASCII
// byte at or after start. Internal callers must prove 0 <= start <= len(src).
func IndexStringSpecial(src []byte, start int) int {
	return scanStringSpecial(src, start)
}

// IndexStringSpecialLong enters the selected long scanner directly. Internal
// callers must prove 0 <= start <= len(src).
func IndexStringSpecialLong(src []byte, start int) int {
	return scanStringSpecialLong(src, start)
}

// IndexStringSyntax returns the first quote, backslash, or control byte at or
// after start. Internal callers must prove 0 <= start <= len(src).
func IndexStringSyntax(src []byte, start int) int {
	return scanStringSyntax(src, start)
}

// IndexHTMLStringSpecial is IndexStringSpecial with '<', '>', and '&' added to
// the stop set. Internal callers must prove 0 <= start <= len(src).
func IndexHTMLStringSpecial(src []byte, start int) int {
	return scanEncodedHTMLSpecialFast(src, start)
}

// IndexHTMLStringSyntax is IndexStringSyntax with '<', '>', and '&' added to
// the stop set. Internal callers must prove 0 <= start <= len(src).
func IndexHTMLStringSyntax(src []byte, start int) int {
	return scanEncodedHTMLSyntaxFast(src, start)
}

// ScanUnicodeEscapeRun scans complete vector-sized groups of JSON escapes.
// Internal callers must prove 0 <= start <= len(src).
func ScanUnicodeEscapeRun(src []byte, start int) (end int, ok bool) {
	return scanUnicodeEscapeRun(src, start)
}

// ValidUTF8 reports whether src consists entirely of valid UTF-8.
func ValidUTF8(src []byte) bool {
	return validUTF8Fast(src)
}

// ValidUTF8NoLineSeparator reports whether src is valid UTF-8 without U+2028
// or U+2029.
func ValidUTF8NoLineSeparator(src []byte) bool {
	return validUTF8NoLineSeparatorFast(src)
}

// HasJSONLineSeparator reports whether U+2028 or U+2029 occurs at or after
// start. Internal callers must prove 0 <= start <= len(src).
func HasJSONLineSeparator(src []byte, start int) bool {
	return hasJSONLineSeparatorFast(src, start)
}

// ScanStringUnicodeRun scans a non-ASCII string run. Internal callers must
// prove 0 <= start <= len(src).
func ScanStringUnicodeRun(src []byte, start int) (next, bad int) {
	return scanStringUnicodeRun(src, start)
}

// StringSpecialMask64 returns the quote, backslash, control, and non-ASCII
// high-bit byte mask for a little-endian word.
func StringSpecialMask64(word uint64) uint64 {
	return stringSpecialMask(word)
}

// StringSyntaxMask64 returns the quote, backslash, and control high-bit byte
// mask for a little-endian word.
func StringSyntaxMask64(word uint64) uint64 {
	return stringSyntaxMask(word)
}

// HTMLStringSpecialMask64 adds '<', '>', and '&' to StringSpecialMask64.
func HTMLStringSpecialMask64(word uint64) uint64 {
	return htmlStringSpecialMask(word)
}

// ByteEqualMask64 returns a high-bit byte mask for lanes equal to value.
func ByteEqualMask64(word uint64, value byte) uint64 {
	return byteEqMask(word, value)
}

// CopyStringPrefix copies the prefix that needs no JSON string escaping. It
// returns -1 for a short destination or overlapping slices.
func CopyStringPrefix(dst, src []byte) int {
	if len(dst) < len(src) {
		return -1
	}
	dst = dst[:len(src)]
	if slicesOverlap(dst, src) {
		return -1
	}
	return copyStringPrefix(dst, src)
}

// CopyHTMLStringPrefix is CopyStringPrefix with '<', '>', and '&' in the stop
// set.
func CopyHTMLStringPrefix(dst, src []byte) int {
	if len(dst) < len(src) {
		return -1
	}
	dst = dst[:len(src)]
	if slicesOverlap(dst, src) {
		return -1
	}
	return copyHTMLStringPrefix(dst, src)
}

func slicesOverlap(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	a0 := uintptr(unsafe.Pointer(unsafe.SliceData(a)))
	b0 := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	return a0 < b0+uintptr(len(b)) && b0 < a0+uintptr(len(a))
}
