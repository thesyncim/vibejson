package vibejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
)

type dynamicJSONHookMap map[string]int

func (value dynamicJSONHookMap) MarshalJSON() ([]byte, error) {
	if value["fail"] != 0 {
		return nil, errors.New("map hook failed")
	}
	return []byte(`"<json-map>"`), nil
}

type dynamicTextHookMap map[string]int

func (value dynamicTextHookMap) MarshalText() ([]byte, error) {
	if value["fail"] != 0 {
		return nil, errors.New("map hook failed")
	}
	return []byte("<text-map>"), nil
}

type dynamicNativeHookMap map[string]int

func (value dynamicNativeHookMap) MarshalVibeJSON(out TrustedAppender) TrustedAppender {
	if value["fail"] != 0 {
		return out.Float64(math.NaN())
	}
	return out.String("<native-map>")
}

type dynamicPointerJSONHookMap map[string]int

func (*dynamicPointerJSONHookMap) MarshalJSON() ([]byte, error) {
	return []byte(`"<pointer-json-map>"`), nil
}

type dynamicPointerTextHookMap map[string]int

func (*dynamicPointerTextHookMap) MarshalText() ([]byte, error) {
	return []byte("<pointer-text-map>"), nil
}

type dynamicPointerNativeHookMap map[string]int

func (*dynamicPointerNativeHookMap) MarshalVibeJSON(out TrustedAppender) TrustedAppender {
	return out.String("<pointer-native-map>")
}

func TestDynamicMapHooks(t *testing.T) {
	jsonPointer := dynamicPointerJSONHookMap{"<key>": 1}
	textPointer := dynamicPointerTextHookMap{"<key>": 1}
	nativePointer := dynamicPointerNativeHookMap{"<key>": 1}
	for _, opts := range []EncoderOptions{
		{}, {InlineFields: true}, {DisableHTMLEscaping: true},
		{InlineFields: true, DisableHTMLEscaping: true},
	} {
		t.Run(fmt.Sprintf("inline=%v/escapeHTML=%v", opts.InlineFields, !opts.DisableHTMLEscaping), func(t *testing.T) {
			encoder := mustCompileTestEncoder[any](t, opts)
			for _, tc := range []struct {
				name  string
				value any
				// Native hooks use an equivalent ordinary value as their oracle.
				oracle any
			}{
				{"json value", dynamicJSONHookMap{"key": 1}, nil},
				{"json nil", dynamicJSONHookMap(nil), nil},
				{"text value", dynamicTextHookMap{"key": 1}, nil},
				{"text nil", dynamicTextHookMap(nil), nil},
				{"native value", dynamicNativeHookMap{"key": 1}, "<native-map>"},
				{"native nil", dynamicNativeHookMap(nil), "<native-map>"},
				{"json pointer-only value", jsonPointer, nil},
				{"json pointer-only pointer", &jsonPointer, nil},
				{"text pointer-only value", textPointer, nil},
				{"text pointer-only pointer", &textPointer, nil},
				{"native pointer-only value", nativePointer, map[string]int(nativePointer)},
				{"native pointer-only pointer", &nativePointer, "<pointer-native-map>"},
				{"plain map", map[string]int{"<key>": 1}, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					oracle := tc.oracle
					if oracle == nil {
						oracle = tc.value
					}
					var reference bytes.Buffer
					stdlib := json.NewEncoder(&reference)
					stdlib.SetEscapeHTML(!opts.DisableHTMLEscaping)
					if err := stdlib.Encode(oracle); err != nil {
						t.Fatal(err)
					}
					want := bytes.TrimSuffix(reference.Bytes(), []byte{'\n'})
					for attempt := 0; attempt < 2; attempt++ {
						got, err := encoder.AppendJSON(nil, &tc.value)
						if err != nil || !bytes.Equal(got, want) {
							t.Errorf("attempt %d = %s, %v; want %s", attempt, got, err, want)
						}
					}
				})
			}
		})
	}
}

func TestDynamicMapHookErrorPreservesDestination(t *testing.T) {
	for _, opts := range []EncoderOptions{{}, {InlineFields: true}} {
		encoder := mustCompileTestEncoder[any](t, opts)
		for _, value := range []any{
			dynamicJSONHookMap{"fail": 1}, dynamicTextHookMap{"fail": 1}, dynamicNativeHookMap{"fail": 1},
		} {
			dst := append(make([]byte, 0, 128), "prefix"...)
			got, err := encoder.AppendJSON(dst, &value)
			if err == nil || string(got) != "prefix" || string(dst) != "prefix" {
				t.Fatalf("inline=%v %T = %q, %v; want preserved prefix and error", opts.InlineFields, value, got, err)
			}
		}
		var value any = dynamicJSONHookMap{"key": 1}
		got, err := encoder.AppendJSON(nil, &value)
		if err != nil || string(got) != `"\u003cjson-map\u003e"` {
			t.Fatalf("after hook errors = %s, %v", got, err)
		}
	}
}

func TestDynamicMapHookNonEmptyInterface(t *testing.T) {
	for _, opts := range []EncoderOptions{{}, {InlineFields: true}} {
		encoder := mustCompileTestEncoder[json.Marshaler](t, opts)
		var value json.Marshaler = dynamicJSONHookMap{"key": 1}
		got, err := encoder.AppendJSON(nil, &value)
		want, wantErr := json.Marshal(value)
		if err != nil || wantErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("non-empty interface = %s, %v; stdlib = %s, %v", got, err, want, wantErr)
		}
	}
}
