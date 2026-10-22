package vibejson

import (
	"bytes"
	"slices"
	"testing"
)

func TestBuildIndexNativeLayout(t *testing.T) {
	for _, value := range []string{
		`[]`, `{}`, `"string"`, `true`, `false`, `null`, `1234567890123`,
		`[1,"abc",true,false,null,{"key":"value"}]`,
	} {
		t.Run(value, func(t *testing.T) {
			// Whitespace makes the public builder select the packed index
			// engine even when the structural classifier is portable.
			src := append([]byte(value), bytes.Repeat([]byte{' '}, ValidBitmapMinBytes-len(value))...)
			want, err := buildIndexReference(src, make([]IndexEntry, 32))
			if err != nil {
				t.Fatal(err)
			}
			got, err := BuildIndex(src, make([]IndexEntry, 32))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Entries, want.Entries) {
				t.Fatalf("public entries = %+v, want %+v", got.Entries, want.Entries)
			}
			entries, ok := BuildIndexBitmap(src, make([]IndexEntry, 32))
			if !ok || !slices.Equal(entries, want.Entries) {
				t.Fatalf("packed entries = %+v, accepted %v; want %+v", entries, ok, want.Entries)
			}
		})
	}
}
