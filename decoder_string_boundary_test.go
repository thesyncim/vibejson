package vibejson

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/thesyncim/vibejson/x/byteview"
)

func TestOwnedUnicodeStringReuse(t *testing.T) {
	decoder := mustCompileTestDecoder[string](t, DecoderOptions{})
	for _, text := range []string{"héllo β世界", strings.Repeat("a", 4096) + "β"} {
		t.Run(fmt.Sprint(len(text)), func(t *testing.T) {
			src := []byte(`"` + text + `"`)
			var dst string
			requireNoTestError(t, decoder.Decode(src, &dst))
			if dst != text {
				t.Fatalf("decoded %q, want %q", dst, text)
			}
			owned := unsafe.StringData(dst)
			requireNoTestError(t, decoder.Decode(src, &dst))
			if unsafe.StringData(dst) != owned {
				t.Fatal("equal independently owned Unicode string was replaced")
			}

			dst = byteview.String(src[1 : len(src)-1])
			aliased := unsafe.StringData(dst)
			requireNoTestError(t, decoder.Decode(src, &dst))
			if unsafe.StringData(dst) == aliased {
				t.Fatal("owned Unicode decode retained an alias into src")
			}
			for i := range src {
				src[i] = 'x'
			}
			if dst != text {
				t.Fatalf("source mutation changed owned Unicode string: %q", dst)
			}
		})
	}
}

func TestDecoderStringScanBoundaryParity(t *testing.T) {
	type textValue string
	for _, opts := range []DecoderOptions{{}, {ZeroCopy: true}, {Replace: true}, {ZeroCopy: true, Replace: true}} {
		decoder := mustCompileTestDecoder[textValue](t, opts)
		for _, prefixLength := range []int{0, 1, 7, 8, 15, 16, 31, 32, 63, 64, 255, 4095, 4096} {
			prefix := strings.Repeat("a", prefixLength)
			for _, suffix := range []string{"β", "世界", "😀", `\n`, `\"tail`, `\u03b2`, `\uD83D\uDE00`, `β\n世界`} {
				src := []byte(`"` + prefix + suffix + `"`)
				var want textValue
				requireNoTestError(t, json.Unmarshal(src, &want))
				got := textValue("previous")
				if err := decoder.Decode(src, &got); err != nil || got != want {
					t.Fatalf("options=%+v prefix=%d suffix=%q: got %q, %v; want %q", opts, prefixLength, suffix, got, err, want)
				}
			}
			for _, suffix := range []string{`\q"`, `\u00"`, string([]byte{0xff, '"'}), string([]byte{0xc2, '"'}), "\n\"", "unterminated"} {
				src := []byte(`"` + prefix + suffix)
				got := textValue("previous")
				if err := decoder.Decode(src, &got); err == nil {
					t.Fatalf("options=%+v prefix=%d: invalid suffix %q accepted", opts, prefixLength, suffix)
				}
			}
		}
	}
}

func TestOwnedUnicodeStringReuseAllocations(t *testing.T) {
	decoder := mustCompileTestDecoder[string](t, DecoderOptions{})
	for _, text := range []string{"héllo β世界", strings.Repeat("a", 4096) + "β"} {
		src := []byte(`"` + text + `"`)
		var dst string
		requireNoTestError(t, decoder.Decode(src, &dst))
		if allocs := testing.AllocsPerRun(100, func() {
			if err := decoder.Decode(src, &dst); err != nil {
				panic(err)
			}
		}); allocs != 0 {
			t.Fatalf("reused %d-byte Unicode string allocated %v times, want 0", len(text), allocs)
		}
	}
}

func TestDecoderUnicodeStringMatchesParser(t *testing.T) {
	for _, zeroCopy := range []bool{false, true} {
		for _, prefixLength := range []int{0, 7, 8, 31, 32, 4096} {
			for _, suffix := range []string{`" , 7`, `\n" , 7`, `\q" , 7`, `\uD800" , 7`, `\u00`, `\`, string([]byte{0xff, '"'}), string([]byte{0xc2, '"'})} {
				align := prefixLength % 16
				src := []byte(strings.Repeat(" ", align) + `"` + strings.Repeat("a", prefixLength) + "β" + suffix)
				cursor := newDecoderCursor(src, DecoderOptions{ZeroCopy: zeroCopy})
				cursor.i = align
				reference := parser{src: src, i: align, maxDepth: DefaultMaxDepth, zeroCopy: zeroCopy}
				want, wantErr := reference.parseString()
				got := "previous"
				err := cursor.String(&got)
				if wantErr != nil {
					want = "previous"
				}
				if got != want || cursor.i != reference.i || !reflect.DeepEqual(err, wantErr) {
					t.Fatalf("zeroCopy=%v prefix=%d suffix=%q: cursor=(%q,%d,%v), parser=(%q,%d,%v)", zeroCopy, prefixLength, suffix, got, cursor.i, err, want, reference.i, wantErr)
				}
			}
		}
	}
}

func TestJSONStringLayoutHintPrefixParity(t *testing.T) {
	for _, prefixLength := range []int{0, 1, 7, 8, 31, 32, 4096} {
		for _, prefix := range []string{strings.Repeat("a", prefixLength), "β" + strings.Repeat("a", prefixLength), string([]byte{0xff}) + strings.Repeat("a", prefixLength)} {
			for _, suffix := range []string{`"`, `\n"`, `\u0041"`, `\uD83D\uDE00"`, `\q"`, `\u00`, `\`, "\n", "unterminated"} {
				src := []byte(`"` + prefix + suffix)
				wantEnd, wantCapacity := rawJSONStringLayoutHint(src, 1)
				stop := scanStringSpecial(src, 1)
				if stop < len(src) && src[stop] >= 0x80 {
					stop = scanStringSyntax(src, stop)
				}
				end, suffixCapacity := rawJSONStringLayoutHint(src, stop)
				capacity := stop - 1 + suffixCapacity
				if end != wantEnd || capacity != wantCapacity {
					t.Fatalf("prefix=%q suffix=%q: resumed=(%d,%d), full=(%d,%d)", prefix, suffix, end, capacity, wantEnd, wantCapacity)
				}
			}
		}
	}
}
