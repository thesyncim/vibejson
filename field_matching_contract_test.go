package slopjson

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type fieldMatchingHook struct {
	Name       int `json:"Name"`
	UpperName  int `json:"NAME"`
	Kelvin     int `json:"Kelvin"`
	LongS      int `json:"ſcore"`
	Sigma      int `json:"Σ"`
	FinalSigma int `json:"ς"`
	Plain      int `json:"plain"`
}

type fieldMatchingPlain struct {
	Name       int `json:"Name"`
	UpperName  int `json:"NAME"`
	Kelvin     int `json:"Kelvin"`
	LongS      int `json:"ſcore"`
	Sigma      int `json:"Σ"`
	FinalSigma int `json:"ς"`
	Plain      int `json:"plain"`
}

var fieldMatchingFields = MakeFieldSet("Name", "NAME", "Kelvin", "ſcore", "Σ", "ς", "plain")

func (v *fieldMatchingHook) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	if null, err := c.Null(); err != nil {
		return c, err
	} else if null {
		return c, nil
	}
	if err := c.BeginObject("fieldMatchingHook"); err != nil {
		return c, err
	}
	if c.Field(true, fieldMatchingFields.Field(0)) {
		if err := c.Int(&v.Name); err != nil {
			return c, err
		}
		err := v.unmarshalFields(&c, false)
		return c, err
	}
	err := v.unmarshalFields(&c, true)
	return c, err
}

func (v *fieldMatchingHook) unmarshalFields(c *DecodeCursor, first bool) error {
	for {
		key, ok, err := c.NextField(first)
		if err != nil || !ok {
			return err
		}
		first = false
		index, known := fieldMatchingFields.Lookup(key, c.CaseSensitive())
		if !known {
			if err := c.Skip(); err != nil {
				return err
			}
			continue
		}
		switch index {
		case 0:
			err = c.Int(&v.Name)
		case 1:
			err = c.Int(&v.UpperName)
		case 2:
			err = c.Int(&v.Kelvin)
		case 3:
			err = c.Int(&v.LongS)
		case 4:
			err = c.Int(&v.Sigma)
		case 5:
			err = c.Int(&v.FinalSigma)
		case 6:
			err = c.Int(&v.Plain)
		}
		if err != nil {
			return err
		}
	}
}

func plainFieldMatchingValue(v fieldMatchingHook) fieldMatchingPlain {
	return fieldMatchingPlain{
		Name: v.Name, UpperName: v.UpperName, Kelvin: v.Kelvin, LongS: v.LongS,
		Sigma: v.Sigma, FinalSigma: v.FinalSigma, Plain: v.Plain,
	}
}

func TestFieldSetUnicodeAndCollisionParity(t *testing.T) {
	hookDecoder, err := CompileDecoder[fieldMatchingHook](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plainDecoder, err := CompileDecoder[fieldMatchingPlain](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{"Name":1}`,
		`{"NAME":2}`,
		`{"name":3}`,
		`{"kelvin":4}`,
		`{"Score":5}`,
		`{"σ":6}`,
		`{"ς":7}`,
		`{"PLAIN":8}`,
		`{"\u006eame":9}`,
		`{"\u212Aelvin":10}`,
		`{"\u03c3":11}`,
		`{"name":1,"NAME":2,"KELVIN":3,"kelvin":4,"σ":5,"ς":6}`,
	}
	for _, text := range cases {
		src := []byte(text)
		var hook fieldMatchingHook
		if err := hookDecoder.Decode(src, &hook); err != nil {
			t.Fatalf("hook Decode(%s): %v", src, err)
		}
		var plain fieldMatchingPlain
		if err := plainDecoder.Decode(src, &plain); err != nil {
			t.Fatalf("compiled Decode(%s): %v", src, err)
		}
		var standard fieldMatchingPlain
		if err := json.Unmarshal(src, &standard); err != nil {
			t.Fatalf("encoding/json Decode(%s): %v", src, err)
		}
		if got := plainFieldMatchingValue(hook); !reflect.DeepEqual(got, plain) || !reflect.DeepEqual(got, standard) {
			t.Fatalf("Decode(%s): hook=%+v compiled=%+v encoding/json=%+v", src, got, plain, standard)
		}
	}
}

func TestFieldSetLookupExactAndFoldedPrecedence(t *testing.T) {
	tests := []struct {
		key           string
		caseSensitive bool
		want          int
		ok            bool
	}{
		{key: "Name", want: 0, ok: true},
		{key: "NAME", want: 1, ok: true},
		{key: "name", want: 0, ok: true},
		{key: "kelvin", want: 2, ok: true},
		{key: "Score", want: 3, ok: true},
		{key: "σ", want: 4, ok: true},
		{key: "ς", want: 5, ok: true},
		{key: "PLAIN", want: 6, ok: true},
		{key: "name", caseSensitive: true, want: -1},
		{key: "missing", want: -1},
	}
	for _, test := range tests {
		got, ok := fieldMatchingFields.Lookup(test.key, test.caseSensitive)
		if got != test.want || ok != test.ok {
			t.Errorf("Lookup(%q, %v) = (%d, %v), want (%d, %v)",
				test.key, test.caseSensitive, got, ok, test.want, test.ok)
		}
	}
}

func TestMakeFieldSetRejectsExactDuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MakeFieldSet accepted an exact duplicate member name")
		}
	}()
	_ = MakeFieldSet("name", "name")
}

func TestFieldSetBoundedFoldExpansionFallback(t *testing.T) {
	name := strings.Repeat("s", 7)
	set := MakeFieldSet(name, "plain")
	if cap(set.fields) == len(set.fields) {
		t.Fatal("fold expansion overflow did not enable the ordered fallback")
	}
	if extra := cap(set.fields) - len(set.fields); extra > maxFieldFoldVariants+1 {
		t.Fatalf("fold metadata grew by %d entries, limit is %d", extra, maxFieldFoldVariants+1)
	}

	for _, test := range []struct {
		key  string
		want int
	}{
		{key: strings.Repeat("ſ", 7), want: 0},
		{key: strings.Repeat("S", 7), want: 0},
		{key: "PLAIN", want: 1},
	} {
		if got, ok := set.Lookup(test.key, false); !ok || got != test.want {
			t.Errorf("Lookup(%q, false) = (%d, %v), want (%d, true)", test.key, got, ok, test.want)
		}
	}
	if got, ok := set.Lookup("missing", false); ok || got != -1 {
		t.Fatalf("Lookup(missing, false) = (%d, %v), want (-1, false)", got, ok)
	}
}

func FuzzFieldSetLookupParity(f *testing.F) {
	f.Add([]byte("Name\x00NAME\x00Kelvin\x00ſcore\x00Σ\x00ς"), []byte("name"), false)
	f.Add([]byte("Kelvin\x00score\x00plain"), []byte("ſcore"), false)
	f.Add([]byte("exact\x00EXACT"), []byte("exact"), true)

	f.Fuzz(func(t *testing.T, encodedNames, encodedKey []byte, caseSensitive bool) {
		if len(encodedNames) > 128 || len(encodedKey) > 32 {
			t.Skip()
		}
		parts := strings.Split(string(encodedNames), "\x00")
		if len(parts) > 8 {
			parts = parts[:8]
		}
		names := make([]string, 0, len(parts))
		for _, name := range parts {
			duplicate := false
			for _, existing := range names {
				if existing == name {
					duplicate = true
					break
				}
			}
			if !duplicate {
				names = append(names, name)
			}
		}

		set := MakeFieldSet(names...)
		key := string(encodedKey)
		want := -1
		for i, name := range names {
			if name == key {
				want = i
				break
			}
		}
		if want < 0 && !caseSensitive {
			for i, name := range names {
				if strings.EqualFold(name, key) {
					want = i
					break
				}
			}
		}
		got, ok := set.Lookup(key, caseSensitive)
		if got != want || ok != (want >= 0) {
			t.Fatalf("names=%q Lookup(%q, %v) = (%d, %v), want (%d, %v)",
				names, key, caseSensitive, got, ok, want, want >= 0)
		}
	})
}
