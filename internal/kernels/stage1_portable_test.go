package kernels

import "testing"

var stage1PortableBlockSink Stage1Masks

func stage1BlockBytewise(block *[64]byte) Stage1Masks {
	var masks Stage1Masks
	for i, c := range block {
		bit := uint64(1) << i
		switch c {
		case ' ', '\t', '\n', '\r':
			masks.Whitespace |= bit
		case '{', '}', '[', ']', ':', ',':
			masks.Structural |= bit
		case '"':
			masks.Quote |= bit
		case '\\':
			masks.Backslash |= bit
		}
		if c < 0x20 {
			masks.Control |= bit
		}
		if c >= 0x80 {
			masks.NonASCII = true
		}
	}
	return masks
}

func TestStage1BlockPortableExhaustive(t *testing.T) {
	var block [64]byte
	for i := range block {
		block[i] = 'a'
	}
	for value := 0; value < 256; value++ {
		for lane := range block {
			block[lane] = byte(value)
			var got Stage1Masks
			stage1BlockPortable(&block, &got)
			if want := stage1BlockBytewise(&block); got != want {
				t.Fatalf("byte %#02x lane %d: got %+v, want %+v", value, lane, got, want)
			}
			block[lane] = 'a'
		}
	}

	state := uint64(0x9e3779b97f4a7c15)
	for round := 0; round < 100000; round++ {
		for i := range block {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			block[i] = byte(state)
		}
		var got Stage1Masks
		stage1BlockPortable(&block, &got)
		if want := stage1BlockBytewise(&block); got != want {
			t.Fatalf("random round %d: got %+v, want %+v", round, got, want)
		}
	}
}

func stage1BracketsBytewise(block *[64]byte) Stage1BracketMasks {
	var masks Stage1BracketMasks
	for i, c := range block {
		bit := uint64(1) << i
		switch c {
		case '"':
			masks.Quote |= bit
		case '\\':
			masks.Backslash |= bit
		case '{', '[':
			masks.Open |= bit
		case '}', ']':
			masks.Close |= bit
		}
	}
	return masks
}

func TestStage1BlockBracketsPortableExhaustive(t *testing.T) {
	var block [64]byte
	for i := range block {
		block[i] = 'a'
	}
	for value := 0; value < 256; value++ {
		for lane := range block {
			block[lane] = byte(value)
			var got Stage1BracketMasks
			stage1BlockBracketsPortable(&block, &got)
			if want := stage1BracketsBytewise(&block); got != want {
				t.Fatalf("byte %#02x lane %d: got %+v, want %+v", value, lane, got, want)
			}
			block[lane] = 'a'
		}
	}

	state := uint64(0x9e3779b97f4a7c15)
	for round := 0; round < 100000; round++ {
		for i := range block {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			block[i] = byte(state)
		}
		var got Stage1BracketMasks
		stage1BlockBracketsPortable(&block, &got)
		if want := stage1BracketsBytewise(&block); got != want {
			t.Fatalf("random round %d: got %+v, want %+v", round, got, want)
		}
	}
}

func BenchmarkStage1BlockPortable(b *testing.B) {
	var block [64]byte
	copy(block[:], `    {"key":"value with words","n":12345,"ok":true},`)
	b.Run("bytewise", func(b *testing.B) {
		b.SetBytes(64)
		for i := 0; i < b.N; i++ {
			stage1PortableBlockSink = stage1BlockBytewise(&block)
		}
	})
	b.Run("swar", func(b *testing.B) {
		b.SetBytes(64)
		for i := 0; i < b.N; i++ {
			stage1BlockPortable(&block, &stage1PortableBlockSink)
		}
	})
}

func stage1RefEscaped(backslash uint64, escaped bool) (uint64, bool) {
	var out uint64
	for bit := uint64(1); bit != 0; bit <<= 1 {
		if escaped {
			out |= bit
			escaped = false
			continue
		}
		if backslash&bit != 0 {
			escaped = true
		}
	}
	return out, escaped
}

func TestStage1EscapedMatchesReference(t *testing.T) {
	state := uint64(0x243f6a8885a308d3)
	var carry Stage1Carry
	refEscaped := false
	for round := 0; round < 200000; round++ {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		backslash := state
		switch round % 4 {
		case 1:
			backslash = 0
		case 2:
			backslash = ^uint64(0)
		case 3:
			backslash &= 0xff00ff00ff00ff0f
		}
		got := Stage1Escaped(backslash, &carry)
		want, wantEscaped := stage1RefEscaped(backslash, refEscaped)
		refEscaped = wantEscaped
		if got != want || (carry.Escaped != 0) != refEscaped {
			t.Fatalf("round %d: got mask=%#x carry=%#x, want mask=%#x carry=%v",
				round, got, carry.Escaped, want, refEscaped)
		}
	}
}

func stage1RefPrefixXOR(quotes uint64, inString bool) (uint64, bool) {
	var out uint64
	for bit := uint64(1); bit != 0; bit <<= 1 {
		if quotes&bit != 0 {
			inString = !inString
		}
		if inString {
			out |= bit
		}
	}
	return out, inString
}

func TestStage1PrefixXORMatchesReference(t *testing.T) {
	state := uint64(0x13198a2e03707344)
	var carry Stage1Carry
	refInString := false
	for round := 0; round < 200000; round++ {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		quotes := state & (state >> 3) & (state >> 11)
		if round%5 == 0 {
			quotes = 0 // exercise the quote-free fast path with both carry states
		}
		got := Stage1PrefixXOR(quotes, &carry)
		want, wantInString := stage1RefPrefixXOR(quotes, refInString)
		refInString = wantInString
		if got != want || (carry.InString != 0) != refInString {
			t.Fatalf("round %d: got mask=%#x carry=%#x, want mask=%#x carry=%v",
				round, got, carry.InString, want, refInString)
		}
	}
}

type stage1PortableWalker struct {
	escaped bool
	inStr   bool
	follows bool
}

func (w *stage1PortableWalker) block(block *[64]byte) (Stage1Masks, Stage1Rec) {
	masks := stage1BlockBytewise(block)
	var rec Stage1Rec
	for i, c := range block {
		bit := uint64(1) << i
		whitespace := c == ' ' || c == '\t' || c == '\n' || c == '\r'
		structural := c == '{' || c == '}' || c == '[' || c == ']' || c == ':' || c == ','
		rawQuote := c == '"'
		control := c < 0x20

		if c >= 0x80 {
			rec.NonASCII = true
		}
		escaped := w.escaped
		w.escaped = false
		if c == '\\' && !escaped {
			w.escaped = true
		}
		quote := rawQuote && !escaped
		if quote {
			w.inStr = !w.inStr
		}
		inString := w.inStr
		outside := !inString && !quote
		candidate := !whitespace && !structural && !rawQuote && !inString
		start := candidate && !w.follows
		w.follows = candidate

		if structural && outside || quote && inString || start && outside {
			rec.Emit |= bit
		}
		if candidate {
			rec.Scalar |= bit
		}
		if escaped && inString {
			rec.EscInStr |= bit
		}
		if control && (inString || outside && !whitespace) {
			rec.Bad = true
		}
		if whitespace && outside {
			rec.WsOut |= bit
		}
		if inString {
			rec.InStr |= bit
		}
	}
	return masks, rec
}

func TestStage1RecFromMasksMatchesWalker(t *testing.T) {
	alphabet := []byte{'"', '\\', '{', '}', '[', ']', ':', ',', ' ', '\t', '\n', '\r', 0, 0x1f, 0x7f, 0x80, 0xff, 'a', '0'}
	state := uint64(0x9e3779b97f4a7c15)
	var walker stage1PortableWalker
	var stream Stage1Stream
	for round := 0; round < 20000; round++ {
		var block [64]byte
		for i := range block {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			block[i] = alphabet[state%uint64(len(alphabet))]
		}
		masks, want := walker.block(&block)
		var classified Stage1Masks
		stage1BlockPortable(&block, &classified)
		if classified != masks {
			t.Fatalf("round %d: classifier %+v, want %+v", round, classified, masks)
		}
		var got Stage1Rec
		Stage1RecFromMasks(&classified, &stream, &got)
		if got != want {
			t.Fatalf("round %d: got %+v, want %+v; block=%q", round, got, want, block)
		}
	}
}
