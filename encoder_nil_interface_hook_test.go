package vibejson

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

type nilInterfaceJSONHook struct{}

func (value *nilInterfaceJSONHook) MarshalJSON() ([]byte, error) {
	if value == nil {
		return []byte(`"<nil-json>"`), nil
	}
	return []byte(`"<json>"`), nil
}

type nilInterfaceTextHook struct{}

func (value *nilInterfaceTextHook) MarshalText() ([]byte, error) {
	if value == nil {
		return []byte("<nil-text>"), nil
	}
	return []byte("<text>"), nil
}

type nilInterfaceDualHook struct{}

func (*nilInterfaceDualHook) MarshalJSON() ([]byte, error) { return []byte(`"<dual-json>"`), nil }
func (*nilInterfaceDualHook) MarshalText() ([]byte, error) { return []byte("<dual-text>"), nil }

type nilInterfaceHookDocument struct {
	JSON     json.Marshaler
	Text     encoding.TextMarshaler
	DualJSON json.Marshaler
	DualText encoding.TextMarshaler
	AnyJSON  any
	AnyText  any
	AnyDual  any
	Direct   *nilInterfaceJSONHook
}

func checkNilInterfaceHookParity[T any](t *testing.T, value T) {
	t.Helper()
	for _, opts := range []EncoderOptions{
		{}, {InlineFields: true}, {DisableHTMLEscaping: true},
		{InlineFields: true, DisableHTMLEscaping: true},
	} {
		t.Run(fmt.Sprintf("inline=%v/escapeHTML=%v", opts.InlineFields, !opts.DisableHTMLEscaping), func(t *testing.T) {
			encoder := mustCompileTestEncoder[T](t, opts)
			var reference bytes.Buffer
			stdlib := json.NewEncoder(&reference)
			stdlib.SetEscapeHTML(!opts.DisableHTMLEscaping)
			if err := stdlib.Encode(&value); err != nil {
				t.Fatal(err)
			}
			want := bytes.TrimSuffix(reference.Bytes(), []byte{'\n'})
			for attempt := 0; attempt < 2; attempt++ {
				got, err := encoder.AppendJSON(nil, &value)
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("attempt %d = %s, %v; stdlib = %s", attempt, got, err, want)
				}
			}
		})
	}
}

func TestNilPointerInMarshalerInterface(t *testing.T) {
	t.Run("nil interfaces", func(t *testing.T) {
		checkNilInterfaceHookParity(t, nilInterfaceHookDocument{})
	})
	t.Run("nil concrete pointers", func(t *testing.T) {
		checkNilInterfaceHookParity(t, nilInterfaceHookDocument{
			JSON: (*nilInterfaceJSONHook)(nil), Text: (*nilInterfaceTextHook)(nil),
			DualJSON: (*nilInterfaceDualHook)(nil), DualText: (*nilInterfaceDualHook)(nil),
			AnyJSON: (*nilInterfaceJSONHook)(nil), AnyText: (*nilInterfaceTextHook)(nil), AnyDual: (*nilInterfaceDualHook)(nil),
		})
	})
	t.Run("non-nil concrete pointers", func(t *testing.T) {
		checkNilInterfaceHookParity(t, nilInterfaceHookDocument{
			JSON: &nilInterfaceJSONHook{}, Text: &nilInterfaceTextHook{},
			DualJSON: &nilInterfaceDualHook{}, DualText: &nilInterfaceDualHook{},
			AnyJSON: &nilInterfaceJSONHook{}, AnyText: &nilInterfaceTextHook{}, AnyDual: &nilInterfaceDualHook{},
			Direct: &nilInterfaceJSONHook{},
		})
	})
	t.Run("root interface", func(t *testing.T) {
		checkNilInterfaceHookParity[json.Marshaler](t, (*nilInterfaceJSONHook)(nil))
	})
	t.Run("map interface value", func(t *testing.T) {
		checkNilInterfaceHookParity(t, map[string]json.Marshaler{"key": (*nilInterfaceJSONHook)(nil)})
	})
	t.Run("slice interface value", func(t *testing.T) {
		checkNilInterfaceHookParity(t, []encoding.TextMarshaler{(*nilInterfaceTextHook)(nil)})
	})
}

type nilInterfaceErrorJSONHook struct{}

func (*nilInterfaceErrorJSONHook) MarshalJSON() ([]byte, error) {
	return nil, errors.New("nil JSON hook error")
}

type nilInterfaceInvalidJSONHook struct{}

func (*nilInterfaceInvalidJSONHook) MarshalJSON() ([]byte, error) { return []byte(`{`), nil }

type nilInterfaceErrorTextHook struct{}

func (*nilInterfaceErrorTextHook) MarshalText() ([]byte, error) {
	return nil, errors.New("nil text hook error")
}

func TestNilInterfaceHookErrorsPreserveDestination(t *testing.T) {
	for _, value := range []nilInterfaceHookDocument{
		{JSON: (*nilInterfaceErrorJSONHook)(nil)},
		{JSON: (*nilInterfaceInvalidJSONHook)(nil)},
		{Text: (*nilInterfaceErrorTextHook)(nil)},
	} {
		if _, err := json.Marshal(&value); err == nil {
			t.Fatal("stdlib accepted the invalid hook")
		}
		for _, opts := range []EncoderOptions{{}, {InlineFields: true}} {
			encoder := mustCompileTestEncoder[nilInterfaceHookDocument](t, opts)
			dst := append(make([]byte, 0, 128), "prefix"...)
			got, err := encoder.AppendJSON(dst, &value)
			if err == nil || string(got) != "prefix" || string(dst) != "prefix" {
				t.Fatalf("inline=%v got %q, %v; want unchanged prefix and error", opts.InlineFields, got, err)
			}
		}
	}
}

type nilInterfaceNativeHook struct{}

func (*nilInterfaceNativeHook) MarshalVibeJSON(out TrustedAppender) TrustedAppender {
	return out.String("native")
}

func TestNilInterfaceNativeHookDispatch(t *testing.T) {
	for _, opts := range []EncoderOptions{{}, {InlineFields: true}} {
		encoder := mustCompileTestEncoder[MarshalerSimd](t, opts)
		for _, tc := range []struct {
			value MarshalerSimd
			want  string
		}{
			{nil, "null"},
			{(*nilInterfaceNativeHook)(nil), "null"},
			{&nilInterfaceNativeHook{}, `"native"`},
		} {
			got, err := encoder.AppendJSON(nil, &tc.value)
			if err != nil || string(got) != tc.want {
				t.Errorf("native interface = %s, %v; want %s", got, err, tc.want)
			}
		}
	}
}
