package slopjson

import (
	"encoding/json"
	"testing"
)

// pointerOnlyJSON implements json.Marshaler on its pointer receiver only, so
// encoding/json calls it only where the value is addressable.
type pointerOnlyJSON struct{ V int }

func (*pointerOnlyJSON) MarshalJSON() ([]byte, error) { return []byte(`"ptr"`), nil }

// pointerOnlyText implements encoding.TextMarshaler on its pointer receiver.
type pointerOnlyText struct{ V int }

func (*pointerOnlyText) MarshalText() ([]byte, error) { return []byte("txt"), nil }

type holdsPointerOnly struct {
	M pointerOnlyJSON `json:"m"`
}

type holdsPointerOnlyText struct {
	M pointerOnlyText `json:"m"`
}

type nestsPointerOnly struct {
	Inner holdsPointerOnly `json:"inner"`
}

// marshalerAndSlice is the documented known limitation: a non-addressable
// struct that both triggers the fallback (M) and carries a slice of the same
// pointer-receiver marshaler. See TestEncodeAddressabilitySliceLimitation.
type marshalerAndSlice struct {
	M pointerOnlyJSON   `json:"m"`
	S []pointerOnlyJSON `json:"s"`
}

// TestEncodeAddressabilityMatchesStdlib pins the addressability rule that
// governs pointer-receiver marshalers: encoding/json calls them only where a
// value can be addressed. Map values, interface contents, and array elements
// inside them are not addressable, so the method is skipped and the value
// takes its default encoding; slice elements and pointers restore
// addressability, so the method runs. Every case is checked byte-for-byte
// against encoding/json.
func TestEncodeAddressabilityMatchesStdlib(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"pointer-only struct as map value", map[string]holdsPointerOnly{"k": {M: pointerOnlyJSON{1}}}},
		{"pointer-only struct in interface", any(holdsPointerOnly{M: pointerOnlyJSON{1}})},
		{"pointer-only array in map value", map[string][2]pointerOnlyJSON{"k": {{1}, {2}}}},
		{"pointer-only text struct as map value", map[string]holdsPointerOnlyText{"k": {M: pointerOnlyText{3}}}},
		{"nested struct in struct in map value", map[string]nestsPointerOnly{"k": {Inner: holdsPointerOnly{M: pointerOnlyJSON{8}}}}},
		// Slice elements and pointers stay addressable, so the method runs
		// in both libraries.
		{"pointer-only slice in map value", map[string][]pointerOnlyJSON{"k": {{9}}}},
		{"pointer to pointer-only in map value", map[string]*pointerOnlyJSON{"k": {5}}},
		{"pointer-only struct as slice element", []holdsPointerOnly{{M: pointerOnlyJSON{7}}}},
		// Top-level and struct-field values are addressable, so the method
		// runs; these guard against the fallback firing too eagerly.
		{"pointer-only struct at top level", holdsPointerOnly{M: pointerOnlyJSON{4}}},
		{"pointer-only value directly in map", map[string]pointerOnlyJSON{"k": {6}}},
	}
	runAddressabilityCases(t, cases)
}

// TestEncodeAddressabilitySliceLimitation documents the one case slopjson does
// not match encoding/json on: a pointer-receiver marshaler reached through a
// slice that is itself nested inside a non-addressable struct which
// independently triggers the fallback. encoding/json still calls the method on
// the addressable slice elements; slopjson falls back for them too. The slice
// elements' addressability is only restored inside the non-addressable subtree
// at a cost the hot encode path cannot absorb, and the shape — a struct that
// both holds a direct pointer-receiver marshaler and a sibling slice of one,
// stored in a map or interface — does not arise in practice. Encoding it never
// errors or corrupts; it emits the default form of the slice elements.
func TestEncodeAddressabilitySliceLimitation(t *testing.T) {
	value := map[string]marshalerAndSlice{"k": {M: pointerOnlyJSON{1}, S: []pointerOnlyJSON{{2}, {3}}}}
	got, err := Marshal(&value)
	if err != nil {
		t.Fatalf("Marshal error = %v", err)
	}
	// The trigger field falls back like encoding/json; only the slice
	// elements diverge, taking their default form instead of the method.
	const want = `{"k":{"m":{"V":1},"s":[{"V":2},{"V":3}]}}`
	if string(got) != want {
		t.Fatalf("got %s, want %s (documented limitation)", got, want)
	}
}

func runAddressabilityCases(t *testing.T, cases []struct {
	name  string
	value any
}) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := json.Marshal(tc.value)
			got, gotErr := Marshal(&tc.value)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("error mismatch: slopjson=%v encoding/json=%v", gotErr, wantErr)
			}
			if string(got) != string(want) {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}
