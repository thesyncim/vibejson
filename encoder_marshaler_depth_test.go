package vibejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEncoderMarshalerCombinedDepth(t *testing.T) {
	type envelope struct {
		Raw json.RawMessage `json:"raw"`
	}
	for _, depth := range []int{DefaultMaxDepth - 1, DefaultMaxDepth} {
		for _, padded := range []bool{false, true} {
			t.Run(fmt.Sprintf("depth=%d/padded=%v", depth, padded), func(t *testing.T) {
				raw := json.RawMessage(strings.Repeat("[", depth) + strings.Repeat("]", depth))
				if padded {
					raw = append(append([]byte(" \n"), raw...), '\t')
				}
				// The standalone payload fits. Its enclosing object must also
				// count when checking the total depth of custom JSON output.
				checkMarshalerDepthResult(t, &raw, true)
				var dynamic any = raw
				checkMarshalerDepthResult(t, &dynamic, true)
				value := envelope{Raw: raw}
				checkMarshalerDepthResult(t, &value, depth < DefaultMaxDepth)
				dynamic = value
				checkMarshalerDepthResult(t, &dynamic, depth < DefaultMaxDepth)
			})
		}
	}
}

type marshalerDepthNode struct {
	Next *marshalerDepthNode `json:"next,omitempty"`
	Raw  json.RawMessage     `json:"raw,omitempty"`
}

func TestEncoderMarshalerAtDepthLimit(t *testing.T) {
	var value marshalerDepthNode
	tail := &value
	for i := 1; i < DefaultMaxDepth; i++ {
		tail.Next = &marshalerDepthNode{}
		tail = tail.Next
	}
	for _, raw := range []string{"0", " true ", `"<tag>"`, "[]", " {} "} {
		t.Run(raw, func(t *testing.T) {
			tail.Raw = json.RawMessage(raw)
			checkMarshalerDepthResult(t, &value, raw != "[]" && raw != " {} ")
		})
	}
}

func checkMarshalerDepthResult[T any](t *testing.T, value *T, wantOK bool) {
	t.Helper()
	want, wantErr := json.Marshal(value)
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, gotErr := encoder.AppendJSON([]byte("prefix:"), value)
	if (gotErr == nil) != wantOK || (wantErr == nil) != wantOK {
		t.Fatalf("vibejson error = %.200v, encoding/json error = %.200v; want success = %v", gotErr, wantErr, wantOK)
	}
	if gotErr != nil {
		if string(got) != "prefix:" {
			t.Fatal("depth error changed the destination prefix")
		}
		return
	}
	if !bytes.Equal(got[7:], want) {
		t.Fatal("encoded output differs from encoding/json")
	}
	if err := Validate(got[7:]); err != nil {
		t.Fatalf("encoded output failed validation: %v", err)
	}
}
