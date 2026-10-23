package vibejson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"runtime"
	"strconv"
	"testing"
)

type omitZeroRetainedValue struct {
	Number   int    `json:"number"`
	Token    string `json:"token"`
	retained *[]*omitZeroRetainedValue
}

func (v omitZeroRetainedValue) IsZero() bool {
	zero := v.Number == 0
	v.Number += 10000
	v.Token = "receiver:" + v.Token
	*v.retained = append(*v.retained, &v)
	return zero
}

type omitZeroRetainedDocument struct {
	Zero omitZeroRetainedValue `json:"zero,omitzero"`
	Keep omitZeroRetainedValue `json:"keep,omitzero"`
}

func newOmitZeroRetainedDocument(round int, token string, retained *[]*omitZeroRetainedValue) omitZeroRetainedDocument {
	return omitZeroRetainedDocument{
		Zero: omitZeroRetainedValue{Token: token + "/zero", retained: retained},
		Keep: omitZeroRetainedValue{Number: round + 1, Token: token + "/keep", retained: retained},
	}
}

type omitZeroPromotedValue struct {
	omitZeroRetainedValue
}

type omitZeroPromotedDocument struct {
	Zero omitZeroPromotedValue `json:"zero,omitzero"`
	Keep omitZeroPromotedValue `json:"keep,omitzero"`
}

func TestOmitZeroValueMethodReceiverRetention(t *testing.T) {
	t.Run("struct", func(t *testing.T) {
		checkOmitZeroValueReceiverRetention(t, func(round int, retained *[]*omitZeroRetainedValue) omitZeroRetainedDocument {
			return newOmitZeroRetainedDocument(round, strconv.Itoa(round), retained)
		})
	})
	t.Run("nested-map", func(t *testing.T) {
		checkOmitZeroValueReceiverRetention(t, func(round int, retained *[]*omitZeroRetainedValue) map[string]map[string]omitZeroRetainedDocument {
			token := strconv.Itoa(round)
			return map[string]map[string]omitZeroRetainedDocument{
				"outer": {
					"a": newOmitZeroRetainedDocument(round, token+"/a", retained),
					"b": newOmitZeroRetainedDocument(round, token+"/b", retained),
				},
			}
		})
	})
	t.Run("interface", func(t *testing.T) {
		checkOmitZeroValueReceiverRetention(t, func(round int, retained *[]*omitZeroRetainedValue) any {
			return newOmitZeroRetainedDocument(round, strconv.Itoa(round), retained)
		})
	})
	t.Run("promoted-value", func(t *testing.T) {
		checkOmitZeroValueReceiverRetention(t, func(round int, retained *[]*omitZeroRetainedValue) omitZeroPromotedDocument {
			doc := newOmitZeroRetainedDocument(round, strconv.Itoa(round), retained)
			return omitZeroPromotedDocument{
				Zero: omitZeroPromotedValue{doc.Zero},
				Keep: omitZeroPromotedValue{doc.Keep},
			}
		})
	})
}

func checkOmitZeroValueReceiverRetention[T any](t *testing.T, build func(int, *[]*omitZeroRetainedValue) T) {
	t.Helper()
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	retained, expected := func() ([]*omitZeroRetainedValue, map[string]int) {
		var retained []*omitZeroRetainedValue
		expected := make(map[string]int)
		buffer := make([]byte, 0, 512)
		for round := 0; round < 12; round++ {
			var oracleReceivers []*omitZeroRetainedValue
			oracle := build(round, &oracleReceivers)
			want, err := json.Marshal(&oracle)
			if err != nil {
				t.Fatal(err)
			}
			for _, receiver := range oracleReceivers {
				if _, duplicate := expected[receiver.Token]; duplicate {
					t.Fatalf("duplicate oracle receiver token %q", receiver.Token)
				}
				expected[receiver.Token] = receiver.Number
			}

			value := build(round, &retained)
			unchanged := build(round, &retained)
			buffer, err = encoder.AppendJSON(buffer[:0], &value)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buffer, want) {
				t.Fatalf("round %d: got %s, want %s", round, buffer, want)
			}
			if !reflect.DeepEqual(value, unchanged) {
				t.Fatalf("round %d: value IsZero method changed its source", round)
			}
		}
		return retained, expected
	}()

	// Inputs have left scope and map/interface scratch has been reused and
	// cleared. Every retained receiver must still be an independent value copy.
	runtime.GC()
	if len(retained) != len(expected) || len(retained) == 0 {
		t.Fatalf("retained %d receivers, want %d", len(retained), len(expected))
	}
	seen := make(map[*omitZeroRetainedValue]bool)
	for _, receiver := range retained {
		if receiver == nil || seen[receiver] {
			t.Fatal("value IsZero calls did not retain distinct receiver copies")
		}
		seen[receiver] = true
		number, ok := expected[receiver.Token]
		if !ok || receiver.Number != number {
			t.Fatalf("retained receiver changed: token %q, number %d; want %d (found %t)", receiver.Token, receiver.Number, number, ok)
		}
		delete(expected, receiver.Token)
	}
}

type omitZeroPromotedPointerReceiver struct {
	Number   int `json:"number"`
	retained *[]*omitZeroPromotedPointerReceiver
}

func (v *omitZeroPromotedPointerReceiver) IsZero() bool {
	if v == nil {
		return true
	}
	zero := v.Number == 0
	v.Number++
	*v.retained = append(*v.retained, v)
	return zero
}

type omitZeroPromotedPointer struct {
	*omitZeroPromotedPointerReceiver
}

type omitZeroPromotedPointerDocument struct {
	Value omitZeroPromotedPointer `json:"value,omitzero"`
}

func TestOmitZeroPromotedPointerMethod(t *testing.T) {
	t.Run("struct", func(t *testing.T) {
		checkOmitZeroPromotedPointer(t, func(receiver *omitZeroPromotedPointerReceiver) omitZeroPromotedPointerDocument {
			return omitZeroPromotedPointerDocument{Value: omitZeroPromotedPointer{receiver}}
		})
	})
	t.Run("interface-map", func(t *testing.T) {
		checkOmitZeroPromotedPointer(t, func(receiver *omitZeroPromotedPointerReceiver) any {
			return map[string]omitZeroPromotedPointerDocument{
				"key": {Value: omitZeroPromotedPointer{receiver}},
			}
		})
	})
}

func checkOmitZeroPromotedPointer[T any](t *testing.T, build func(*omitZeroPromotedPointerReceiver) T) {
	t.Helper()
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The field implements IsZero through an embedded pointer even though the
	// field itself is not a pointer. Promotion preserves nil handling and mutation.
	nilValue := build(nil)
	want, err := json.Marshal(&nilValue)
	if err != nil {
		t.Fatal(err)
	}
	got, err := encoder.AppendJSON(nil, &nilValue)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("nil promoted receiver: got %s (%v), want %s", got, err, want)
	}

	var retained, oracleRetained []*omitZeroPromotedPointerReceiver
	receiver := &omitZeroPromotedPointerReceiver{retained: &retained}
	oracleReceiver := &omitZeroPromotedPointerReceiver{retained: &oracleRetained}
	value, oracle := build(receiver), build(oracleReceiver)
	for round := 0; round < 4; round++ {
		want, err := json.Marshal(&oracle)
		if err != nil {
			t.Fatal(err)
		}
		got, err := encoder.AppendJSON(nil, &value)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("round %d: got %s (%v), want %s", round, got, err, want)
		}
		if receiver.Number != oracleReceiver.Number {
			t.Fatalf("round %d: shared receiver number = %d, want %d", round, receiver.Number, oracleReceiver.Number)
		}
	}
	runtime.GC()
	if len(retained) != len(oracleRetained) || len(retained) != 4 {
		t.Fatalf("retained %d receivers, want %d", len(retained), len(oracleRetained))
	}
	for _, got := range retained {
		if got != receiver {
			t.Fatal("promoted pointer method received a copied pointee")
		}
	}
}
