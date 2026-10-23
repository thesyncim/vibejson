package vibejson

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errReaderRejectedValue = errors.New("custom decoder rejected value")

type readerRejectJSON struct {
	calls int
	raw   string
}

func (v *readerRejectJSON) UnmarshalJSON(src []byte) error {
	v.calls++
	v.raw = string(src)
	return errReaderRejectedValue
}

type readerRejectNative struct {
	calls int
	raw   string
}

func (v *readerRejectNative) UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error) {
	raw, err := c.Raw()
	if err != nil {
		return c, err
	}
	v.calls++
	v.raw = string(raw.Src)
	return c, errReaderRejectedValue
}

func TestDecodeNextPreservesFirstNumberDecodeError(t *testing.T) {
	standard := mustCompileTestDecoder[readerRejectJSON](t, DecoderOptions{})
	native := mustCompileTestDecoder[readerRejectNative](t, DecoderOptions{})
	scalar := mustCompileTestDecoder[bool](t, DecoderOptions{})
	for _, tc := range []struct {
		input string
		first string
	}{
		{"1.2 ", "1.2"},
		{"1.2.3", "1.2"},
		{"1e2e3", "1e2"},
	} {
		for _, chunk := range []int{1, 512} {
			for _, mode := range []string{"UnmarshalJSON", "native", "scalar"} {
				t.Run(fmt.Sprintf("%s/chunk%d/%s", tc.input, chunk, mode), func(t *testing.T) {
					source := &readCounter{in: &chunkReader{data: []byte(tc.input), chunk: chunk}}
					reader := newSizedReader(source, 512)
					var standardValue readerRejectJSON
					var nativeValue readerRejectNative
					var scalarValue bool
					next := func() bool {
						switch mode {
						case "UnmarshalJSON":
							return DecodeNext(reader, standard, &standardValue)
						case "native":
							return DecodeNext(reader, native, &nativeValue)
						default:
							return DecodeNext(reader, scalar, &scalarValue)
						}
					}
					if next() || reader.Err() == nil {
						t.Fatalf("DecodeNext succeeded or returned nil error: %v", reader.Err())
					}
					var syntax *SyntaxError
					if errors.As(reader.Err(), &syntax) {
						t.Fatalf("later input replaced decode error: %v", reader.Err())
					}
					switch mode {
					case "UnmarshalJSON":
						var decodeErr *DecodeError
						if !errors.As(reader.Err(), &decodeErr) || decodeErr.Reason != errReaderRejectedValue.Error() || standardValue.calls != 1 || standardValue.raw != tc.first {
							t.Fatalf("value=%+v error=%v; want one rejected %q", standardValue, reader.Err(), tc.first)
						}
					case "native":
						if !errors.Is(reader.Err(), errReaderRejectedValue) || nativeValue.calls != 1 || nativeValue.raw != tc.first {
							t.Fatalf("value=%+v error=%v; want one rejected %q", nativeValue, reader.Err(), tc.first)
						}
					case "scalar":
						var decodeErr *DecodeError
						if !errors.As(reader.Err(), &decodeErr) {
							t.Fatalf("error=%v; want scalar type mismatch", reader.Err())
						}
					}
					err := reader.Err()
					reads := source.reads.Load()
					if reader.Bytes() != nil || next() || reader.Next() || reader.Err() != err || source.reads.Load() != reads || standardValue.calls > 1 || nativeValue.calls > 1 {
						t.Fatal("decode error was not sticky")
					}
				})
			}
		}
	}
}

func TestDecodeNextRejectsMalformedNumberBeforeCustomDecoder(t *testing.T) {
	standard := mustCompileTestDecoder[readerRejectJSON](t, DecoderOptions{})
	boom := errors.New("source failed")
	for _, tc := range []struct {
		input     string
		sourceErr error
		wantErr   error
	}{
		{"1.e2", nil, nil},
		{"1e+ ", nil, nil},
		{"1.e2", boom, nil},
		{"1e+ ", boom, nil},
		{"1e+", boom, boom},
		{"1.2", boom, boom},
	} {
		t.Run(fmt.Sprintf("%q/sourceError%v", tc.input, tc.sourceErr != nil), func(t *testing.T) {
			reader := newSizedReader(&scriptedReader{steps: []scriptStep{{data: []byte(tc.input), err: tc.sourceErr}}}, 512)
			var value readerRejectJSON
			if DecodeNext(reader, standard, &value) || reader.Err() == nil || value.calls != 0 {
				t.Fatalf("value=%+v error=%v; want rejection before custom decoder", value, reader.Err())
			}
			if tc.wantErr != nil {
				if !errors.Is(reader.Err(), tc.wantErr) {
					t.Fatalf("error=%v; want source error", reader.Err())
				}
			} else {
				var syntax *SyntaxError
				if !errors.As(reader.Err(), &syntax) || strings.Contains(reader.Err().Error(), errReaderRejectedValue.Error()) {
					t.Fatalf("error=%v; want syntax error", reader.Err())
				}
			}
		})
	}
}
