package vibejson

import (
	"reflect"
	"testing"
)

func TestReplaceNullReuse(t *testing.T) {
	dec, err := CompileDecoder[[]int64](DecoderOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	reused := []int64{5, 5, 5, 5, 5}
	if err := dec.Decode([]byte(`[1,null,2]`), &reused); err != nil {
		t.Fatal(err)
	}
	var fresh []int64
	if err := dec.Decode([]byte(`[1,null,2]`), &fresh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reused, fresh) {
		t.Fatalf("Replace null reuse: reused=%v fresh=%v", reused, fresh)
	}
	if !reflect.DeepEqual(reused, []int64{1, 0, 2}) {
		t.Fatalf("Replace null reuse: got %v want [1 0 2]", reused)
	}
}
