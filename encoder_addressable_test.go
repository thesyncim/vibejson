package vibejson

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

// marshalerAndSlice combines a direct non-addressable field with a sibling
// slice whose elements regain addressability.
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

// A non-addressable struct envelope must not suppress pointer-receiver methods
// below a slice or pointer, both of which restore addressability. Arrays and
// nested structs preserve the envelope's non-addressability.
func TestEncodeAddressabilityRestoredInsideNonAddressableValue(t *testing.T) {
	type nested struct {
		Direct pointerOnlyJSON      `json:"direct"`
		Slice  []pointerOnlyJSON    `json:"slice"`
		Array  [1]pointerOnlyJSON   `json:"array"`
		Ptr    *pointerOnlyJSON     `json:"ptr"`
		Child  marshalerAndSlice    `json:"child"`
		Kids   []marshalerAndSlice  `json:"kids"`
		Grid   [1]marshalerAndSlice `json:"grid"`
	}
	value := nested{
		Direct: pointerOnlyJSON{1},
		Slice:  []pointerOnlyJSON{{2}},
		Array:  [1]pointerOnlyJSON{{3}},
		Ptr:    &pointerOnlyJSON{4},
		Child:  marshalerAndSlice{M: pointerOnlyJSON{5}, S: []pointerOnlyJSON{{6}}},
		Kids:   []marshalerAndSlice{{M: pointerOnlyJSON{7}, S: []pointerOnlyJSON{{8}}}},
		Grid:   [1]marshalerAndSlice{{M: pointerOnlyJSON{9}, S: []pointerOnlyJSON{{10}}}},
	}
	runAddressabilityCases(t, []struct {
		name  string
		value any
	}{
		{"map value with sibling slice", map[string]marshalerAndSlice{"k": {
			M: pointerOnlyJSON{1}, S: []pointerOnlyJSON{{2}, {3}},
		}}},
		{"interface value with sibling slice", any(marshalerAndSlice{
			M: pointerOnlyJSON{1}, S: []pointerOnlyJSON{{2}, {3}},
		})},
		{"nested addressability boundaries in map", map[string]nested{"k": value}},
		{"nested addressability boundaries in interface", any(value)},
	})
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
				t.Fatalf("error mismatch: vibejson=%v encoding/json=%v", gotErr, wantErr)
			}
			if string(got) != string(want) {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}
