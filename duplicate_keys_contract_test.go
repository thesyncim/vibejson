package slopjson

import (
	"testing"
)

// ---------------------------------------------------------------------------
// duplicate-key semantics must agree across every pointer lookup path.
// Node.Get, Value.Get, and GetRaw use last-wins. ScanFirstRaw names and tests
// its first-wins early-exit contract explicitly.
// ---------------------------------------------------------------------------

// Duplicate object keys resolve differently by contract: the Get family
// matches encoding/json's last occurrence, while the early-exit Scan family
// resolves each token to the first occurrence it meets and never sees later
// duplicates. This pins both contracts on the divergent example.
func TestDuplicateKeyContracts(t *testing.T) {
	src := []byte(`{"dup":{"x":1},"dup":{}}`)

	for _, tc := range []struct {
		pointer string
		getOK   bool
		getRaw  string
		scanOK  bool
		scanRaw string
	}{
		{"/dup", true, `{}`, true, `{"x":1}`},
		{"/dup/x", false, ``, true, `1`},
		{"/dup/y", false, ``, false, ``},
		{"", true, string(src), true, string(src)},
	} {
		getRaw, getOK, getErr := GetRaw(src, tc.pointer)
		if getErr != nil || getOK != tc.getOK || string(getRaw.Bytes()) != tc.getRaw {
			t.Errorf("GetRaw(%q) = %q, %v, %v; want %q, %v", tc.pointer, getRaw.Bytes(), getOK, getErr, tc.getRaw, tc.getOK)
		}
		node, nodeOK, nodeErr := mustBuildIndex(t, src).Pointer(tc.pointer)
		if nodeErr != nil || nodeOK != tc.getOK || string(node.Raw().Bytes()) != tc.getRaw {
			t.Errorf("Index.Pointer(%q) = %q, %v, %v; want %q, %v", tc.pointer, node.Raw().Bytes(), nodeOK, nodeErr, tc.getRaw, tc.getOK)
		}
		scanRaw, scanOK, scanErr := ScanFirstRaw(src, tc.pointer)
		if scanErr != nil || scanOK != tc.scanOK || string(scanRaw.Bytes()) != tc.scanRaw {
			t.Errorf("ScanFirstRaw(%q) = %q, %v, %v; want %q, %v", tc.pointer, scanRaw.Bytes(), scanOK, scanErr, tc.scanRaw, tc.scanOK)
		}
		compiled, err := CompilePointer(tc.pointer)
		if err != nil {
			t.Fatal(err)
		}
		compiledRaw, compiledOK, compiledErr := compiled.ScanFirstRaw(src)
		if compiledErr != nil || compiledOK != tc.scanOK || string(compiledRaw.Bytes()) != tc.scanRaw {
			t.Errorf("CompiledPointer.ScanFirstRaw(%q) = %q, %v, %v; want %q, %v", tc.pointer, compiledRaw.Bytes(), compiledOK, compiledErr, tc.scanRaw, tc.scanOK)
		}
	}
}
