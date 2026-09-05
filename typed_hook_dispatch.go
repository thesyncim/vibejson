package vibejson

// decodeRootHook constructs the receiver interface directly at the generic
// entry point, avoiding reflection and interpreter dispatch for a root hook.
// Replacement still uses the shared reset and alias-handling path.
func decodeRootHook(src []byte, options DecoderOptions, hook UnmarshalerSimd) error {
	cursor := newDecoderCursor(src, options)
	cursor.skipSpace()
	next, err := hook.UnmarshalVibeJSON(DecodeCursor{d: cursor})
	if err != nil {
		return err
	}
	return next.d.Finish()
}

// Hook dispatch has one implementation in every build. Decode cursor state is
// transferred by value, like TrustedAppender on encode, so user code never
// receives a pointer into the decoder's frame. Interface values are constructed
// by the Go runtime; there is no safety build tag or unsafe fast alternative.

// dispatchDecodeHook gives the hook an owned cursor value and copies the
// returned state back. A retained DecodeCursor is an independent value holding
// ordinary Go slice and pointer fields, never an alias to a decoder stack frame.
func dispatchDecodeHook(hook UnmarshalerSimd, cursor *decoderCursor) error {
	next, err := hook.UnmarshalVibeJSON(DecodeCursor{d: *cursor})
	*cursor = next.d
	return err
}

// dispatchEncodeHook calls the encode hook with an ordinary value. Retaining
// that value cannot create a dangling stack pointer, but it still violates the
// buffer-ownership contract if the caller later reuses the output storage.
func dispatchEncodeHook(hook MarshalerSimd, w TrustedAppender) TrustedAppender {
	return hook.MarshalVibeJSON(w)
}
