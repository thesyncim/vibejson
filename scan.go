package slopjson

import "github.com/thesyncim/slopjson/internal/scanner"

func scanStringSpecial(src []byte, i int) int {
	return scanner.IndexStringSpecial(src, i)
}

// scanStringSpecialShort keeps sub-word cursor tails on the inlined scalar
// path instead of paying the selected scanner's call boundary.
func scanStringSpecialShort(src []byte, i int) (int, bool) {
	if len(src)-i >= 8 {
		return 0, false
	}
	for i < len(src) {
		c := src[i]
		if c < 0x20 || c >= 0x80 || c == '"' || c == '\\' {
			return i, true
		}
		i++
	}
	return len(src), true
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

func scanStringUnicodeRun(src []byte, i int) (next, bad int) {
	return scanner.ScanStringUnicodeRun(src, i)
}

func stringSpecialMask(word uint64) uint64 {
	return scanner.StringSpecialMask64(word)
}

func byteEqMask(word uint64, value byte) uint64 {
	return scanner.ByteEqualMask64(word, value)
}
