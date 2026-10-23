package vibejson

import "slices"

// Long paths use an append-only reversed buffer while an error unwinds, so
// a deeply nested failure does not repeatedly copy the entire growing path.
// Keep short errors on the direct string path without an extra allocation.
const decodePathBufferThreshold = 256

type reversedDecodePath []byte

func newReversedDecodePath(path string) reversedDecodePath {
	buffer := reversedDecodePath(path)
	slices.Reverse(buffer)
	return buffer
}

func (p *reversedDecodePath) prepend(segment string) {
	buffer := *p
	if len(buffer) > 0 && buffer[len(buffer)-1] != '[' {
		buffer = append(buffer, '.')
	}
	for i := len(segment) - 1; i >= 0; i-- {
		buffer = append(buffer, segment[i])
	}
	*p = buffer
}

func (p reversedDecodePath) render() string {
	// Diagnostics can be requested before another caller prepends context.
	buffer := slices.Clone(p)
	slices.Reverse(buffer)
	return string(buffer)
}

func (p reversedDecodePath) finish() string {
	slices.Reverse(p)
	return string(p)
}

type decodePathError struct {
	cause    *DecodeError
	reversed reversedDecodePath
}

func (e *decodePathError) Error() string {
	value := *e.cause
	value.Path = e.reversed.render()
	return value.Error()
}

func newDecodePathError(e *DecodeError) *decodePathError {
	return &decodePathError{cause: e, reversed: newReversedDecodePath(e.Path)}
}

func finishDecodeError(err error) error {
	if e, ok := err.(*decodePathError); ok {
		e.cause.Path = e.reversed.finish()
		return e.cause
	}
	return err
}
