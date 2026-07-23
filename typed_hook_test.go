package slopjson

import (
	stdjson "encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// This file exercises the method-hook tier end to end. hookAddress and
// hookPerson below implement UnmarshalerSimd/MarshalerSimd using only the
// public DecodeCursor/TrustedAppender/Field surface — the exact code a generator would emit,
// including a full arbitrary-order fallback that handles reordered, missing,
// extra, and duplicate members and honours DecoderOptions.CaseSensitive. Their
// twins hookAddressPlain/hookPersonPlain carry the identical layout and json
// tags but no hooks, so the differential tests can compare the hook path
// against this package's own reflection path and against encoding/json.

// --- hookAddress: a small nested struct with a hook ------------------------

type hookAddress struct {
	Street string `json:"street"`
	City   string `json:"city"`
	Zip    int    `json:"zip"`
}

type hookAddressPlain struct {
	Street string `json:"street"`
	City   string `json:"city"`
	Zip    int    `json:"zip"`
}

var hookAddressFields = MakeFieldSet("street", "city", "zip")

func (a *hookAddress) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	// A top-level null is a no-op on a struct, matching encoding/json.
	if null, err := c.Null(); err != nil {
		return c, err
	} else if null {
		return c, nil
	}
	if err := c.BeginObject("hookAddress"); err != nil {
		return c, err
	}
	// Expected-order fast path: chain packed matches, drop to the general
	// loop at the first miss so any other order still decodes correctly.
	if c.Field(true, hookAddressFields.Field(0)) {
		if err := c.String(&a.Street); err != nil {
			return c, err
		}
		if c.Field(false, hookAddressFields.Field(1)) {
			if err := c.String(&a.City); err != nil {
				return c, err
			}
			if c.Field(false, hookAddressFields.Field(2)) {
				if err := c.Int(&a.Zip); err != nil {
					return c, err
				}
				if c.ExpectObjectClose() {
					return c, nil
				}
				err := a.unmarshalRest(&c, false)
				return c, err
			}
			err := a.unmarshalRest(&c, false)
			return c, err
		}
		err := a.unmarshalRest(&c, false)
		return c, err
	}
	err := a.unmarshalRest(&c, true)
	return c, err
}

// unmarshalRest is the arbitrary-order fallback: a NextField loop keyed by the
// FieldSet, tolerant of reordered, missing, extra, and duplicate members and
// case-insensitive per the decoder option. A real generator emits exactly this.
func (a *hookAddress) unmarshalRest(c *DecodeCursor, first bool) error {
	cs := c.CaseSensitive()
	for {
		key, ok, err := c.NextField(first)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		first = false
		idx, known := hookAddressFields.Lookup(key, cs)
		if !known {
			if err := c.Skip(); err != nil {
				return err
			}
			continue
		}
		switch idx {
		case 0:
			err = c.String(&a.Street)
		case 1:
			err = c.String(&a.City)
		case 2:
			err = c.Int(&a.Zip)
		}
		if err != nil {
			return err
		}
	}
}

func (a *hookAddress) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	w = w.RawUnchecked(`{"street":`).String(a.Street)
	w = w.RawUnchecked(`,"city":`).String(a.City)
	w = w.RawUnchecked(`,"zip":`).Int(int64(a.Zip))
	return w.RawByteUnchecked('}')
}

// --- hookPerson: the outer type, nesting a hooked struct and a slice --------

type hookPerson struct {
	ID       int64         `json:"id"`
	Name     string        `json:"name"`
	Active   bool          `json:"active"`
	Score    float64       `json:"score"`
	Tags     []string      `json:"tags"`
	Address  hookAddress   `json:"address"`
	Aliases  []hookAddress `json:"aliases"`
	Nickname string        `json:"nickname,omitempty"`
}

type hookPersonPlain struct {
	ID       int64              `json:"id"`
	Name     string             `json:"name"`
	Active   bool               `json:"active"`
	Score    float64            `json:"score"`
	Tags     []string           `json:"tags"`
	Address  hookAddressPlain   `json:"address"`
	Aliases  []hookAddressPlain `json:"aliases"`
	Nickname string             `json:"nickname,omitempty"`
}

var hookPersonFields = MakeFieldSet("id", "name", "active", "score", "tags", "address", "aliases", "nickname")

func (p *hookPerson) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	if null, err := c.Null(); err != nil {
		return c, err
	} else if null {
		return c, nil
	}
	if err := c.BeginObject("hookPerson"); err != nil {
		return c, err
	}
	// This body always takes the general loop, exercising the FieldSet lookup
	// and nested-hook dispatch on every member.
	err := p.unmarshalAll(&c, true)
	return c, err
}

func (p *hookPerson) unmarshalAll(c *DecodeCursor, first bool) error {
	cs := c.CaseSensitive()
	for {
		key, ok, err := c.NextField(first)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		first = false
		idx, known := hookPersonFields.Lookup(key, cs)
		if !known {
			if err := c.Skip(); err != nil {
				return err
			}
			continue
		}
		switch idx {
		case 0:
			err = c.Int64(&p.ID)
		case 1:
			err = c.String(&p.Name)
		case 2:
			err = c.Bool(&p.Active)
		case 3:
			err = c.Float64(&p.Score)
		case 4:
			err = p.decodeTags(c)
		case 5:
			var next DecodeCursor
			next, err = p.Address.UnmarshalSimdJSON(*c)
			*c = next
		case 6:
			err = p.decodeAliases(c)
		case 7:
			err = c.String(&p.Nickname)
		}
		if err != nil {
			return err
		}
	}
}

func (p *hookPerson) decodeTags(c *DecodeCursor) error {
	if null, err := c.Null(); err != nil {
		return err
	} else if null {
		p.Tags = nil
		return nil
	}
	if err := c.BeginArray("[]string"); err != nil {
		return err
	}
	// An empty array must decode to a non-nil empty slice, matching
	// encoding/json (nil encodes as null, [] as an empty array).
	if p.Tags == nil {
		p.Tags = []string{}
	} else {
		p.Tags = p.Tags[:0]
	}
	first := true
	for {
		more, err := c.NextElement(first)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		first = false
		var s string
		if err := c.String(&s); err != nil {
			return err
		}
		p.Tags = append(p.Tags, s)
	}
}

func (p *hookPerson) decodeAliases(c *DecodeCursor) error {
	if null, err := c.Null(); err != nil {
		return err
	} else if null {
		p.Aliases = nil
		return nil
	}
	if err := c.BeginArray("[]hookAddress"); err != nil {
		return err
	}
	if p.Aliases == nil {
		p.Aliases = []hookAddress{}
	} else {
		p.Aliases = p.Aliases[:0]
	}
	first := true
	for {
		more, err := c.NextElement(first)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		first = false
		var a hookAddress
		next, err := a.UnmarshalSimdJSON(*c)
		*c = next
		if err != nil {
			return err
		}
		p.Aliases = append(p.Aliases, a)
	}
}

func (p *hookPerson) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	w = w.RawUnchecked(`{"id":`).Int(p.ID)
	w = w.RawUnchecked(`,"name":`).String(p.Name)
	w = w.RawUnchecked(`,"active":`).Bool(p.Active)
	w = w.RawUnchecked(`,"score":`).Float64(p.Score)
	w = w.RawUnchecked(`,"tags":`)
	if p.Tags == nil {
		w = w.Null()
	} else {
		w = w.RawByteUnchecked('[')
		for i, t := range p.Tags {
			if i > 0 {
				w = w.RawByteUnchecked(',')
			}
			w = w.String(t)
		}
		w = w.RawByteUnchecked(']')
	}
	w = w.RawUnchecked(`,"address":`)
	w = p.Address.MarshalSimdJSON(w)
	w = w.RawUnchecked(`,"aliases":`)
	if p.Aliases == nil {
		w = w.Null()
	} else {
		w = w.RawByteUnchecked('[')
		for i := range p.Aliases {
			if i > 0 {
				w = w.RawByteUnchecked(',')
			}
			w = p.Aliases[i].MarshalSimdJSON(w)
		}
		w = w.RawByteUnchecked(']')
	}
	if p.Nickname != "" {
		w = w.RawUnchecked(`,"nickname":`).String(p.Nickname)
	}
	return w.RawByteUnchecked('}')
}

// sampleHookPersonJSON returns a canonical, in-order document.
func sampleHookPersonJSON() []byte {
	return []byte(`{"id":42,"name":"Ada","active":true,"score":3.5,` +
		`"tags":["x","y","z"],"address":{"street":"1 Main","city":"Metropolis","zip":12345},` +
		`"aliases":[{"street":"2 Oak","city":"Gotham","zip":54321},{"street":"3 Elm","city":"Star","zip":99999}],` +
		`"nickname":"Countess"}`)
}

// adversarialHookDocs returns documents that stress every fallback path: exact
// order, reordered members, missing members, extra unknown members, duplicate
// members (last wins, per encoding/json), escaped values, null containers, and
// the omitempty field both present and absent.
func adversarialHookDocs() map[string]string {
	return map[string]string{
		"canonical":    string(sampleHookPersonJSON()),
		"reordered":    `{"score":1.25,"name":"Bob","tags":["a"],"active":false,"id":7,"aliases":[],"address":{"zip":1,"city":"C","street":"S"}}`,
		"missing":      `{"name":"Carol","address":{"city":"OnlyCity"}}`,
		"extraFront":   `{"unknown1":123,"id":9,"extra":{"nested":[1,2,3]},"name":"Dan","address":{"street":"x","city":"y","zip":3,"stray":true}}`,
		"dupKeys":      `{"id":1,"id":2,"name":"first","name":"second","address":{"street":"a","street":"b","city":"c","zip":4,"zip":5}}`,
		"escaped":      `{"name":"a\"b\\c\n\td","address":{"street":"é ","city":"😀","zip":0},"tags":["\/","\b\f"]}`,
		"nullArrays":   `{"id":3,"name":"E","tags":null,"aliases":null,"address":{"street":"s","city":"c","zip":1}}`,
		"emptyArrays":  `{"id":4,"tags":[],"aliases":[],"address":{"street":"","city":"","zip":0}}`,
		"noNickname":   `{"id":5,"name":"F","address":{"street":"s","city":"c","zip":2}}`,
		"withNickname": `{"id":6,"name":"G","nickname":"H","address":{"street":"s","city":"c","zip":3}}`,
		"whitespace":   "{ \"id\" : 8 , \"name\" : \"ws\" , \"address\" : { \"street\" : \"s\" , \"city\" : \"c\" , \"zip\" : 1 } }",
		"scoreEdge":    `{"id":9,"score":-0.0,"name":"neg","address":{"street":"s","city":"c","zip":-2147483648}}`,
		"bigZip":       `{"id":10,"name":"big","address":{"street":"s","city":"c","zip":2147483647}}`,
	}
}

// decodePlain / decodeHook / decodeStd decode the same document three ways and
// return the projected plain form so results are directly comparable regardless
// of the concrete type carrying the hooks.
func projectHook(p hookPerson) hookPersonPlain {
	out := hookPersonPlain{
		ID: p.ID, Name: p.Name, Active: p.Active, Score: p.Score,
		Tags: p.Tags, Nickname: p.Nickname,
		Address: hookAddressPlain(p.Address),
	}
	if p.Aliases != nil {
		out.Aliases = make([]hookAddressPlain, len(p.Aliases))
		for i, a := range p.Aliases {
			out.Aliases[i] = hookAddressPlain(a)
		}
	}
	return out
}

func TestHookDecodeMatchesReflectionAndStdlib(t *testing.T) {
	for _, cs := range []bool{true, false} {
		opts := DecoderOptions{CaseSensitive: cs}
		hookDec, err := CompileDecoder[hookPerson](opts)
		if err != nil {
			t.Fatal(err)
		}
		plainDec, err := CompileDecoder[hookPersonPlain](opts)
		if err != nil {
			t.Fatal(err)
		}
		for name, doc := range adversarialHookDocs() {
			t.Run(fmt.Sprintf("cs=%v/%s", cs, name), func(t *testing.T) {
				var viaHook hookPerson
				if err := hookDec.Decode([]byte(doc), &viaHook); err != nil {
					t.Fatalf("hook decode: %v", err)
				}
				var viaPlain hookPersonPlain
				if err := plainDec.Decode([]byte(doc), &viaPlain); err != nil {
					t.Fatalf("reflection decode: %v", err)
				}
				var viaStd hookPersonPlain
				if err := stdjson.Unmarshal([]byte(doc), &viaStd); err != nil {
					t.Fatalf("stdlib decode: %v", err)
				}
				got := projectHook(viaHook)
				if !hookPersonEqual(got, viaPlain) {
					t.Fatalf("hook vs reflection differ:\n hook=%+v\nplain=%+v", got, viaPlain)
				}
				if !hookPersonEqual(got, viaStd) {
					t.Fatalf("hook vs stdlib differ:\n hook=%+v\n  std=%+v", got, viaStd)
				}
			})
		}
	}
}

func TestHookEncodeMatchesReflectionAndStdlib(t *testing.T) {
	// Decode each adversarial doc once with the reflection path, then encode the
	// resulting value three ways and require byte equality.
	plainDec, err := CompileDecoder[hookPersonPlain](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hookEnc, err := CompileEncoder[hookPerson](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plainEnc, err := CompileEncoder[hookPersonPlain](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range adversarialHookDocs() {
		t.Run(name, func(t *testing.T) {
			var plain hookPersonPlain
			if err := plainDec.Decode([]byte(doc), &plain); err != nil {
				t.Fatalf("seed decode: %v", err)
			}
			hook := unprojectHook(plain)

			viaHook, err := hookEnc.AppendJSON(nil, &hook)
			if err != nil {
				t.Fatalf("hook encode: %v", err)
			}
			viaPlain, err := plainEnc.AppendJSON(nil, &plain)
			if err != nil {
				t.Fatalf("reflection encode: %v", err)
			}
			viaStd, err := stdjson.Marshal(&plain)
			if err != nil {
				t.Fatalf("stdlib encode: %v", err)
			}
			if string(viaHook) != string(viaPlain) {
				t.Fatalf("hook vs reflection encode differ:\n hook=%s\nplain=%s", viaHook, viaPlain)
			}
			if string(viaHook) != string(viaStd) {
				t.Fatalf("hook vs stdlib encode differ:\n hook=%s\n  std=%s", viaHook, viaStd)
			}
		})
	}
}

func unprojectHook(p hookPersonPlain) hookPerson {
	out := hookPerson{
		ID: p.ID, Name: p.Name, Active: p.Active, Score: p.Score,
		Tags: p.Tags, Nickname: p.Nickname,
		Address: hookAddress(p.Address),
	}
	if p.Aliases != nil {
		out.Aliases = make([]hookAddress, len(p.Aliases))
		for i, a := range p.Aliases {
			out.Aliases[i] = hookAddress(a)
		}
	}
	return out
}

func hookPersonEqual(a, b hookPersonPlain) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Active != b.Active || a.Nickname != b.Nickname {
		return false
	}
	if a.Score != b.Score && !(math.IsNaN(a.Score) && math.IsNaN(b.Score)) {
		return false
	}
	if !stringsEqual(a.Tags, b.Tags) {
		return false
	}
	if a.Address != b.Address {
		return false
	}
	if len(a.Aliases) != len(b.Aliases) {
		return false
	}
	for i := range a.Aliases {
		if a.Aliases[i] != b.Aliases[i] {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHookRoundTrip proves a full decode->encode round trip through the hook
// path is byte-identical to encoding/json's own round trip.
func TestHookRoundTrip(t *testing.T) {
	dec, err := CompileDecoder[hookPerson](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := CompileEncoder[hookPerson](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := sampleHookPersonJSON()
	var p hookPerson
	if err := dec.Decode(src, &p); err != nil {
		t.Fatal(err)
	}
	out, err := enc.AppendJSON(nil, &p)
	if err != nil {
		t.Fatal(err)
	}
	var stdVal hookPersonPlain
	if err := stdjson.Unmarshal(src, &stdVal); err != nil {
		t.Fatal(err)
	}
	stdOut, err := stdjson.Marshal(&stdVal)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(stdOut) {
		t.Fatalf("round trip differs:\nhook=%s\n std=%s", out, stdOut)
	}
}

// TestHookEncodeFloatEdges checks that a NaN/Inf poisons the TrustedAppender and the
// enclosing encode reports the value as unsupported, matching encoding/json's
// rejection.
func TestHookEncodeFloatEdges(t *testing.T) {
	enc, err := CompileEncoder[hookPerson](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := hookPerson{Score: bad}
		if _, err := enc.AppendJSON(nil, &p); err == nil {
			t.Fatalf("expected error for score=%v", bad)
		} else if !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("score=%v: unexpected error %v", bad, err)
		}
	}
}

// TestHookNonAddressableEncodeFallback verifies encoding/json's condAddr rule
// for a pointer-receiver hook: as an addressable slice element the hook runs,
// but as a non-addressable map value the encoder falls back to the default
// struct encoding, byte-identical to encoding/json for the plain twin. This
// exercises the map-value route called out in the hardening requirements.
func TestHookNonAddressableEncodeFallback(t *testing.T) {
	// Non-addressable map value: *hookAddress's pointer-receiver hook cannot
	// run, so the default struct encoding applies, matching encoding/json.
	type hookMap struct {
		Items map[string]hookAddress `json:"items"`
	}
	type plainMap struct {
		Items map[string]hookAddressPlain `json:"items"`
	}
	hookEnc, err := CompileEncoder[hookMap](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hm := hookMap{Items: map[string]hookAddress{"a": {Street: "s", City: "c", Zip: 1}}}
	got, err := hookEnc.AppendJSON(nil, &hm)
	if err != nil {
		t.Fatal(err)
	}
	pm := plainMap{Items: map[string]hookAddressPlain{"a": {Street: "s", City: "c", Zip: 1}}}
	want, err := stdjson.Marshal(&pm)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("non-addressable map value:\n got=%s\nwant=%s", got, want)
	}

	// Addressable slice element: the hook DOES run and produces the hook's own
	// compact form, which here coincides with the default encoding.
	type hookSlice struct {
		Items []hookAddress `json:"items"`
	}
	sliceEnc, err := CompileEncoder[hookSlice](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hs := hookSlice{Items: []hookAddress{{Street: "s", City: "c", Zip: 1}}}
	sliceOut, err := sliceEnc.AppendJSON(nil, &hs)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"items":[{"street":"s","city":"c","zip":1}]}`; string(sliceOut) != want {
		t.Fatalf("addressable slice element hook:\n got=%s\nwant=%s", sliceOut, want)
	}
}

// hookFieldOuter is a plain (non-hook) struct whose field is a hook type, so
// decode and encode reach the hook through the interpreter's struct-field
// dispatch (typedOpUnmarshaler -> typedUnmarshalerSimd, and typedOpMarshaler ->
// the Simd encode hook) rather than through an explicit body call. hookFieldSlice
// does the same through the slice-element dispatch.
type hookFieldOuter struct {
	Label string      `json:"label"`
	Addr  hookAddress `json:"addr"`
	Count int         `json:"count"`
}

type hookFieldOuterPlain struct {
	Label string           `json:"label"`
	Addr  hookAddressPlain `json:"addr"`
	Count int              `json:"count"`
}

type hookFieldSlice struct {
	Items []hookAddress `json:"items"`
}

type hookFieldSlicePlain struct {
	Items []hookAddressPlain `json:"items"`
}

func TestHookInterpreterFieldDispatch(t *testing.T) {
	// A hook type embedded as a struct field: the interpreter's field switch
	// must dispatch it, matching encoding/json (default struct form here).
	fieldSrc := []byte(`{"label":"L","addr":{"zip":9,"street":"s","city":"c"},"count":3}`)
	fieldDec, err := CompileDecoder[hookFieldOuter](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gotField hookFieldOuter
	if err := fieldDec.Decode(fieldSrc, &gotField); err != nil {
		t.Fatal(err)
	}
	var wantField hookFieldOuterPlain
	if err := stdjson.Unmarshal(fieldSrc, &wantField); err != nil {
		t.Fatal(err)
	}
	if gotField.Label != wantField.Label || hookAddressPlain(gotField.Addr) != wantField.Addr || gotField.Count != wantField.Count {
		t.Fatalf("field-dispatch decode mismatch:\n got=%+v\nwant=%+v", gotField, wantField)
	}
	fieldEnc, err := CompileEncoder[hookFieldOuter](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gotFieldOut, err := fieldEnc.AppendJSON(nil, &gotField)
	if err != nil {
		t.Fatal(err)
	}
	wantFieldOut, err := stdjson.Marshal(&wantField)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotFieldOut) != string(wantFieldOut) {
		t.Fatalf("field-dispatch encode mismatch:\n got=%s\nwant=%s", gotFieldOut, wantFieldOut)
	}

	// A slice of a hook type: the interpreter's element dispatch must reach the
	// hook for each element.
	sliceSrc := []byte(`{"items":[{"street":"a","city":"b","zip":1},{"zip":2,"city":"d","street":"c"}]}`)
	sliceDec, err := CompileDecoder[hookFieldSlice](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gotSlice hookFieldSlice
	if err := sliceDec.Decode(sliceSrc, &gotSlice); err != nil {
		t.Fatal(err)
	}
	var wantSlice hookFieldSlicePlain
	if err := stdjson.Unmarshal(sliceSrc, &wantSlice); err != nil {
		t.Fatal(err)
	}
	if len(gotSlice.Items) != len(wantSlice.Items) {
		t.Fatalf("slice-dispatch length mismatch: %d vs %d", len(gotSlice.Items), len(wantSlice.Items))
	}
	for i := range gotSlice.Items {
		if hookAddressPlain(gotSlice.Items[i]) != wantSlice.Items[i] {
			t.Fatalf("slice-dispatch element %d mismatch: %+v vs %+v", i, gotSlice.Items[i], wantSlice.Items[i])
		}
	}
	sliceEnc, err := CompileEncoder[hookFieldSlice](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gotSliceOut, err := sliceEnc.AppendJSON(nil, &gotSlice)
	if err != nil {
		t.Fatal(err)
	}
	wantSliceOut, err := stdjson.Marshal(&wantSlice)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSliceOut) != string(wantSliceOut) {
		t.Fatalf("slice-dispatch encode mismatch:\n got=%s\nwant=%s", gotSliceOut, wantSliceOut)
	}
}
