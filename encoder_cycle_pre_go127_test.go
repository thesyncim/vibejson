//go:build !go1.27

package slopjson

import (
	"encoding/json"
	"testing"
)

func TestStableEncoderRejectsMapAndSliceCycles(t *testing.T) {
	cyclicMap := map[string]any{}
	cyclicMap["self"] = cyclicMap

	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice

	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "map", value: cyclicMap},
		{name: "slice", value: cyclicSlice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := json.Marshal(tc.value); err == nil {
				t.Fatal("encoding/json accepted a reference cycle")
			}
			if _, err := Marshal(&tc.value); err == nil {
				t.Fatal("slopjson accepted a reference cycle")
			}
		})
	}
}
