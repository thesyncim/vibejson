package simdjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/thesyncim/simdjson/document"
)

// canonicalMarshal serializes v the way this library's AppendJSON does: compact,
// HTML characters left unescaped. It is the oracle for round-trip tests.
func canonicalMarshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	// Encoder appends a newline; trim it.
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// TestLazyGetLastOccurrence verifies duplicate-key Get returns the last
// value, matching encoding/json's map semantics.
func TestLazyGetLastOccurrence(t *testing.T) {
	cases := []struct {
		doc  string
		key  string
		want string
	}{
		{`{"a":1,"a":2,"a":3}`, "a", "3"},
		{`{"a":1,"b":2,"a":9}`, "a", "9"},
		{`{"a":1,"b":2,"a":9}`, "b", "2"},
		{`{"x":{"y":1},"x":{"y":2}}`, "x", `{"y":2}`},
		{`{"dup":[1],"dup":[2],"dup":[3,4]}`, "dup", "[3,4]"},
		{`{"":5,"":6}`, "", "6"},
		{`{"abc":1,"abc":2}`, "abc", "2"}, // escaped key collides with plain
		{`{"abc":1,"abc":2}`, "abc", "2"},
	}
	for _, c := range cases {
		v, err := Parse([]byte(c.doc))
		if err != nil {
			t.Fatalf("Parse %q: %v", c.doc, err)
		}
		got, ok := v.Get(c.key)
		if !ok {
			t.Fatalf("Get(%q) in %q: not found", c.key, c.doc)
		}
		if s := got.String(); s != c.want {
			t.Fatalf("Get(%q) in %q = %s, want %s", c.key, c.doc, s, c.want)
		}
	}
}

// TestLazyEmptyContainers exercises the empty-object/array tape edge that
// the Get and iterator arithmetic must not step past.
func TestLazyEmptyContainers(t *testing.T) {
	docs := []string{
		`{}`, `[]`, `{"a":{}}`, `{"a":[]}`, `[[],{}]`, `[{},{},{}]`,
		`{"a":{},"b":{},"c":[]}`, `[[[]]]`, `{"x":{"y":{}}}`,
	}
	for _, d := range docs {
		v, err := Parse([]byte(d))
		if err != nil {
			t.Fatalf("Parse %q: %v", d, err)
		}
		if v.Kind() == document.Object {
			if _, ok := v.Get("missing"); ok {
				t.Fatalf("Get(missing) in %q returned ok", d)
			}
		}
		// The input is already canonical, so AppendJSON must reproduce it byte
		// for byte (member order and structure preserved).
		if got := v.String(); got != d {
			t.Fatalf("marshal %q = %s, want %s", d, got, d)
		}
	}
}

func TestNodeFromStorageRequiresBothBackings(t *testing.T) {
	src := []byte("0")
	entries := []IndexEntry{{}}
	for _, test := range []struct {
		name    string
		src     []byte
		entries []IndexEntry
		valid   bool
	}{
		{name: "both empty"},
		{name: "source only", src: src},
		{name: "entries only", entries: entries},
		{name: "both present", src: src, entries: entries, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nodeFromStorage(test.src, test.entries).valid(); got != test.valid {
				t.Fatalf("valid = %v, want %v", got, test.valid)
			}
		})
	}
}

// TestLazyNavigateFuzz builds canonical documents (unique keys, canonical
// numbers/strings, no HTML escapes), so the library's spelling-preserving
// AppendJSON must reproduce the input byte for byte, and the dynamic trees must
// round-trip through the same canonical serializer.
func TestLazyNavigateFuzz(t *testing.T) {
	r := rand.New(rand.NewSource(0x1A2F))
	for i := 0; i < testIterations(80_000, 800); i++ {
		native := randNative(r, 0)
		m := canonicalMarshal(native)
		v, err := Parse(m)
		if err != nil {
			t.Fatalf("Parse rejected canonical %q: %v", m, err)
		}

		if got := v.AppendJSON(nil); !bytes.Equal(got, m) {
			t.Fatalf("AppendJSON divergence:\n got %s\nwant %s", got, m)
		}

		// Any() and the UseNumber dynamic decode must round-trip to the same canonical
		// bytes. Any() yields json.Number for numbers, so compare via a
		// UseNumber re-decode of the source to keep numeric spelling.
		if got := canonicalMarshal(v.Any()); !bytes.Equal(got, m) {
			t.Fatalf("Any divergence:\n got %s\nwant %s", got, m)
		}
		pa, err := decodeAnyUseNumberForTest(m)
		if err != nil {
			t.Fatalf("UseNumber dynamic decode rejected %q: %v", m, err)
		}
		if got := canonicalMarshal(pa); !bytes.Equal(got, m) {
			t.Fatalf("dynamic decode divergence:\n got %s\nwant %s", got, m)
		}
	}
}

// TestLazyPointerFuzz walks random pointers into canonical documents and
// compares the resolved value's AppendJSON against the canonical marshaling of
// the corresponding stdlib-navigated sub-value.
func TestLazyPointerFuzz(t *testing.T) {
	r := rand.New(rand.NewSource(0xB0B))
	for i := 0; i < testIterations(60_000, 600); i++ {
		native := randNative(r, 0)
		m := canonicalMarshal(native)
		v, err := Parse(m)
		if err != nil {
			t.Fatalf("Parse %q: %v", m, err)
		}
		ptr, target := randomPointer(r, native)
		got, found, err := v.Pointer(ptr)
		if err != nil {
			t.Fatalf("Pointer(%q) in %q: %v", ptr, m, err)
		}
		if !found {
			t.Fatalf("Pointer(%q) in %q: not found", ptr, m)
		}
		want := canonicalMarshal(target)
		if gotBytes := got.AppendJSON(nil); !bytes.Equal(gotBytes, want) {
			t.Fatalf("Pointer(%q) in %q = %s, want %s", ptr, m, gotBytes, want)
		}
	}
}

// randomPointer descends v choosing a random path, returning an RFC6901 pointer
// and the target sub-value.
func randomPointer(r *rand.Rand, v any) (string, any) {
	ptr := ""
	for depth := 0; depth < 6; depth++ {
		switch node := v.(type) {
		case map[string]any:
			if len(node) == 0 || r.Intn(3) == 0 {
				return ptr, v
			}
			keys := make([]string, 0, len(node))
			for k := range node {
				keys = append(keys, k)
			}
			k := keys[r.Intn(len(keys))]
			ptr += "/" + escapePointerToken(k)
			v = node[k]
		case []any:
			if len(node) == 0 || r.Intn(3) == 0 {
				return ptr, v
			}
			idx := r.Intn(len(node))
			ptr += "/" + fmt.Sprint(idx)
			v = node[idx]
		default:
			return ptr, v
		}
	}
	return ptr, v
}

func escapePointerToken(k string) string {
	out := make([]byte, 0, len(k))
	for i := 0; i < len(k); i++ {
		switch k[i] {
		case '~':
			out = append(out, '~', '0')
		case '/':
			out = append(out, '~', '1')
		default:
			out = append(out, k[i])
		}
	}
	return string(out)
}

// randNative builds a random Go-native JSON value with unique object keys and
// canonical scalars, so its canonical marshaling is a stable oracle.
func randNative(r *rand.Rand, depth int) any {
	if depth > 4 {
		return randNativeScalar(r)
	}
	switch r.Intn(6) {
	case 0, 1:
		return randNativeScalar(r)
	case 2, 3:
		n := r.Intn(5)
		arr := make([]any, n)
		for i := range arr {
			arr[i] = randNative(r, depth+1)
		}
		return arr
	default:
		n := r.Intn(5)
		obj := make(map[string]any, n)
		for i := 0; i < n; i++ {
			obj[randNativeKey(r, i)] = randNative(r, depth+1)
		}
		return obj
	}
}

func randNativeScalar(r *rand.Rand) any {
	switch r.Intn(9) {
	case 0:
		return true
	case 1:
		return false
	case 2:
		return nil
	case 3:
		return float64(int64(r.Uint64() >> 12)) // integer-valued, round-trips cleanly
	case 4:
		return r.NormFloat64() * 1e6
	case 5:
		return "plain string"
	case 6:
		return "escapes \t\n\"\\ / and unicode é 𝄞 ✓"
	case 7:
		return json.Number(fmt.Sprintf("%d", int64(r.Uint64())))
	default:
		return json.Number(fmt.Sprintf("%d.%de%d", r.Intn(1000), r.Intn(1000000), r.Intn(40)-20))
	}
}

func randNativeKey(r *rand.Rand, i int) string {
	switch r.Intn(4) {
	case 0:
		return fmt.Sprintf("key%d", i)
	case 1:
		return fmt.Sprintf("k/with~special%d", i)
	case 2:
		return fmt.Sprintf("é%d", i)
	default:
		return fmt.Sprintf("Field%d", i)
	}
}
