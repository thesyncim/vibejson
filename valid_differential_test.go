package slopjson

import (
	"encoding/json"
	"math/rand"
	"testing"
	"unicode/utf8"
)

// seedDocs are valid JSON documents mutated to probe the accept/reject boundary.
var validParitySeeds = []string{
	`{"a":1,"b":[2,3],"c":{"d":true}}`,
	`[1,2,3,4,5]`,
	`"string with \t escapes"`,
	`{"nested":{"deep":{"deeper":[1,[2,[3,[4]]]]}}}`,
	`123.456e-7`,
	`[{},{},[],{"k":null}]`,
	`{"unicode":"é𝄞","esc":"é"}`,
	`true`, `false`, `null`, `0`, `-0.0`, `1e10`,
	`{"dup":1,"dup":2}`,
	`[]`, `{}`, `""`,
}

// TestValidateParity mutates valid JSON by byte flips, insertions, and
// deletions, then checks Parse, dynamic Unmarshal, and Validate all agree with json.Valid
// on acceptance. A structural parser bug that accepts invalid input (or rejects
// valid input) shows up as a mismatch here.
func TestValidateParity(t *testing.T) {
	r := rand.New(rand.NewSource(0x5A11D))
	structural := []byte(`{}[]",:0123456789.eEtfn-+ \/`)
	check := func(b []byte) {
		// The library intentionally rejects invalid UTF-8 (and lone surrogates)
		// in strings, where json.Valid is lenient. Restrict the structural
		// oracle to valid-UTF-8 inputs so only real structural disagreements
		// surface.
		if !utf8.Valid(b) {
			return
		}
		want := json.Valid(b)
		// Parse
		_, perr := Parse(b)
		if (perr == nil) != want {
			// Parse is stricter on trailing data etc.; only fail if Parse ACCEPTS
			// what json.Valid REJECTS (unsafe direction), or rejects what it
			// accepts as a single value.
			if perr == nil && !want {
				t.Fatalf("Parse ACCEPTED json-invalid %.80q", b)
			}
			if perr != nil && want {
				t.Fatalf("Parse REJECTED json-valid %.80q: %v", b, perr)
			}
		}
		// Validate must match json.Valid exactly (both whole-document validators).
		verr := Validate(b)
		if (verr == nil) != want {
			t.Fatalf("Validate/%v disagree on %.80q: Validate=%v json.Valid=%v", want, b, verr, want)
		}
		// Dynamic decode DECODES numbers into float64, so it range-checks like
		// json.Unmarshal into any (not the purely structural json.Valid). Compare
		// against that oracle instead.
		var stdAny any
		stdErr := json.Unmarshal(b, &stdAny)
		_, aerr := unmarshalAnyForTest(b)
		if (aerr == nil) != (stdErr == nil) {
			t.Fatalf("dynamic decode disagrees with Unmarshal-into-any on %.80q: ours=%v stdlib=%v", b, aerr, stdErr)
		}
	}
	for _, seed := range validParitySeeds {
		for iter := 0; iter < testIterations(20_000, 200); iter++ {
			b := []byte(seed)
			mut := make([]byte, len(b))
			copy(mut, b)
			ops := 1 + r.Intn(3)
			for o := 0; o < ops; o++ {
				if len(mut) == 0 {
					mut = append(mut, structural[r.Intn(len(structural))])
					continue
				}
				switch r.Intn(3) {
				case 0: // flip
					mut[r.Intn(len(mut))] = structural[r.Intn(len(structural))]
				case 1: // insert
					pos := r.Intn(len(mut) + 1)
					mut = append(mut[:pos], append([]byte{structural[r.Intn(len(structural))]}, mut[pos:]...)...)
				case 2: // delete
					pos := r.Intn(len(mut))
					mut = append(mut[:pos], mut[pos+1:]...)
				}
			}
			check(mut)
		}
	}
}
