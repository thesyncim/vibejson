package slopjson

import (
	"bytes"
	"testing"
)

// scalarFrame is an independent, byte-at-a-time reference for valueFrame.scan.
// It is the pre-SIMD algorithm, kept here so the SIMD framer can be pinned
// against it: for any input and any prefix length the two must agree on the
// framed extent and on whether the value is structurally complete. Keeping the
// reference in the test file guarantees it never shares code with the scanner
// it checks.
type scalarFrame struct {
	mode    uint8
	depth   int
	inStr   bool
	esc     bool
	numE    bool
	litLeft int
	framed  int
}

func (f *scalarFrame) init(c byte) {
	f.framed = 1
	switch {
	case c == '"':
		f.mode = frameString
	case c == '{' || c == '[':
		f.mode = frameContainer
		f.depth = 1
	case c == 't' || c == 'n':
		f.mode = frameLiteral
		f.litLeft = 3
	case c == 'f':
		f.mode = frameLiteral
		f.litLeft = 4
	default:
		f.mode = frameNumber
	}
}

func (f *scalarFrame) scan(src []byte, start, n int) bool {
	i := start + f.framed
	switch f.mode {
	case frameString:
		for i < n {
			c := src[i]
			i++
			if f.esc {
				f.esc = false
				continue
			}
			switch c {
			case '\\':
				f.esc = true
			case '"':
				f.framed = i - start
				return true
			}
		}
	case frameNumber:
		for i < n {
			c := src[i]
			switch {
			case c >= '0' && c <= '9', c == '.':
				f.numE = false
			case c == 'e' || c == 'E':
				f.numE = true
			case c == '+' || c == '-':
				if !f.numE {
					f.framed = i - start
					return true
				}
				f.numE = false
			default:
				f.framed = i - start
				return true
			}
			i++
		}
	case frameLiteral:
		for i < n && f.litLeft > 0 {
			i++
			f.litLeft--
		}
		f.framed = i - start
		return f.litLeft == 0
	default: // frameContainer
		for i < n {
			c := src[i]
			i++
			if f.inStr {
				if f.esc {
					f.esc = false
					continue
				}
				switch c {
				case '\\':
					f.esc = true
				case '"':
					f.inStr = false
				}
				continue
			}
			switch c {
			case '"':
				f.inStr = true
			case '{', '[':
				f.depth++
			case '}', ']':
				f.depth--
				if f.depth == 0 {
					f.framed = i - start
					return true
				}
			}
		}
	}
	f.framed = i - start
	return false
}

// frameCorpus exercises every scan mode and the boundaries the SIMD string
// scanner cares about: plain runs long enough to cross a vector, escapes at and
// across chunk edges, escaped quotes and backslashes, control and non-ASCII
// bytes as string content, nested strings holding brackets, and truncations.
func frameCorpus() [][]byte {
	long := bytes.Repeat([]byte("a"), 200)
	utf8 := bytes.Repeat([]byte("héllo   世界 "), 8)
	ctrl := append([]byte("ctrl"), 0x01, 0x02, 0x1f, 'x')
	cases := []string{
		`"simple"`,
		`"with \"escaped\" quotes"`,
		`"trailing backslash pair \\"`,
		`"\\\\\\\""`,
		`""`,
		`"` + string(long) + `"`,
		`"` + string(utf8) + `"`,
		`"` + string(ctrl) + `"`,
		`{}`,
		`[]`,
		`{"k":"v","arr":[1,2,{"n":"}]{"}]}`,
		`[{"a":"]"},{"b":"["}]`,
		`{"s":"` + string(long) + `","t":"` + string(utf8) + `"}`,
		`[` + string(bytes.Repeat([]byte(`"xxxxxxxxxxxxxxxxxxxx",`), 20)) + `"end"]`,
		`true`,
		`false`,
		`null`,
		`  false`, // leading space is not part of a framed value; still valid to scan
		`0`,
		`-12.5e+10`,
		`3.14159E-2`,
		`123 `,
		`{"open":`,
		`"unterminated`,
		`"unterminated \`,
		`[1,2,"str`,
		`{"a":"b\`,
		string([]byte{'"', 0xff, 0xfe, '"'}),
	}
	out := make([][]byte, 0, len(cases))
	for _, c := range cases {
		out = append(out, []byte(c))
	}
	return out
}

// TestValueFrameSIMDMatchesScalar pins the SIMD framer against the scalar
// reference. For every corpus entry and every prefix length, the value is
// revealed one byte at a time (the worst case for a resumable scanner) and the
// two framers must agree on completion and framed extent after each reveal.
func TestValueFrameSIMDMatchesScalar(t *testing.T) {
	for _, src := range frameCorpus() {
		if len(src) == 0 {
			continue
		}
		var fast valueFrame
		var ref scalarFrame
		fast.init(src[0])
		ref.init(src[0])
		var fastDone, refDone bool
		for n := 1; n <= len(src); n++ {
			if !fastDone {
				fastDone = fast.scan(src, 0, n)
			}
			if !refDone {
				refDone = ref.scan(src, 0, n)
			}
			if fastDone != refDone || fast.framed != ref.framed {
				t.Fatalf("divergence on %.60q at n=%d: simd(done=%v,framed=%d) scalar(done=%v,framed=%d)",
					src, n, fastDone, fast.framed, refDone, ref.framed)
			}
			if fastDone {
				break
			}
		}
		// Feeding the whole buffer at once must land in the same place as the
		// byte-at-a-time reveal did.
		var whole valueFrame
		whole.init(src[0])
		wholeDone := whole.scan(src, 0, len(src))
		if wholeDone != fastDone || whole.framed != fast.framed {
			t.Fatalf("whole vs incremental divergence on %.60q: whole(done=%v,framed=%d) incr(done=%v,framed=%d)",
				src, wholeDone, whole.framed, fastDone, fast.framed)
		}
	}
}

// frameBenchInputs returns representative large values whose framing time is
// dominated by string-body scanning: a single large string, a large ASCII
// string embedded in an object, and an array of medium strings.
func frameBenchInputs() []struct {
	name string
	data []byte
} {
	bigStr := append(append([]byte{'"'}, bytes.Repeat([]byte("abcdefghijklmnopqrstuvwxyz0123456789"), 4096)...), '"')
	obj := append(append([]byte(`{"payload":`), bigStr...), '}')
	arr := append(append([]byte{'['},
		bytes.Repeat(append(append([]byte{'"'}, bytes.Repeat([]byte("value-"), 40)...), '"', ','), 200)...),
		'0', ']')
	return []struct {
		name string
		data []byte
	}{
		{"BigString", bigStr},
		{"ObjectBigString", obj},
		{"ArrayStrings", arr},
	}
}

// BenchmarkValueFrameScan measures the SIMD framer against the scalar reference
// on large values so the win from vectorizing the string-body scan is visible.
func BenchmarkValueFrameScan(b *testing.B) {
	for _, in := range frameBenchInputs() {
		b.Run(in.name+"/SIMD", func(b *testing.B) {
			b.SetBytes(int64(len(in.data)))
			for i := 0; i < b.N; i++ {
				var f valueFrame
				f.init(in.data[0])
				f.scan(in.data, 0, len(in.data))
			}
		})
		b.Run(in.name+"/Scalar", func(b *testing.B) {
			b.SetBytes(int64(len(in.data)))
			for i := 0; i < b.N; i++ {
				var f scalarFrame
				f.init(in.data[0])
				f.scan(in.data, 0, len(in.data))
			}
		})
	}
}

// checkValueFrameSIMDMatchesScalar generalizes the deterministic frame check
// to arbitrary bytes and chunk boundaries. The retained stream campaign calls
// it before exercising the same bytes through Reader.
func checkValueFrameSIMDMatchesScalar(t *testing.T, src []byte, step uint16) {
	t.Helper()
	if len(src) == 0 || len(src) > 1<<14 {
		return
	}
	var fast valueFrame
	var ref scalarFrame
	fast.init(src[0])
	ref.init(src[0])
	stride := 1 + int(step%64)
	var fastDone, refDone bool
	for n := 1; n <= len(src); {
		if !fastDone {
			fastDone = fast.scan(src, 0, n)
		}
		if !refDone {
			refDone = ref.scan(src, 0, n)
		}
		if fastDone != refDone || fast.framed != ref.framed {
			t.Fatalf("divergence on %.60q at n=%d: simd(done=%v,framed=%d) scalar(done=%v,framed=%d)",
				src, n, fastDone, fast.framed, refDone, ref.framed)
		}
		if fastDone || n == len(src) {
			break
		}
		n += stride
		if n > len(src) {
			n = len(src)
		}
	}
}
