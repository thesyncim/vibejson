package vibejson

import (
	"reflect"
	"testing"
)

type nativeEncodeByte uint8

func (value *nativeEncodeByte) MarshalVibeJSON(out TrustedAppender) TrustedAppender {
	return out.Uint(uint64(*value) + 10)
}

type nativeDecodeByte uint8

func (value *nativeDecodeByte) UnmarshalVibeJSON(cursor DecodeCursor) (DecodeCursor, error) {
	var decoded uint8
	if err := cursor.Uint8(&decoded); err != nil {
		return cursor, err
	}
	*value = nativeDecodeByte(decoded + 10)
	return cursor, nil
}

func TestNativeByteElementHooksAreDirectionSpecific(t *testing.T) {
	t.Run("encode hook changes array form only", func(t *testing.T) {
		encoder, err := CompileEncoder[[]nativeEncodeByte](EncoderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		value := []nativeEncodeByte{1, 2}
		out, err := encoder.AppendJSON(nil, &value)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `[11,12]` {
			t.Fatalf("native byte encode = %s, want [11,12]", out)
		}

		decoder := mustCompileTestDecoder[[]nativeEncodeByte](t, DecoderOptions{})
		var decoded []nativeEncodeByte
		if err := decoder.Decode([]byte(`"AQI="`), &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, []nativeEncodeByte{1, 2}) {
			t.Fatalf("opposite-direction base64 decode = %v", decoded)
		}
	})

	t.Run("decode hook changes array form only", func(t *testing.T) {
		decoder := mustCompileTestDecoder[[]nativeDecodeByte](t, DecoderOptions{})
		var decoded []nativeDecodeByte
		if err := decoder.Decode([]byte(`[1,2]`), &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, []nativeDecodeByte{11, 12}) {
			t.Fatalf("native byte array decode = %v, want [11 12]", decoded)
		}

		if err := decoder.Decode([]byte(`"AQI="`), &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, []nativeDecodeByte{1, 2}) {
			t.Fatalf("native byte base64 decode = %v, want [1 2]", decoded)
		}

		encoder, err := CompileEncoder[[]nativeDecodeByte](EncoderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		value := []nativeDecodeByte{1, 2}
		out, err := encoder.AppendJSON(nil, &value)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `"AQI="` {
			t.Fatalf("opposite-direction base64 encode = %s", out)
		}
	})
}
