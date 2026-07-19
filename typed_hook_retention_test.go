package simdjson

import (
	"runtime"
	"testing"
)

var retainedPanicCursor DecodeCursor

// retentionProbe keeps the input cursor value before advancing the copy it
// returns. The saved value must remain memory-safe and independent after the
// enclosing decode completes.
type retentionProbe struct {
	stashed DecodeCursor
}

func (r *retentionProbe) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	r.stashed = c
	err := c.Skip()
	return c, err
}

func TestHookRetainedCursorValueIsIndependent(t *testing.T) {
	dec, err := CompileDecoder[retentionProbe](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"x":1}`)
	var value retentionProbe
	if err := dec.Decode(input, &value); err != nil {
		t.Fatal(err)
	}
	input = nil
	runtime.GC()

	// The saved copy still owns the source slice and starts where the hook
	// received it. Advancing it cannot alter the cursor returned to Decode.
	if err := value.stashed.Skip(); err != nil {
		t.Fatalf("retained value was not independently usable: %v", err)
	}
	if value.stashed.d.i != len(value.stashed.d.src) {
		t.Fatalf("retained value stopped at %d of %d", value.stashed.d.i, len(value.stashed.d.src))
	}
}

func TestDecodeArrayHookReceiversRemainIndependent(t *testing.T) {
	dec, err := CompileDecoder[retentionProbe](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	values, err := dec.DecodeArray([]byte(`[1,{"x":2},"three"]`), make([]retentionProbe, 0, 3))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 {
		t.Fatalf("DecodeArray returned %d hook values, want 3", len(values))
	}
	runtime.GC()
	for i := range values {
		if err := values[i].stashed.Skip(); err != nil {
			t.Fatalf("retained cursor %d was not independently usable: %v", i, err)
		}
	}
}

type panicRetentionProbe struct{}

func (*panicRetentionProbe) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	retainedPanicCursor = c
	panic("hook panic")
}

func TestHookRetainedCursorValueAfterPanic(t *testing.T) {
	retainedPanicCursor = DecodeCursor{}
	t.Cleanup(func() { retainedPanicCursor = DecodeCursor{} })
	dec, err := CompileDecoder[panicRetentionProbe](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panicking hook did not propagate its panic")
			}
		}()
		var value panicRetentionProbe
		_ = dec.Decode([]byte(`null`), &value)
	}()
	if len(retainedPanicCursor.d.src) == 0 {
		t.Fatal("panicking hook did not retain its cursor value")
	}
	if err := retainedPanicCursor.Skip(); err != nil {
		t.Fatalf("retained cursor value after panic was unsafe: %v", err)
	}
}
