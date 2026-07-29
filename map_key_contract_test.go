package vibejson

import "testing"

type decodeOnlyMapKey struct {
	Value string
}

func (k *decodeOnlyMapKey) UnmarshalText(text []byte) error {
	k.Value = string(text)
	return nil
}

type encodeOnlyMapKey struct {
	Value string
}

func (k encodeOnlyMapKey) MarshalText() ([]byte, error) {
	return []byte(k.Value), nil
}

func TestMapKeyCompileChecksDirectionSpecificTextMethod(t *testing.T) {
	t.Run("encoder rejects decode-only key", func(t *testing.T) {
		if _, err := CompileEncoder[map[decodeOnlyMapKey]int](EncoderOptions{}); err == nil {
			t.Fatal("CompileEncoder accepted a key with TextUnmarshaler but no TextMarshaler")
		}
	})

	t.Run("decoder rejects encode-only key", func(t *testing.T) {
		if _, err := CompileDecoder[map[encodeOnlyMapKey]int](DecoderOptions{}); err == nil {
			t.Fatal("CompileDecoder accepted a key with TextMarshaler but no TextUnmarshaler")
		}
	})
}
