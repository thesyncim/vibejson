package vibejson

import (
	"bytes"
	"testing"
)

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

func TestValueFrameSIMDMatchesScalar(t *testing.T) {
	for _, src := range frameCorpus() {
		if len(src) == 0 {
			continue
		}
		var fast ValueFrame
		var ref scalarFrame
		fast.Init(src[0])
		ref.init(src[0])
		var fastDone, refDone bool
		for n := 1; n <= len(src); n++ {
			if !fastDone {
				fastDone = fast.Scan(src, 0, n)
			}
			if !refDone {
				refDone = ref.scan(src, 0, n)
			}
			if fastDone != refDone || fast.Framed != ref.framed {
				t.Fatalf("divergence on %.60q at n=%d: simd(done=%v,framed=%d) scalar(done=%v,framed=%d)",
					src, n, fastDone, fast.Framed, refDone, ref.framed)
			}
			if fastDone {
				break
			}
		}
		var whole ValueFrame
		whole.Init(src[0])
		wholeDone := whole.Scan(src, 0, len(src))
		if wholeDone != fastDone || whole.Framed != fast.Framed {
			t.Fatalf("whole vs incremental divergence on %.60q: whole(done=%v,framed=%d) incr(done=%v,framed=%d)",
				src, wholeDone, whole.Framed, fastDone, fast.Framed)
		}
	}
}

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

func BenchmarkValueFrameScan(b *testing.B) {
	for _, in := range frameBenchInputs() {
		b.Run(in.name+"/SIMD", func(b *testing.B) {
			b.SetBytes(int64(len(in.data)))
			for i := 0; i < b.N; i++ {
				var f ValueFrame
				f.Init(in.data[0])
				f.Scan(in.data, 0, len(in.data))
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

func checkValueFrameSIMDMatchesScalar(t *testing.T, src []byte, step uint16) {
	t.Helper()
	if len(src) == 0 || len(src) > 1<<14 {
		return
	}
	var fast ValueFrame
	var ref scalarFrame
	fast.Init(src[0])
	ref.init(src[0])
	stride := 1 + int(step%64)
	var fastDone, refDone bool
	for n := 1; n <= len(src); {
		if !fastDone {
			fastDone = fast.Scan(src, 0, n)
		}
		if !refDone {
			refDone = ref.scan(src, 0, n)
		}
		if fastDone != refDone || fast.Framed != ref.framed {
			t.Fatalf("divergence on %.60q at n=%d: simd(done=%v,framed=%d) scalar(done=%v,framed=%d)",
				src, n, fastDone, fast.Framed, refDone, ref.framed)
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
