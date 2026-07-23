package slopjson

import "testing"

func mustBuildIndex(t *testing.T, src []byte) Index {
	t.Helper()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	index, err := BuildIndex(src, make([]IndexEntry, count))
	if err != nil {
		t.Fatal(err)
	}
	return index
}
