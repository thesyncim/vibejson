package vibejson

import (
	"errors"
	"testing"
)

type decodeErrorTextKey string

func (*decodeErrorTextKey) UnmarshalText([]byte) error {
	return errors.New("rejected text key")
}

type decodeErrorJSONValue int

func (*decodeErrorJSONValue) UnmarshalJSON([]byte) error {
	return errors.New("rejected JSON value")
}

type decodeErrorNativeValue int

func (*decodeErrorNativeValue) UnmarshalVibeJSON(cursor DecodeCursor) (DecodeCursor, error) {
	if err := cursor.Skip(); err != nil {
		return cursor, err
	}
	return cursor, &DecodeError{Reason: "rejected native value"}
}

type decodeErrorInlineValues struct {
	Extra map[string]int `json:",inline"`
}

func TestDecodeErrorPathOwnsInputKey(t *testing.T) {
	for _, zeroCopy := range []bool{false, true} {
		name := "owned"
		if zeroCopy {
			name = "zero-copy"
		}
		t.Run(name, func(t *testing.T) {
			opts := DecoderOptions{ZeroCopy: zeroCopy}
			t.Run("map-value", func(t *testing.T) {
				checkDecodeErrorKeyOwnership[map[string]int](t, `{"private":true}`, "private", opts)
			})
			t.Run("escaped-key", func(t *testing.T) {
				checkDecodeErrorKeyOwnership[map[string]int](t, `{"pri\u0076ate":true}`, "private", opts)
			})
			t.Run("integer-key", func(t *testing.T) {
				checkDecodeErrorKeyOwnership[map[int]int](t, `{"private":1}`, "private", opts)
			})
			t.Run("text-key", func(t *testing.T) {
				checkDecodeErrorKeyOwnership[map[decodeErrorTextKey]int](t, `{"private":1}`, "private", opts)
			})
			t.Run("JSON-method", func(t *testing.T) {
				checkDecodeErrorKeyOwnership[map[string]decodeErrorJSONValue](t, `{"private":1}`, "private", opts)
			})
			t.Run("native-method", func(t *testing.T) {
				checkDecodeErrorKeyOwnership[map[string]decodeErrorNativeValue](t, `{"private":1}`, "private", opts)
			})
			t.Run("inline", func(t *testing.T) {
				opts.InlineFields = true
				checkDecodeErrorKeyOwnership[decodeErrorInlineValues](t, `{"private":true}`, "private", opts)
			})
		})
	}
}

func checkDecodeErrorKeyOwnership[T any](t *testing.T, input, wantPath string, opts DecoderOptions) {
	t.Helper()
	decoder := mustCompileTestDecoder[T](t, opts)
	methods := []string{"Decode", "DecodePrefix"}
	if !opts.InlineFields {
		methods = append(methods, "Unmarshal")
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			src := []byte(input)
			var dst T
			var err error
			switch method {
			case "Decode":
				err = decoder.Decode(src, &dst)
			case "DecodePrefix":
				_, err = decoder.DecodePrefix(src, &dst)
			case "Unmarshal":
				err = Unmarshal(src, &dst)
			}
			var decodeErr *DecodeError
			if !errors.As(err, &decodeErr) || decodeErr.Path != wantPath {
				t.Fatalf("error = %v, want DecodeError path %q", err, wantPath)
			}
			message := err.Error()
			for i := range src {
				src[i] = '!'
			}
			if decodeErr.Path != wantPath || err.Error() != message {
				t.Fatalf("input reuse changed error: got %v, want %s", err, message)
			}
		})
	}
}
