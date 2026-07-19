package simdjson

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

type hookIntegrityValid int

func (v hookIntegrityValid) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	return w.Int(int64(v))
}

type hookIntegrityInvalid struct{}

func (hookIntegrityInvalid) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	return w.RawUnchecked(`{"unterminated"`)
}

func TestHookIntegrityValidationIsSpanLocal(t *testing.T) {
	type document struct {
		Head int                `json:"head"`
		Hook hookIntegrityValid `json:"hook"`
		Tail int                `json:"tail"`
	}
	enc, err := CompileEncoder[document](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value := document{Head: 1, Hook: 2, Tail: 3}
	got, err := enc.AppendJSON(nil, &value)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"head":1,"hook":2,"tail":3}`; string(got) != want {
		t.Fatalf("AppendJSON = %s, want %s", got, want)
	}
}

func TestHookIntegrityValidationMode(t *testing.T) {
	type document struct {
		Hook hookIntegrityInvalid `json:"hook"`
	}
	enc, err := CompileEncoder[document](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value := document{}
	got, err := enc.AppendJSON(nil, &value)
	if validateSimdHookOutput {
		if err == nil || !strings.Contains(err.Error(), "MarshalSimdJSON produced invalid JSON") {
			t.Fatalf("debug validation = %q, %v, want invalid-hook error", got, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("production unchecked hook returned error: %v", err)
	}
	if Valid(got) {
		t.Fatalf("unchecked malformed hook unexpectedly produced valid JSON: %s", got)
	}
}

type hookIntegrityRaw []byte

func (raw hookIntegrityRaw) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	return w.RawBytesUnchecked(raw)
}

func FuzzHookIntegritySpan(f *testing.F) {
	f.Add([]byte("null"))
	f.Add([]byte(`{"valid":[1,2,3]}`))
	f.Add([]byte(`{"unterminated"`))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 0})
	f.Add([]byte{2, 2, 1, 0, 1, 0})

	type document struct {
		Hook hookIntegrityRaw `json:"hook"`
	}
	enc, err := CompileEncoder[document](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	recoveryEnc, err := CompileEncoder[hookRecoveryDocument](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) <= 64 {
			checkHookPlanRecoverySequence(t, raw, recoveryEnc)
		}
		if len(raw) > 1<<12 {
			t.Skip()
		}
		value := document{Hook: raw}
		got, err := enc.AppendJSON(nil, &value)
		if validateSimdHookOutput {
			if valid := Valid(raw); valid != (err == nil) {
				t.Fatalf("checked hook acceptance for %q = %v, want valid=%v", raw, err, valid)
			}
			if err == nil && !Valid(got) {
				t.Fatalf("checked hook produced invalid document: %q", got)
			}
			return
		}
		if err != nil {
			t.Fatalf("unchecked hook returned error for %q: %v", raw, err)
		}
		want := append([]byte(`{"hook":`), raw...)
		want = append(want, '}')
		if !bytes.Equal(got, want) {
			t.Fatalf("unchecked hook = %q, want %q", got, want)
		}
	})
}

type hookRecoveryValue struct {
	Mode byte
	N    int64
}

type hookRecoveryDocument struct {
	Values map[string]int    `json:"values"`
	Hook   hookRecoveryValue `json:"hook"`
}

func (v hookRecoveryValue) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	switch v.Mode % 3 {
	case 0:
		return w.Int(v.N)
	case 1:
		return w.Float64(math.NaN())
	default:
		panic("hook recovery fuzz panic")
	}
}

func checkHookPlanRecoverySequence(t *testing.T, operations []byte, enc Encoder[hookRecoveryDocument]) {
	t.Helper()
	buffer := make([]byte, 0, 128)
	value := hookRecoveryDocument{Values: map[string]int{"stable": 1}}
	encode := func() (out []byte, err error, panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		out, err = enc.AppendJSON(buffer[:0], &value)
		return out, err, false
	}
	for step, operation := range operations {
		value.Hook = hookRecoveryValue{Mode: operation, N: int64(step)}
		out, err, panicked := encode()
		switch operation % 3 {
		case 0:
			if panicked || err != nil || !Valid(out) {
				t.Fatalf("step %d success = %q, %v, panic=%v", step, out, err, panicked)
			}
			buffer = out
		case 1:
			if panicked || err == nil {
				t.Fatalf("step %d unsupported value = %v, panic=%v", step, err, panicked)
			}
		default:
			if !panicked {
				t.Fatalf("step %d hook panic was not propagated", step)
			}
		}
	}
	value.Hook = hookRecoveryValue{N: 99}
	out, err, panicked := encode()
	if panicked || err != nil || !bytes.Contains(out, []byte(`"hook":99`)) || !Valid(out) {
		t.Fatalf("final recovery = %q, %v, panic=%v", out, err, panicked)
	}
}
