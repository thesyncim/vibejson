package slopjson

import (
	"runtime"
	"testing"
)

type safeHookReceiver struct {
	Value int
}

var (
	retainedDecodeReceiver *safeHookReceiver
	retainedEncodeReceiver *safeHookReceiver
	retainedArrayReceivers [4]*safeArrayHook
)

type safeArrayHook int

func (receiver *safeArrayHook) MarshalSimdJSON(appender TrustedAppender) TrustedAppender {
	index := int(*receiver)
	if 0 <= index && index < len(retainedArrayReceivers) {
		retainedArrayReceivers[index] = receiver
	}
	return appender.Int(int64(index))
}

func (receiver *safeHookReceiver) UnmarshalSimdJSON(cursor DecodeCursor) (DecodeCursor, error) {
	retainedDecodeReceiver = receiver
	stackSink := forceStackMovement(48, receiver.Value)
	runtime.GC()
	runtime.KeepAlive(stackSink)
	err := cursor.Int(&receiver.Value)
	return cursor, err
}

// TestEncodeHookArrayUsesStableSourcePointers covers the allocation-free batch
// case directly: every element receives its distinct address in caller-owned
// array storage, and retained pointers remain valid across stack movement and
// GC without a per-element shadow allocation.
func TestEncodeHookArrayUsesStableSourcePointers(t *testing.T) {
	retainedArrayReceivers = [4]*safeArrayHook{}
	t.Cleanup(func() { retainedArrayReceivers = [4]*safeArrayHook{} })
	encoder, err := CompileEncoder[[4]safeArrayHook](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	values := [4]safeArrayHook{0, 1, 2, 3}
	out, err := encoder.AppendJSON(nil, &values)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `[0,1,2,3]` {
		t.Fatalf("encoded %s, want [0,1,2,3]", out)
	}
	for i := range values {
		if retainedArrayReceivers[i] != &values[i] {
			t.Fatalf("element %d receiver does not point at caller-owned array storage", i)
		}
	}
	stackSink := forceStackMovement(64, 19)
	runtime.GC()
	for i := range values {
		*retainedArrayReceivers[i] += 10
	}
	runtime.KeepAlive(stackSink)
	if values != [4]safeArrayHook{10, 11, 12, 13} {
		t.Fatalf("retained receivers did not remain valid: %v", values)
	}
}

func (receiver *safeHookReceiver) MarshalSimdJSON(appender TrustedAppender) TrustedAppender {
	retainedEncodeReceiver = receiver
	receiver.Value++
	return appender.Int(int64(receiver.Value))
}

// TestHookReceiverLifetimes pins the ordinary receiver contract. Decode and
// encode both receive the addressable caller-owned *T, so retaining either
// receiver keeps and aliases the caller's value like a direct Go method call.
func TestHookReceiverLifetimes(t *testing.T) {
	retainedDecodeReceiver = nil
	retainedEncodeReceiver = nil
	t.Cleanup(func() {
		retainedDecodeReceiver = nil
		retainedEncodeReceiver = nil
	})

	decoder, err := CompileDecoder[safeHookReceiver](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded := safeHookReceiver{}
	input := [...]byte{'7'}
	if err := decoder.Decode(input[:], &decoded); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(input)
	if decoded.Value != 7 {
		t.Fatalf("decoded value = %d, want 7", decoded.Value)
	}
	if retainedDecodeReceiver != &decoded {
		t.Fatal("decode hook did not receive the caller-owned addressable receiver")
	}

	encoder, err := CompileEncoder[safeHookReceiver](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded := safeHookReceiver{Value: 10}
	out, err := encoder.AppendJSON(nil, &encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `11` || encoded.Value != 11 {
		t.Fatalf("encoded %s with copied-back value %d, want 11 and 11", out, encoded.Value)
	}
	if retainedEncodeReceiver != &encoded {
		t.Fatal("encode hook did not receive the caller-owned addressable receiver")
	}

	stackSink := forceStackMovement(48, 7)
	runtime.GC()
	retainedDecodeReceiver.Value = 70
	retainedEncodeReceiver.Value = 110
	runtime.KeepAlive(stackSink)
	if decoded.Value != 70 || encoded.Value != 110 {
		t.Fatalf("receiver ownership differs: decoded=%d encoded=%d", decoded.Value, encoded.Value)
	}
}
