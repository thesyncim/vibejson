package vibejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type dynamicDepthScalar uint32

func (dynamicDepthScalar) dynamicDepthMarker() {}

type dynamicDepthInterface interface {
	dynamicDepthMarker()
}

func TestEncoderDynamicLeavesAtDepthLimit(t *testing.T) {
	number := 7
	var nilPointer *int
	leaves := []any{
		uint32(7), int8(-7), float32(1.5), dynamicDepthScalar(7),
		json.Number("7"), []byte("text"), []byte(nil),
		&number, nilPointer, map[string]any(nil), []any(nil),
	}
	for _, inline := range []bool{false, true} {
		t.Run(fmt.Sprintf("inline=%v", inline), func(t *testing.T) {
			encoder, err := CompileEncoder[any](EncoderOptions{InlineFields: inline})
			if err != nil {
				t.Fatal(err)
			}
			for index, value := range leaves {
				t.Run(fmt.Sprintf("%d/%T", index, value), func(t *testing.T) {
					want, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					// A scalar or null leaf adds no container at this boundary.
					state := encodeState{depth: DefaultMaxDepth, escapeHTML: true}
					if err := encoder.encodeTypedSource(&state, &value); err != nil {
						t.Fatalf("scalar at depth limit: %v", err)
					}
					if !bytes.Equal(state.dst, want) || state.depth != DefaultMaxDepth || state.ptrRun != 0 {
						t.Fatalf("output = %s, depth = %d, pointer run = %d; want %s", state.dst, state.depth, state.ptrRun, want)
					}
				})
			}

			// A non-empty interface reaches the other dynamic dispatch entry.
			typed, err := CompileEncoder[dynamicDepthInterface](EncoderOptions{InlineFields: inline})
			if err != nil {
				t.Fatal(err)
			}
			var value dynamicDepthInterface = dynamicDepthScalar(7)
			state := encodeState{depth: DefaultMaxDepth, escapeHTML: true}
			if err := typed.encodeTypedSource(&state, &value); err != nil || string(state.dst) != "7" {
				t.Fatalf("non-empty interface at depth limit = %s, %v", state.dst, err)
			}
		})
	}
}

func TestEncoderDynamicContainerDepth(t *testing.T) {
	for _, shape := range []string{"slice", "map"} {
		for _, depth := range []int{DefaultMaxDepth, DefaultMaxDepth + 1} {
			var value any = uint32(7)
			for range depth {
				if shape == "slice" {
					value = []any{value}
				} else {
					value = map[string]any{"v": value}
				}
			}
			want, wantErr := json.Marshal(value)
			if (wantErr == nil) != (depth <= DefaultMaxDepth) {
				t.Fatalf("encoding/json depth %d: %v", depth, wantErr)
			}
			for _, inline := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/depth=%d/inline=%v", shape, depth, inline), func(t *testing.T) {
					encoder, err := CompileEncoder[any](EncoderOptions{InlineFields: inline})
					if err != nil {
						t.Fatal(err)
					}
					got, err := encoder.AppendJSON([]byte("prefix:"), &value)
					if (err == nil) != (wantErr == nil) {
						t.Fatalf("vibejson error = %v, encoding/json error = %v", err, wantErr)
					}
					if err != nil {
						if !strings.Contains(err.Error(), "maximum nesting depth") || string(got) != "prefix:" {
							t.Fatalf("depth error = %v, destination = %q", err, got)
						}
						return
					}
					if !bytes.Equal(got[7:], want) {
						t.Fatal("encoded output differs from encoding/json")
					}
					if err := Validate(got[7:]); err != nil {
						t.Fatalf("encoded output exceeds validation depth: %v", err)
					}
				})
			}
		}
	}
}

func TestEncoderDynamicCyclesTerminate(t *testing.T) {
	var pointerCycle any
	pointerCycle = &pointerCycle
	mapCycle := map[string]any{}
	mapCycle["self"] = mapCycle
	sliceCycle := make([]any, 1)
	sliceCycle[0] = sliceCycle
	for _, inline := range []bool{false, true} {
		for name, value := range map[string]any{
			"pointer-interface": pointerCycle,
			"map-interface":     mapCycle,
			"slice-interface":   sliceCycle,
		} {
			t.Run(fmt.Sprintf("%s/inline=%v", name, inline), func(t *testing.T) {
				encoder, err := CompileEncoder[any](EncoderOptions{InlineFields: inline})
				if err != nil {
					t.Fatal(err)
				}
				got, err := encoder.AppendJSON([]byte("prefix:"), &value)
				if err == nil || !strings.Contains(err.Error(), "maximum nesting depth") || string(got) != "prefix:" {
					t.Fatalf("cycle error = %v, destination length = %d", err, len(got))
				}
				value = uint32(7)
				got, err = encoder.AppendJSON(nil, &value)
				if err != nil || string(got) != "7" {
					t.Fatalf("encode after cycle = %s, %v", got, err)
				}
			})
		}
	}
}
