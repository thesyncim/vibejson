package keyhash

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/thesyncim/vibejson/document"
)

var benchHashSink uint32

func BenchmarkBytes(b *testing.B) {
	lengths := []struct {
		name string
		data []byte
	}{
		{"Len0", []byte("")},
		{"Len2", []byte("id")},
		{"Len4", []byte("name")},
		{"Len7", []byte("user_id")},
		{"Len8", []byte("username")},
		{"Len12", []byte("created_time")},
		{"Len16", []byte("organization_ids")},
		{"Len32", []byte(strings.Repeat("k", 32))},
		{"Len64", []byte(strings.Repeat("k", 64))},
	}
	for _, tc := range lengths {
		b.Run(tc.name, func(b *testing.B) {
			var h uint32
			for b.Loop() {
				h = Bytes(tc.data)
			}
			benchHashSink = h
		})
	}
}

func BenchmarkString(b *testing.B) {
	for _, s := range []string{"id", "username", "organization_id", strings.Repeat("k", 32)} {
		b.Run(s[:min(len(s), 16)], func(b *testing.B) {
			var h uint32
			for b.Loop() {
				h = String(s)
			}
			benchHashSink = h
		})
	}
}

func BenchmarkWord(b *testing.B) {
	w := binary.LittleEndian.Uint64([]byte("username"))
	var h uint32
	for b.Loop() {
		h ^= Word(w, 0) ^ Word(w, 2) ^ Word(w, 4) ^ Word(w, 7) ^ Word(w, 8)
	}
	benchHashSink = h
}

func makeSyntheticTape(objects int) ([]byte, []document.IndexEntry) {
	// Realistic mix of short (<=8), medium (9..16), and long (>16) keys plus values.
	keys := []string{
		"id", "type", "name", "url", "text", "username",
		"created_at", "updated_at", "organization_id", "followers_count",
		"very_long_metadata_attribute_name",
	}
	var buf []byte
	var entries []document.IndexEntry
	buf = append(buf, '[')
	entries = append(entries, document.IndexEntry{Start: 0, Info: document.PackInfo(uint32(objects), document.Array, 0)})
	for o := 0; o < objects; o++ {
		if o > 0 {
			buf = append(buf, ',')
		}
		objStart := uint32(len(buf))
		objIdx := len(entries)
		buf = append(buf, '{')
		entries = append(entries, document.IndexEntry{Start: objStart, Info: document.PackInfo(uint32(len(keys)), document.Object, 0)})
		for kIdx, k := range keys {
			if kIdx > 0 {
				buf = append(buf, ',')
			}
			ks := uint32(len(buf))
			buf = append(buf, '"')
			buf = append(buf, k...)
			buf = append(buf, '"')
			ke := uint32(len(buf))
			entries = append(entries, document.IndexEntry{Start: ks, End: ke, Next: 1, Info: document.PackInfo(0, document.String, document.TapeFlagKey)})
			buf = append(buf, ':')
			vs := uint32(len(buf))
			buf = append(buf, `"val"`...)
			ve := uint32(len(buf))
			entries = append(entries, document.IndexEntry{Start: vs, End: ve, Next: 1, Info: document.PackInfo(0, document.String, 0)})
		}
		buf = append(buf, '}')
		entries[objIdx].End = uint32(len(buf))
		entries[objIdx].Next = uint32(len(entries) - objIdx)
	}
	buf = append(buf, ']')
	entries[0].End = uint32(len(buf))
	entries[0].Next = uint32(len(entries))
	return buf, entries
}

func BenchmarkEnrichIndex(b *testing.B) {
	src, entries := makeSyntheticTape(256)
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for b.Loop() {
		EnrichIndex(src, entries)
	}
}
