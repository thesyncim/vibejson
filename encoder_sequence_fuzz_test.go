package slopjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"testing"
)

var errEncoderSequence = errors.New("encoder sequence marshaler failure")

type encoderSequenceMarshaler struct {
	Mode byte
	Text string
}

func (m encoderSequenceMarshaler) MarshalJSON() ([]byte, error) {
	switch m.Mode & 3 {
	case 0:
		return json.Marshal(m.Text)
	case 1:
		text, err := json.Marshal(m.Text)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(text)+18)
		out = append(out, " { \"wrapped\": "...)
		out = append(out, text...)
		out = append(out, " } "...)
		return out, nil
	case 2:
		return []byte("{"), nil
	default:
		return nil, errEncoderSequence
	}
}

type encoderSequenceNode struct {
	Name string               `json:"name"`
	Next *encoderSequenceNode `json:"next,omitempty"`
}

type encoderSequenceDoc struct {
	Ints    map[string]int                      `json:"ints"`
	Strings map[string]string                   `json:"strings"`
	Nodes   map[string]*encoderSequenceNode     `json:"nodes"`
	Customs map[string]encoderSequenceMarshaler `json:"customs"`
	Custom  encoderSequenceMarshaler            `json:"custom"`
}

// FuzzEncoderScratchOperationSequence checks the stateful contract that a
// compiled encoder remains equivalent to encoding/json across arbitrary
// sequences of successes, marshaler failures, invalid marshaler output, map
// replacement and churn, stack growth, GC, and pooled scratch reuse.
func FuzzEncoderScratchOperationSequence(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 9, 19, 3, 35, 6, 7, 0, 17})
	f.Add([]byte{255, 31, 2, 10, 26, 42, 58, 74, 90})
	// Former FuzzEncoderScratchRetentionSequence seeds.
	f.Add([]byte{1, 2, 31, 1, 0, 3})
	f.Add([]byte{31, 0, 1})

	enc, err := CompileEncoder[encoderSequenceDoc](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	retentionEnc, err := CompileEncoder[map[int]uint64](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	retentionScratch, retentionPool := dedicatedEncoderScratch(&retentionEnc)

	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 256 {
			return
		}
		if len(operations) <= 64 {
			checkEncoderScratchRetentionSequence(t, retentionEnc, retentionScratch, retentionPool, operations)
		}

		v := encoderSequenceDoc{
			Ints:    map[string]int{"initial": 1},
			Strings: map[string]string{"initial": "value"},
			Nodes: map[string]*encoderSequenceNode{
				"initial": {Name: "root"},
			},
			Customs: map[string]encoderSequenceMarshaler{
				"initial": {Text: "map-initial"},
			},
			Custom: encoderSequenceMarshaler{Text: "initial"},
		}
		buffer := make([]byte, 0, 512)
		stackSink := 0
		gcRuns := 0

		check := func(step int) {
			t.Helper()
			want, wantErr := json.Marshal(&v)
			dst := buffer[:0]
			got, gotErr := enc.AppendJSON(dst, &v)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("step %d acceptance differs: encoder=%v stdlib=%v", step, gotErr, wantErr)
			}
			if gotErr == nil {
				if !bytes.Equal(got, want) {
					t.Fatalf("step %d encoding differs:\n encoder %s\n stdlib  %s", step, got, want)
				}
				buffer = got
			} else {
				// An error must not prevent the same backing buffer or pooled
				// scratch object from being reused by the next operation.
				buffer = dst
			}
		}

		check(-1)
		for step, operation := range operations {
			key := "k" + strconv.Itoa(int(operation>>4))
			switch operation & 7 {
			case 0:
				v.Ints = nil
				v.Strings = nil
				v.Nodes = nil
				v.Customs = nil
			case 1:
				if v.Ints == nil {
					v.Ints = make(map[string]int)
				}
				if v.Strings == nil {
					v.Strings = make(map[string]string)
				}
				if v.Customs == nil {
					v.Customs = make(map[string]encoderSequenceMarshaler)
				}
				v.Ints[key] = int(int8(operation))
				v.Strings[key] = string([]byte{'v', operation, '<', '&', '>'})
				v.Customs[key] = encoderSequenceMarshaler{Text: "map-" + key}
			case 2:
				delete(v.Ints, key)
				delete(v.Strings, key)
				delete(v.Nodes, key)
				delete(v.Customs, key)
			case 3:
				v.Custom.Mode = operation >> 3
				v.Custom.Text = string([]byte{'m', operation, '\n', 0xff})
				if v.Customs == nil {
					v.Customs = make(map[string]encoderSequenceMarshaler)
				}
				v.Customs[key] = encoderSequenceMarshaler{
					Mode: operation >> 3,
					Text: "nested-" + key,
				}
			case 4:
				v.Ints = map[string]int{}
				v.Strings = map[string]string{}
				v.Nodes = map[string]*encoderSequenceNode{}
				v.Customs = map[string]encoderSequenceMarshaler{}
			case 5:
				if v.Nodes == nil {
					v.Nodes = make(map[string]*encoderSequenceNode)
				}
				v.Nodes[key] = &encoderSequenceNode{
					Name: key,
					Next: &encoderSequenceNode{Name: string([]byte{'n', operation})},
				}
			case 6:
				v.Ints = map[string]int{key: step, "other": -step}
				v.Strings = map[string]string{key: v.Custom.Text}
				v.Customs = map[string]encoderSequenceMarshaler{
					key: {Mode: operation >> 3, Text: "replacement-" + key},
				}
			case 7:
				if gcRuns < 2 {
					stackSink ^= forceStackMovement(16+int(operation&15), step+1)
					runtime.GC()
					gcRuns++
				}
			}
			check(step)
		}

		// Finish with a known-successful operation so every error sequence
		// also proves that all scratch state was released and reset.
		v.Custom.Mode = 0
		v.Custom.Text = "recovered"
		for key, value := range v.Customs {
			value.Mode = 0
			v.Customs[key] = value
		}
		check(len(operations))
		runtime.KeepAlive(stackSink)
	})
}
