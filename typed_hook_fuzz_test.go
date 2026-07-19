package simdjson

import (
	stdjson "encoding/json"
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
