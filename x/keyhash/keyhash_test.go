package keyhash

import (
	"encoding/binary"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/thesyncim/vibejson/document"
)

func TestBytesStringAgreement(t *testing.T) {
	vectors := []string{
		"", "a", "ab", "abc", "abcd", "abcde", "abcdef", "abcdefg", "abcdefgh",
		"abcdefghi", "abcdefghijklmnop", "abcdefghijklmnopq",
		strings.Repeat("k", 31), strings.Repeat("k", 32), strings.Repeat("k", 33),
		"héllo", "😀", `back\slash`, `esc\nspelling`, "\x00\x01\x02", "key with spaces",
	}
	for _, v := range vectors {
		if fromBytes, fromString := Bytes([]byte(v)), String(v); fromBytes != fromString {
			t.Fatalf("Bytes(%q) = %#x, String = %#x", v, fromBytes, fromString)
		}
	}
}

func TestWordAgreement(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 13))
	for round := 0; round < 2048; round++ {
		word := rng.Uint64()
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], word)
		for n := 0; n <= 8; n++ {
			got := Word(word, n)
			if want := Bytes(buf[:n]); got != want {
				t.Fatalf("Word(%#016x, %d) = %#x, Bytes = %#x", word, n, got, want)
			}
			if n < 8 {
				dirty := word | ^uint64(0)<<(8*n)
				if Word(dirty, n) != got {
					t.Fatalf("Word(%#016x, %d) depends on bytes past content", word, n)
				}
			}
		}
	}
}

func TestEnrichIndex(t *testing.T) {
	src := []byte(`{"a":"val","long_key_name":1,"":true}`)
	entries := []document.IndexEntry{
		{Start: 0, End: uint32(len(src)), Next: 7, Info: document.PackInfo(3, document.Object, 0)},
		{Start: 1, End: 4, Next: 1, Info: document.PackInfo(0, document.String, document.TapeFlagKey)},
		{Start: 5, End: 10, Next: 1, Info: document.PackInfo(0, document.String, 0)},
		{Start: 11, End: 26, Next: 1, Info: document.PackInfo(0, document.String, document.TapeFlagKey)},
		{Start: 27, End: 28, Next: 1, Info: document.PackInfo(0, document.Number, document.TapeFlagInt)},
		{Start: 29, End: 31, Next: 1, Info: document.PackInfo(0, document.String, document.TapeFlagKey)},
		{Start: 32, End: 36, Next: 1, Info: document.PackInfo(0, document.Bool, 0)},
	}

	EnrichIndex(src, entries)

	if !entries[0].KeysHashed() {
		t.Fatal("object entry missing TapeFlagObjectKeysHashed")
	}
	if entries[1].Next != String("a") {
		t.Fatalf("key \"a\" hash = %#x, want %#x", entries[1].Next, String("a"))
	}
	if entries[2].Next != 1 {
		t.Fatalf("value string Next = %d, want untouched 1", entries[2].Next)
	}
	if entries[3].Next != String("long_key_name") {
		t.Fatalf("key \"long_key_name\" hash = %#x, want %#x", entries[3].Next, String("long_key_name"))
	}
	if entries[5].Next != String("") {
		t.Fatalf("empty key hash = %#x, want %#x", entries[5].Next, String(""))
	}

	// Verify EnrichIndex agrees with Bytes/String across all key lengths 0..40,
	// including keys right at the end of src (no trailing 8-byte slack).
	for length := 0; length <= 40; length++ {
		key := strings.Repeat("k", length)
		raw := []byte(`"` + key + `"`)
		es := []document.IndexEntry{
			{Start: 0, End: uint32(len(raw)), Next: 1, Info: document.PackInfo(0, document.String, document.TapeFlagKey)},
		}
		EnrichIndex(raw, es)
		if want := Bytes([]byte(key)); es[0].Next != want {
			t.Fatalf("EnrichIndex length %d (tight end): got %#x, want %#x", length, es[0].Next, want)
		}
		padded := append(raw, `:1234567890`...)
		es[0].Next = 1
		EnrichIndex(padded, es)
		if want := Bytes([]byte(key)); es[0].Next != want {
			t.Fatalf("EnrichIndex length %d (padded): got %#x, want %#x", length, es[0].Next, want)
		}
	}
}

func TestZeroAlloc(t *testing.T) {
	keyBytes := []byte("organization_id")
	keyStr := "organization_id"
	src := []byte(`{"a":1,"organization_id":2}`)
	entries := []document.IndexEntry{
		{Start: 0, End: uint32(len(src)), Next: 5, Info: document.PackInfo(2, document.Object, 0)},
		{Start: 1, End: 4, Next: 1, Info: document.PackInfo(0, document.String, document.TapeFlagKey)},
		{Start: 5, End: 6, Next: 1, Info: document.PackInfo(0, document.Number, document.TapeFlagInt)},
		{Start: 7, End: 24, Next: 1, Info: document.PackInfo(0, document.String, document.TapeFlagKey)},
		{Start: 25, End: 26, Next: 1, Info: document.PackInfo(0, document.Number, document.TapeFlagInt)},
	}

	if allocs := testing.AllocsPerRun(100, func() {
		if Bytes(keyBytes) != String(keyStr) {
			t.Fatal("mismatch")
		}
		EnrichIndex(src, entries)
	}); allocs != 0 {
		t.Fatalf("keyhash operations allocated %.1f times", allocs)
	}
}
