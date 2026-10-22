package vibejson

import (
	"encoding/json"
	"testing"
)

type longFieldCollisionPlain struct {
	Name        int `json:"abcdefghName"`
	UpperName   int `json:"abcdefghNAME"`
	Sigma       int `json:"abcdefghΣ"`
	FinalSigma  int `json:"abcdefghς"`
	NumericHead int `json:"01234567Name"`
}

type longFieldCollisionHook longFieldCollisionPlain

var longFieldCollisionFields = MakeFieldSet("abcdefghName", "abcdefghNAME", "abcdefghΣ", "abcdefghς", "01234567Name")

func (v *longFieldCollisionHook) UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error) {
	if err := c.BeginObject("longFieldCollisionHook"); err != nil {
		return c, err
	}
	destinations := [...]*int{&v.Name, &v.UpperName, &v.Sigma, &v.FinalSigma, &v.NumericHead}
	first := true
	for i, dst := range destinations {
		if !c.Field(first, longFieldCollisionFields.Field(i)) {
			break
		}
		if err := c.Int(dst); err != nil {
			return c, err
		}
		first = false
	}
	for {
		key, ok, err := c.NextField(first)
		if err != nil || !ok {
			return c, err
		}
		first = false
		if i, known := longFieldCollisionFields.Lookup(key, c.CaseSensitive()); known {
			err = c.Int(destinations[i])
		} else {
			err = c.Skip()
		}
		if err != nil {
			return c, err
		}
	}
}

func TestLongFieldCollisionPrecedenceMatchesStdlib(t *testing.T) {
	plainDecoder := mustCompileTestDecoder[longFieldCollisionPlain](t, DecoderOptions{})
	hookDecoder := mustCompileTestDecoder[longFieldCollisionHook](t, DecoderOptions{})
	for _, input := range []string{
		`{"abcdefghNAME":1}`,
		`{"abcdefghname":2}`,
		`{"abcdefghName":1,"abcdefghname":2}`,
		`{"abcdefghNAME":1,"abcdefghName":2}`,
		`{"abcdefghName":1,"abcdefghNAME":2}`,
		`{"abcdefghς":3}`,
		`{"abcdefghσ":4}`,
		`{"abcdefghΣ":3,"abcdefghσ":4}`,
		`{"abcdefgh\u004eAME":5}`,
		`{"\u0061bcdefghNAME":6}`,
		`{"abcdefgh\u03c2":7}`,
		`{"abcdefghNAME" : 8}`,
		`{"01234567name":9}`,
		`{"unknown":0,"abcdefghNAME":10,"abcdefghσ":11}`,
	} {
		t.Run(input, func(t *testing.T) {
			src := []byte(input)
			var want, plain longFieldCollisionPlain
			var hook longFieldCollisionHook
			if err := json.Unmarshal(src, &want); err != nil {
				t.Fatal(err)
			}
			if err := plainDecoder.Decode(src, &plain); err != nil {
				t.Fatal(err)
			}
			if err := hookDecoder.Decode(src, &hook); err != nil {
				t.Fatal(err)
			}
			if plain != want || longFieldCollisionPlain(hook) != want {
				t.Fatalf("compiled=%+v hook=%+v encoding/json=%+v", plain, hook, want)
			}
		})
	}
}
