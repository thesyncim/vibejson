//go:build !go1.27 || go1.28 || !goexperiment.simd || (!arm64 && !amd64)

package scanner

import "unicode/utf8"

func scanStringSpecial(src []byte, i int) int {
	return scanStringSpecialScalar(src, i)
}

func scanStringSpecialLong(src []byte, i int) int {
	return scanStringSpecialScalar(src, i)
}

func scanStringSyntax(src []byte, i int) int {
	return scanStringSyntaxScalar(src, i)
}

func scanEncodedHTMLSpecialFast(src []byte, i int) int {
	return scanEncodedHTMLSpecialScalar(src, i)
}

func scanEncodedHTMLSyntaxFast(src []byte, i int) int {
	return scanEncodedHTMLSyntaxScalar(src, i)
}

func scanUnicodeEscapeRun(src []byte, i int) (int, bool) {
	return i, true
}

func validUTF8Fast(src []byte) bool {
	return utf8.Valid(src)
}

func validUTF8NoLineSeparatorFast(src []byte) bool {
	return utf8.Valid(src) && !hasJSONLineSeparatorScalar(src, 0)
}

func hasJSONLineSeparatorFast(src []byte, start int) bool {
	return hasJSONLineSeparatorScalar(src, start)
}

func copyStringPrefix(dst, src []byte) int {
	end := scanStringSpecialScalar(src, 0)
	copy(dst, src[:end])
	return end
}

func copyHTMLStringPrefix(dst, src []byte) int {
	end := scanEncodedHTMLSpecialScalar(src, 0)
	copy(dst, src[:end])
	return end
}

func currentInfo() Info {
	return Info{
		Backend: "scalar",
	}
}
