package simdjson

import "github.com/thesyncim/simdjson/internal/scanner"

func scanStringSpecial(src []byte, i int) int {
	return scanner.IndexStringSpecial(src, i)
}

func scanStringSpecialLong(src []byte, i int) int {
	return scanner.IndexStringSpecialLong(src, i)
}

func scanStringSyntax(src []byte, i int) int {
	return scanner.IndexStringSyntax(src, i)
}

func scanEncodedHTMLSpecialFast(src []byte, i int) int {
	return scanner.IndexHTMLStringSpecial(src, i)
}

func scanEncodedHTMLSyntaxFast(src []byte, i int) int {
	return scanner.IndexHTMLStringSyntax(src, i)
}

func scanUnicodeEscapeRun(src []byte, i int) (int, bool) {
	return scanner.ScanUnicodeEscapeRun(src, i)
}

func validUTF8Fast(src []byte) bool {
	return scanner.ValidUTF8(src)
}

func validUTF8NoLineSeparatorFast(src []byte) bool {
	return scanner.ValidUTF8NoLineSeparator(src)
}

func hasJSONLineSeparatorFast(src []byte, start int) bool {
	return scanner.HasJSONLineSeparator(src, start)
}

func scanStringUnicodeRun(src []byte, i int) (next, bad int) {
	return scanner.ScanStringUnicodeRun(src, i)
}

func stringSpecialMask(word uint64) uint64 {
	return scanner.StringSpecialMask64(word)
}

func stringSyntaxMask(word uint64) uint64 {
	return scanner.StringSyntaxMask64(word)
}

func byteEqMask(word uint64, value byte) uint64 {
	return scanner.ByteEqualMask64(word, value)
}
