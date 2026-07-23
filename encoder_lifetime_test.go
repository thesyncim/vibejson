package slopjson

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// TestEncodeMapErrorPathStability exercises the pooled encoder scratch: an
// error Path built from the pooled numeric-key arena must survive later
// encodes that rewrite that arena. TestEncoderScratchConcurrency covers
// the companion property that concurrent encodes through one compiled encoder
// do not cross-talk.
func TestEncodeMapErrorPathStability(t *testing.T) {
	type doc struct {
		M map[int]float64 `json:"m"`
	}
	enc, err := CompileEncoder[doc](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bad := doc{M: map[int]float64{1234567: math.NaN()}}
	_, err = enc.AppendJSON(nil, &bad)
	if err == nil {
		t.Fatal("want error for NaN map value")
	}
	encErr, ok := err.(*EncodeError)
	if !ok {
		t.Fatalf("want *EncodeError, got %T", err)
	}
	pathBefore := encErr.Path
	// Rewrite the pooled key arena with different digits many times.
	good := doc{M: map[int]float64{7654321: 1, 999: 2, 88: 3}}
	for i := 0; i < 32; i++ {
		if _, err := enc.AppendJSON(nil, &good); err != nil {
			t.Fatal(err)
		}
	}
	if encErr.Path != pathBefore || !strings.Contains(pathBefore, "1234567") {
		t.Fatalf("error path mutated by pooled arena reuse: before %q after %q", pathBefore, encErr.Path)
	}
}

func TestEncoderScratchConcurrency(t *testing.T) {
	type doc struct {
		M map[string]int  `json:"m"`
		N map[int]string  `json:"n"`
		A any             `json:"a"`
		S staticMarshaler `json:"s"`
	}
	enc, err := CompileEncoder[doc](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	done := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			v := doc{
				M: map[string]int{fmt.Sprintf("k%d", w): w},
				N: map[int]string{w * 1111: fmt.Sprintf("v%d", w)},
				A: map[string]bool{fmt.Sprintf("a%d", w): true},
				S: staticMarshaler{V: w},
			}
			want := fmt.Sprintf(`{"m":{"k%d":%d},"n":{"%d":"v%d"},"a":{"a%d":true},"s":"static"}`, w, w, w*1111, w, w)
			for i := 0; i < 500; i++ {
				out, err := enc.AppendJSON(nil, &v)
				if err != nil {
					done <- err
					return
				}
				if string(out) != want {
					done <- fmt.Errorf("worker %d: got %s want %s", w, out, want)
					return
				}
			}
			done <- nil
		}(w)
	}
	for w := 0; w < workers; w++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// TestDifferentialEscapeBattery is a differential battery over
// escape-dense documents decoded into an any/map/string-bearing struct in both
// ownership modes, compared against encoding/json.
func TestDifferentialEscapeBattery(t *testing.T) {
	type doc struct {
		A string            `json:"a"`
		B any               `json:"b"`
		C map[string]string `json:"c"`
		D []any             `json:"d"`
		E string            `json:"e"`
	}
	rng := rand.New(rand.NewSource(1234))
	escapes := []string{
		jsonUnicodeEscape("0041"), jsonUnicodeEscape("00e9"), jsonUnicodeEscape("20ac"),
		jsonUnicodeEscape("d834") + jsonUnicodeEscape("dd1e"),
		`\n`, `\t`, `\\`, `\"`, `\/`,
	}
	randString := func() string {
		var b strings.Builder
		n := rng.Intn(12)
		for i := 0; i < n; i++ {
			if rng.Intn(2) == 0 {
				b.WriteString(escapes[rng.Intn(len(escapes))])
			} else {
				b.WriteString("ab")
			}
		}
		return b.String()
	}
	decOwned, err := CompileDecoder[doc](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decZero, err := CompileDecoder[doc](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 400; round++ {
		src := []byte(fmt.Sprintf(
			`{"a":"%s","b":{"k%s":"%s","n":[1,"%s",2.5]},"c":{"%s":"%s","x":"%s"},"d":["%s",7,"%s"],"e":"%s"}`,
			randString(), randString(), randString(), randString(),
			"K"+randString(), randString(), randString(),
			randString(), randString(), randString()))
		var want doc
		if err := json.Unmarshal(src, &want); err != nil {
			continue // generator made something encoding/json rejects; skip
		}
		var gotOwned, gotZero doc
		if err := decOwned.Decode(src, &gotOwned); err != nil {
			t.Fatalf("round %d owned: %v\nsrc %s", round, err, src)
		}
		if err := decZero.Decode(src, &gotZero); err != nil {
			t.Fatalf("round %d zerocopy: %v\nsrc %s", round, err, src)
		}
		if !reflect.DeepEqual(gotOwned, want) {
			t.Fatalf("round %d owned mismatch:\nsrc  %s\ngot  %#v\nwant %#v", round, src, gotOwned, want)
		}
		if !reflect.DeepEqual(gotZero, want) {
			t.Fatalf("round %d zerocopy mismatch:\nsrc  %s\ngot  %#v\nwant %#v", round, src, gotZero, want)
		}
	}
}
