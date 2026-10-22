package vibejson

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestReaderConcatenatedNumbersRespectPerValueLimit(t *testing.T) {
	decoder := mustCompileTestDecoder[any](t, DecoderOptions{})
	for _, tc := range []struct {
		name  string
		input string
		limit int
		want  []string
	}{
		{"zero", "01", 1, []string{"0", "1"}},
		{"negative-zero", "-01", 2, []string{"-0", "1"}},
		{"integer", "0123", 3, []string{"0", "123"}},
		{"repeated-zero", "00123", 3, []string{"0", "0", "123"}},
		{"decimal", "00.1", 3, []string{"0", "0.1"}},
		{"exponent", "01e2", 3, []string{"0", "1e2"}},
		{"negative-decimal", "-0.1", 4, []string{"-0.1"}},
		{"negative-exponent", "-0e-1", 5, []string{"-0e-1"}},
		{"buffer-boundary", strings.Repeat("0", 513), 1, strings.Split(strings.Repeat("0", 513), "")},
	} {
		for _, chunk := range []int{1, 2, 512} {
			for _, decode := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/chunk%d/decode%v", tc.name, chunk, decode), func(t *testing.T) {
					reader := newConfiguredReader(&chunkReader{data: []byte(tc.input), chunk: chunk}, 512, tc.limit)
					var got []string
					for {
						var value any
						var ok bool
						if decode {
							ok = DecodeNext(reader, decoder, &value)
						} else {
							ok = reader.Next()
						}
						if !ok {
							break
						}
						got = append(got, string(reader.Bytes()))
					}
					if !reflect.DeepEqual(got, tc.want) || reader.Err() != nil {
						t.Fatalf("values=%q error=%v; want %q without error", got, reader.Err(), tc.want)
					}
				})
			}
		}
	}
}

func TestReaderNumberPrefixBeforeError(t *testing.T) {
	decoder := mustCompileTestDecoder[any](t, DecoderOptions{})
	boom := errors.New("source failed")
	for _, tc := range []struct {
		input     string
		want      string
		syntaxErr bool
	}{
		{"01", "0", false},
		{"-01", "-0", false},
		{"1.2.3", "1.2", true},
		{"1e2e3", "1e2", true},
	} {
		for _, sourceErr := range []error{nil, boom} {
			for _, split := range []bool{false, true} {
				for _, decode := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/sourceError%v/split%v/decode%v", tc.input, sourceErr != nil, split, decode), func(t *testing.T) {
						steps := []scriptStep{{data: []byte(tc.input), err: sourceErr}}
						if split {
							steps = nil
							for i := range tc.input {
								steps = append(steps, scriptStep{data: []byte(tc.input[i : i+1])})
							}
							steps[len(steps)-1].err = sourceErr
						}
						reader := newConfiguredReader(&scriptedReader{steps: steps}, 512, len(tc.want))
						var value any = 99
						next := func() bool {
							if decode {
								return DecodeNext(reader, decoder, &value)
							}
							return reader.Next()
						}
						if !next() || string(reader.Bytes()) != tc.want {
							t.Fatalf("first value=%q error=%v; want %s before error", reader.Bytes(), reader.Err(), tc.want)
						}
						if !tc.syntaxErr && sourceErr == nil {
							if !next() || string(reader.Bytes()) != "1" || next() || reader.Err() != nil {
								t.Fatalf("remaining number not delivered cleanly: %v", reader.Err())
							}
							return
						}
						if next() || reader.Err() == nil {
							t.Fatalf("error=%v; want trailing input error", reader.Err())
						}
						if tc.syntaxErr {
							var syntax *SyntaxError
							if !errors.As(reader.Err(), &syntax) {
								t.Fatalf("error=%v; want syntax error", reader.Err())
							}
						} else if !errors.Is(reader.Err(), boom) {
							t.Fatalf("error=%v; want source failure at unconfirmed final number", reader.Err())
						}
					})
				}
			}
		}
	}
}

func TestDecodeNextOversizedNumberLeavesDestination(t *testing.T) {
	decoder := mustCompileTestDecoder[int](t, DecoderOptions{})
	boom := errors.New("source failed")
	for _, input := range []string{"123", "123 "} {
		for _, sourceErr := range []error{nil, boom} {
			for _, split := range []bool{false, true} {
				t.Run(fmt.Sprintf("%q/sourceError%v/split%v", input, sourceErr != nil, split), func(t *testing.T) {
					steps := []scriptStep{{data: []byte(input), err: sourceErr}}
					if split {
						steps = nil
						for i := range input {
							steps = append(steps, scriptStep{data: []byte(input[i : i+1])})
						}
						steps[len(steps)-1].err = sourceErr
					}
					value := 99
					reader := newConfiguredReader(&scriptedReader{steps: steps}, 512, 2)
					if DecodeNext(reader, decoder, &value) || reader.Err() == nil || value != 99 {
						t.Fatalf("value=%d error=%v; want error before destination changes", value, reader.Err())
					}
					if input == "123" && sourceErr != nil {
						if !errors.Is(reader.Err(), boom) {
							t.Fatalf("error=%v; want source error at unconfirmed number", reader.Err())
						}
					} else if !strings.Contains(reader.Err().Error(), "limit") {
						t.Fatalf("error=%v; want size-limit error", reader.Err())
					}
				})
			}
		}
	}
}
