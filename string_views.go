package vibejson

import "github.com/thesyncim/vibejson/x/byteview"

// appendJSONString exposes text as a read-only byte view for the shared JSON
// string encoder. The view is retained only for the duration of the call.
func appendJSONString(dst []byte, text string) []byte {
	return appendJSONStringBytes(dst, byteview.Bytes(text))
}

// string converts p.src[start:end] into a result string. Zero-copy results
// alias p.src directly. Owned results are packed into append-only blocks, so
// only retained string bytes are copied and earlier strings are never mutated.
func (p *parser) string(start, end int) string {
	if start == end {
		return ""
	}
	if p.zeroCopy {
		return byteview.String(p.src[start:end])
	}
	need := end - start
	out := p.arenaBlock()
	if cap(out)-len(out) < need {
		capacity := 2 * cap(out)
		if minimum := need + stringArenaHeadroom; capacity < minimum {
			capacity = minimum
		}
		out = make([]byte, 0, capacity)
	}
	offset := len(out)
	out = append(out, p.src[start:end]...)
	p.strings = out
	return OwnedBytesString(out[offset:])
}

// OwnedBytesString exposes owned bytes as a string without copying. Callers
// must keep the backing bytes alive and immutable for the string's lifetime.
func OwnedBytesString(b []byte) string {
	return byteview.String(b)
}
