package vibejson

import (
	"encoding/json"
	"testing"
)

type pointerOnlyJSON struct{ V int }

func (*pointerOnlyJSON) MarshalJSON() ([]byte, error) { return []byte(`"ptr"`), nil }

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

type marshalerAndSlice struct {
	M pointerOnlyJSON   `json:"m"`
	S []pointerOnlyJSON `json:"s"`
}

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
		{"pointer-only slice in map value", map[string][]pointerOnlyJSON{"k": {{9}}}},
		{"pointer to pointer-only in map value", map[string]*pointerOnlyJSON{"k": {5}}},
		{"pointer-only struct as slice element", []holdsPointerOnly{{M: pointerOnlyJSON{7}}}},
		{"pointer-only struct at top level", holdsPointerOnly{M: pointerOnlyJSON{4}}},
		{"pointer-only value directly in map", map[string]pointerOnlyJSON{"k": {6}}},
	}
	runAddressabilityCases(t, cases)
}

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
