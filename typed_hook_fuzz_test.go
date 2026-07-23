package slopjson

import (
	"bytes"
	stdjson "encoding/json"
	"math"
	"strings"
	"testing"
)

// FuzzHookContracts owns the hook differential and integrity campaigns. Even
// modes compare hook decoding and encoding with the reflection and
// encoding/json oracles. Odd modes validate raw hook spans and verify that a
// failed or panicking hook does not corrupt later encoder calls. Keeping both
// contracts in one campaign preserves their seeds without duplicating fuzz
// process startup and corpus maintenance.
//
// Run at least 60s:
//
//	GOEXPERIMENT=simd gotip test -run x -fuzz FuzzHookContracts -fuzztime 60s ./
func FuzzHookContracts(f *testing.F) {
	for _, doc := range adversarialHookDocs() {
		f.Add(byte(0), []byte(doc), true)
		f.Add(byte(0), []byte(doc), false)
	}
	f.Add(byte(0), []byte(`{}`), true)
	f.Add(byte(0), []byte(`null`), true)
	f.Add(byte(0), []byte(`{"ADDRESS":{"STREET":"x"},"ID":9}`), false)
	f.Add(byte(0), []byte(`{"id":1,"id":2,"id":3}`), true)
	f.Add(byte(0), []byte(`{"ſcore":2.5}`), false)
	f.Add(byte(0), []byte(`{"nic\u212Aname":"kelvin-fold"}`), false)
	for _, raw := range [][]byte{
		[]byte("null"),
		[]byte(`{"valid":[1,2,3]}`),
		[]byte(`{"unterminated"`),
		{},
		{0, 1, 2, 0},
		{2, 2, 1, 0, 1, 0},
	} {
		f.Add(byte(1), raw, false)
	}

	build := func(cs bool) (Decoder[hookPerson], Decoder[hookPersonPlain]) {
		opts := DecoderOptions{CaseSensitive: cs}
		hd, err := CompileDecoder[hookPerson](opts)
		if err != nil {
			f.Fatal(err)
		}
		pd, err := CompileDecoder[hookPersonPlain](opts)
		if err != nil {
			f.Fatal(err)
		}
		return hd, pd
	}
	hookDecCS, plainDecCS := build(true)
	hookDecCI, plainDecCI := build(false)

	hookEnc, err := CompileEncoder[hookPerson](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	plainEnc, err := CompileEncoder[hookPersonPlain](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	integrityEnc, err := CompileEncoder[hookIntegrityDocument](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	recoveryEnc, err := CompileEncoder[hookRecoveryDocument](EncoderOptions{})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, mode byte, src []byte, caseSensitive bool) {
		if mode%2 != 0 {
			checkHookIntegritySpan(t, src, integrityEnc, recoveryEnc)
			return
		}
		if len(src) > 1<<14 || !Valid(src) {
			return
		}
		hookDec, plainDec := hookDecCS, plainDecCS
		if !caseSensitive {
			hookDec, plainDec = hookDecCI, plainDecCI
		}

		var viaHook hookPerson
		hookErr := hookDec.Decode(src, &viaHook)
		var viaPlain hookPersonPlain
		plainErr := plainDec.Decode(src, &viaPlain)

		if (hookErr == nil) != (plainErr == nil) {
			t.Fatalf("acceptance differs (cs=%v): hook=%v plain=%v\nsrc=%s", caseSensitive, hookErr, plainErr, src)
		}
		if hookErr != nil {
			return
		}
		if !hookPersonEqual(projectHook(viaHook), viaPlain) {
			t.Fatalf("decoded value differs (cs=%v):\n hook=%+v\nplain=%+v\nsrc=%s", caseSensitive, projectHook(viaHook), viaPlain, src)
		}

		// Re-encode the decoded value three ways and require byte equality.
		hookOut, hookEncErr := hookEnc.AppendJSON(nil, &viaHook)
		plainOut, plainEncErr := plainEnc.AppendJSON(nil, &viaPlain)
		if (hookEncErr == nil) != (plainEncErr == nil) {
			t.Fatalf("encode acceptance differs: hook=%v plain=%v", hookEncErr, plainEncErr)
		}
		if hookEncErr != nil {
			return
		}
		if string(hookOut) != string(plainOut) {
			t.Fatalf("hook vs reflection encode differ:\n hook=%s\nplain=%s", hookOut, plainOut)
		}
		stdOut, err := stdjson.Marshal(&viaPlain)
		if err != nil {
			t.Fatalf("stdlib marshal: %v", err)
		}
		if string(hookOut) != string(stdOut) {
			t.Fatalf("hook vs stdlib encode differ:\n hook=%s\n  std=%s", hookOut, stdOut)
		}
	})
}

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

type hookIntegrityDocument struct {
	Hook hookIntegrityRaw `json:"hook"`
}

func checkHookIntegritySpan(t *testing.T, raw []byte, enc Encoder[hookIntegrityDocument], recoveryEnc Encoder[hookRecoveryDocument]) {
	t.Helper()
	if len(raw) <= 64 {
		checkHookPlanRecoverySequence(t, raw, recoveryEnc)
	}
	if len(raw) > 1<<12 {
		t.Skip()
	}
	value := hookIntegrityDocument{Hook: raw}
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
