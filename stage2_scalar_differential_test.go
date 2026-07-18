package simdjson

import (
	"testing"
	"unsafe"
)

func TestStage2FastNumberScannerMatchesDiagnosticScanner(t *testing.T) {
	alphabet := []byte{'0', '1', '2', '9', '-', '+', '.', 'e', 'E', ' ', ',', ']', '}', 'x'}
	state := uint64(0x9e3779b97f4a7c15)
	buf := make([]byte, 48)
	for round := 0; round < testIterations(1_000_000, 10_000); round++ {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		n := int(state%uint64(len(buf)-1)) + 1
		for i := 0; i < n; i++ {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			buf[i] = alphabet[state%uint64(len(alphabet))]
		}
		src := buf[:n]
		wantEnd, msg := scanNumber(src, 0)
		gotEnd, gotOK := scanNumberFast(unsafe.Pointer(unsafe.SliceData(src)), len(src), 0)
		wantOK := msg == ""
		if gotOK != wantOK || gotOK && gotEnd != wantEnd {
			t.Fatalf("round %d src=%q: fast=(%d,%v), diagnostic=(%d,%q)", round, src, gotEnd, gotOK, wantEnd, msg)
		}
	}
}
