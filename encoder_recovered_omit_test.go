package vibejson

import (
	"bytes"
	"encoding/json"
	"testing"
)

type recoveredOmitJSONSlice []chan int
type recoveredOmitJSONMap map[chan int]int
type recoveredOmitJSONArray0 [0]chan int
type recoveredOmitJSONArray1 [1]chan int

func (recoveredOmitJSONSlice) MarshalJSON() ([]byte, error)  { return []byte(`"slice"`), nil }
func (recoveredOmitJSONMap) MarshalJSON() ([]byte, error)    { return []byte(`"map"`), nil }
func (recoveredOmitJSONArray0) MarshalJSON() ([]byte, error) { return []byte(`"array"`), nil }
func (recoveredOmitJSONArray1) MarshalJSON() ([]byte, error) { return []byte(`"array"`), nil }

type recoveredOmitTextSlice []chan int
type recoveredOmitTextMap map[chan int]int
type recoveredOmitTextArray0 [0]chan int
type recoveredOmitTextArray1 [1]chan int

func (recoveredOmitTextSlice) MarshalText() ([]byte, error)  { return []byte("slice"), nil }
func (recoveredOmitTextMap) MarshalText() ([]byte, error)    { return []byte("map"), nil }
func (recoveredOmitTextArray0) MarshalText() ([]byte, error) { return []byte("array"), nil }
func (recoveredOmitTextArray1) MarshalText() ([]byte, error) { return []byte("array"), nil }

type recoveredOmitNativeSlice []chan int
type recoveredOmitNativeMap map[chan int]int
type recoveredOmitNativeArray0 [0]chan int
type recoveredOmitNativeArray1 [1]chan int

func (recoveredOmitNativeSlice) MarshalVibeJSON(w TrustedAppender) TrustedAppender {
	return w.String("slice")
}
func (recoveredOmitNativeMap) MarshalVibeJSON(w TrustedAppender) TrustedAppender {
	return w.String("map")
}
func (recoveredOmitNativeArray0) MarshalVibeJSON(w TrustedAppender) TrustedAppender {
	return w.String("array")
}
func (recoveredOmitNativeArray1) MarshalVibeJSON(w TrustedAppender) TrustedAppender {
	return w.String("array")
}

type recoveredOmitDocument[S, M, A0, A1 any] struct {
	Slice S  `json:"slice,omitempty"`
	Map   M  `json:"map,omitempty"`
	Zero  A0 `json:"zero,omitempty"`
	One   A1 `json:"one,omitempty"`
}

func testRecoveredOmitCollections[S ~[]chan int, M ~map[chan int]int, A0 ~[0]chan int, A1 ~[1]chan int](t *testing.T, standardMethods bool) {
	t.Helper()
	type document = recoveredOmitDocument[S, M, A0, A1]
	encoder, err := CompileEncoder[document](EncoderOptions{})
	requireNoTestError(t, err)
	for _, test := range []struct {
		name  string
		slice S
		index M
	}{
		{name: "nil"},
		{name: "empty", slice: S{}, index: M{}},
		{name: "populated", slice: S{nil}, index: M{nil: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := document{Slice: test.slice, Map: test.index}
			// JSON methods are the standard-library oracle for native hooks too:
			// every method emits the same string for its collection kind.
			oracle := recoveredOmitDocument[recoveredOmitJSONSlice, recoveredOmitJSONMap, recoveredOmitJSONArray0, recoveredOmitJSONArray1]{
				Slice: recoveredOmitJSONSlice(test.slice),
				Map:   recoveredOmitJSONMap(test.index),
			}
			want, err := json.Marshal(&oracle)
			requireNoTestError(t, err)
			if standardMethods {
				want, err = json.Marshal(&value)
				requireNoTestError(t, err)
			}
			got, err := encoder.AppendJSON(nil, &value)
			requireNoTestError(t, err)
			if !bytes.Equal(got, want) {
				t.Errorf("AppendJSON = %s, want %s", got, want)
			}
			got, err = Marshal(&value)
			requireNoTestError(t, err)
			if !bytes.Equal(got, want) {
				t.Errorf("Marshal = %s, want %s", got, want)
			}
		})
	}
}

func TestRecoveredCustomHooksOmitEmptyCollections(t *testing.T) {
	t.Run("JSON", func(t *testing.T) {
		testRecoveredOmitCollections[recoveredOmitJSONSlice, recoveredOmitJSONMap, recoveredOmitJSONArray0, recoveredOmitJSONArray1](t, true)
	})
	t.Run("Text", func(t *testing.T) {
		testRecoveredOmitCollections[recoveredOmitTextSlice, recoveredOmitTextMap, recoveredOmitTextArray0, recoveredOmitTextArray1](t, true)
	})
	t.Run("native", func(t *testing.T) {
		testRecoveredOmitCollections[recoveredOmitNativeSlice, recoveredOmitNativeMap, recoveredOmitNativeArray0, recoveredOmitNativeArray1](t, false)
	})
}
