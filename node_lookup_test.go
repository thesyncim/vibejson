package vibejson

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNodeGetFlatPlainMatchesJSON(t *testing.T) {
	// Equivalent escaped and unescaped spellings must resolve to the last
	// member, including when that member is at either end of the object.
	keys := []string{
		`""`, `"a"`, `"\u0061"`, `"abc"`, `"\u0061bc"`,
		`"a/b"`, `"a\/b"`, `"café"`, `"caf\u00e9"`,
		`"😀"`, `"\ud83d\ude00"`, `"tab\tkey"`, `"tab\u0009key"`,
		`"nul\u0000key"`, `"a_much_longer_key"`,
	}
	for _, width := range []int{0, 1, 2, 3, 4, 7, 8, 9, 31, 32, 33, 128} {
		for shift := range keys {
			t.Run(fmt.Sprintf("width=%d/shift=%d", width, shift), func(t *testing.T) {
				var object strings.Builder
				object.WriteByte('{')
				for i := 0; i < width; i++ {
					if i != 0 {
						object.WriteByte(',')
					}
					fmt.Fprintf(&object, "%s:%d", keys[(i+shift)%len(keys)], i)
				}
				object.WriteByte('}')
				var want map[string]int64
				if err := json.Unmarshal([]byte(object.String()), &want); err != nil {
					t.Fatal(err)
				}

				// Surround the object with matching sibling keys to verify that
				// missing lookups stop at the object's own entry bounds.
				src := []byte(`[{"outside":-1},` + object.String() + `,{"outside":-2}]`)
				index := mustBuildIndex(t, src)
				node, ok := index.Root().Index(1)
				if !ok {
					t.Fatal("missing object")
				}
				for key, value := range want {
					got, found := node.Get(key)
					if !found {
						t.Fatalf("Get(%q) missing", key)
					}
					if n, ok := got.Int64(); !ok || n != value {
						t.Fatalf("Get(%q) = %d, %v; want %d", key, n, ok, value)
					}
					compiled, found := node.GetCompiled(CompileKey(key))
					if !found || compiled.Entry != got.Entry {
						t.Fatalf("GetCompiled(%q) disagrees with Get", key)
					}
				}
				for _, missing := range []string{"outside", "missing", "abcd", "caf", "a_much_longer_key_suffix"} {
					if _, found := node.Get(missing); found {
						t.Fatalf("Get(%q) unexpectedly found a member", missing)
					}
				}
			})
		}
	}
}
