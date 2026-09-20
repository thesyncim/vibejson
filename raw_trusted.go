package vibejson

import (
	"math/bits"

	simdkernels "github.com/thesyncim/vibejson/x/kernels"
)

// ScanFirstRawTrusted resolves a pointer without validating src. It is
// memory-safe for malformed input but returns unspecified lookup results there.
func ScanFirstRawTrusted(src []byte, pointer string) (RawValue, bool, error) {
	return ScanFirstRawTrustedOptions(src, pointer, Options{})
}

// ScanFirstRawTrustedOptions is ScanFirstRawTrusted with parser options.
func ScanFirstRawTrustedOptions(src []byte, pointer string, opts Options) (RawValue, bool, error) {
	p, err := CompilePointer(pointer)
	if err != nil {
		return RawValue{}, false, err
	}
	return p.ScanFirstRawTrustedOptions(src, opts)
}

// ScanFirstRawTrusted resolves p without validating src.
func (p CompiledPointer) ScanFirstRawTrusted(src []byte) (RawValue, bool, error) {
	return p.ScanFirstRawTrustedOptions(src, Options{})
}

// ScanFirstRawTrustedOptions is ScanFirstRawTrusted with parser options.
func (p CompiledPointer) ScanFirstRawTrustedOptions(src []byte, opts Options) (RawValue, bool, error) {
	s := trustedSeeker{src: src, maxDepth: maxDepthOrDefault(opts.MaxDepth)}
	s.i = SkipSpace(src, 0)
	return s.find(0, 0, p)
}

// GetRawTrusted resolves p with last-duplicate semantics on validated input.
func (p CompiledPointer) GetRawTrusted(src []byte) (RawValue, bool, error) {
	s := trustedSeeker{src: src, maxDepth: DefaultMaxDepth, lastWins: true}
	s.i = SkipSpace(src, 0)
	return s.find(0, 0, p)
}

// trustedSeeker is the non-validating counterpart of rawSeeker.
type trustedSeeker struct {
	src      []byte
	i        int
	maxDepth int
	done     bool
	lastWins bool
}

func (s *trustedSeeker) find(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if tokenIndex >= len(pointer.Tokens) {
		return s.capture(depth)
	}
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	if s.i >= len(s.src) {
		return RawValue{}, false, nil
	}
	switch s.src[s.i] {
	case '{':
		return s.findObject(depth+1, tokenIndex, pointer)
	case '[':
		return s.findArray(depth+1, tokenIndex, pointer)
	default:
		// Consume scalars so enclosing loops stay positioned.
		if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		return RawValue{}, false, nil
	}
}

// capture consumes and returns the value at s.i.
func (s *trustedSeeker) capture(depth int) (RawValue, bool, error) {
	start := s.i
	if err := s.skipValue(depth); err != nil {
		return RawValue{}, false, err
	}
	if s.i == start {
		return RawValue{}, false, nil
	}
	if !s.lastWins {
		s.done = true
	}
	return RawValue{Src: s.src[start:s.i]}, true, nil
}

func (s *trustedSeeker) findArray(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	index, indexOK, err := pointer.Tokens[tokenIndex].arrayIndex()
	if err != nil {
		return RawValue{}, false, err
	}

	s.i++
	s.i = SkipSpace(s.src, s.i)
	if s.i < len(s.src) && s.src[s.i] == ']' {
		s.i++
		return RawValue{}, false, nil
	}

	var selected RawValue
	var selectedOK bool
	for elem := 0; ; elem++ {
		s.i = SkipSpace(s.src, s.i)
		if indexOK && elem == index {
			raw, ok, err := s.find(depth, tokenIndex+1, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
			if s.lastWins {
				selected, selectedOK = raw, ok
			}
			// Keep consuming after an absent selected element.
		} else if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			if s.lastWins {
				return selected, selectedOK, nil
			}
			return RawValue{}, false, nil
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			if s.lastWins {
				return selected, selectedOK, nil
			}
			return RawValue{}, false, nil
		default:
			return RawValue{}, false, nil
		}
	}
}

func (s *trustedSeeker) findObject(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	token := pointer.Tokens[tokenIndex].Text

	s.i++
	var last RawValue
	var lastOK bool
	for {
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		}
		if s.src[s.i] == '}' {
			s.i++
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		}
		if s.src[s.i] != '"' {
			return RawValue{}, false, nil
		}
		keyStart, keyEnd, escaped := s.scanKey()
		matched := s.trustedKeyMatches(token, keyStart, keyEnd, escaped)
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) || s.src[s.i] != ':' {
			return RawValue{}, false, nil
		}
		s.i++
		s.i = SkipSpace(s.src, s.i)
		if matched {
			raw, ok, err := s.find(depth, tokenIndex+1, pointer)
			if err != nil {
				return RawValue{}, false, err
			}
			if s.lastWins {
				last, lastOK = raw, ok
			} else if s.done {
				return raw, ok, err
			}
			// A later duplicate may still resolve.
		} else if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		default:
			return RawValue{}, false, nil
		}
	}
}

// scanKey scans the key at s.i without validating its escapes.
func (s *trustedSeeker) scanKey() (start, end int, escaped bool) {
	s.i++
	start = s.i
	for {
		j := scanStringSyntax(s.src, s.i)
		if j >= len(s.src) {
			s.i = len(s.src)
			return start, len(s.src), escaped
		}
		switch s.src[j] {
		case '"':
			s.i = j + 1
			return start, j, escaped
		case '\\':
			escaped = true
			s.i = j + 2
			if s.i > len(s.src) {
				s.i = len(s.src)
				return start, len(s.src), escaped
			}
		default:
			s.i = j + 1
		}
	}
}

// trustedKeyMatches compares a scanned key with token.
func (s *trustedSeeker) trustedKeyMatches(token string, keyStart, keyEnd int, escaped bool) bool {
	if !escaped {
		return BytesEqualString(s.src[keyStart:keyEnd], token)
	}
	p := parser{src: s.src, i: keyStart - 1, maxDepth: s.maxDepth, zeroCopy: true}
	key, err := p.parseString()
	if err != nil {
		return false
	}
	return key == token
}

// skipValue consumes one value using structural scanning.
func (s *trustedSeeker) skipValue(depth int) error {
	if s.i >= len(s.src) {
		return nil
	}
	switch s.src[s.i] {
	case '{', '[':
		return s.skipComposite(depth)
	case '"':
		s.i = skipStringTrusted(s.src, s.i+1)
		return nil
	default:
		s.i = skipScalarTrusted(s.src, s.i)
		return nil
	}
}

// skipComposite consumes a container with the stage-1 bitmap pipeline.
func (s *trustedSeeker) skipComposite(depth int) error {
	src := s.src
	i := s.i
	budget := s.maxDepth - depth
	nest := 0
	var carry simdkernels.Stage1Carry
	var m simdkernels.Stage1BracketMasks
	for i < len(src) {
		block := (*[64]byte)(nil)
		if len(src)-i >= 64 {
			block = (*[64]byte)(src[i:])
		} else {
			// Zero padding prevents a short tail from adding syntax.
			var tail [64]byte
			copy(tail[:], src[i:])
			block = &tail
		}
		simdkernels.Stage1BlockBrackets(block, &m)
		inString := simdkernels.Stage1PrefixXOR(m.Quote&^simdkernels.Stage1Escaped(m.Backslash, &carry), &carry)
		open := m.Open &^ inString
		closes := m.Close &^ inString
		opened := bits.OnesCount64(open)
		closed := bits.OnesCount64(closes)
		// The whole block stays within the depth budget.
		if nest-closed > 0 && nest+opened <= budget {
			nest += opened - closed
			i += 64
			continue
		}
		if nest+opened <= budget {
			// Find the close that returns the composite to depth zero.
			ordinal := 0
			for br := closes; br != 0; br &= br - 1 {
				p := bits.TrailingZeros64(br)
				if nest+bits.OnesCount64(open&(uint64(1)<<p-1))-ordinal == 1 {
					s.i = i + p + 1
					return nil
				}
				ordinal++
			}
			nest += opened - closed
			i += 64
			continue
		}
		for br := open | closes; br != 0; br &= br - 1 {
			if bit := br & (^br + 1); open&bit != 0 {
				nest++
				if nest > budget {
					pos := i + bits.TrailingZeros64(br)
					s.i = pos
					return syntaxError(src, pos, "maximum nesting depth exceeded")
				}
			} else {
				nest--
				if nest == 0 {
					s.i = i + bits.TrailingZeros64(br) + 1
					return nil
				}
			}
		}
		i += 64
	}
	s.i = len(src)
	return nil
}

// skipStringTrusted returns the byte after a closing quote, or len(src).
func skipStringTrusted(src []byte, i int) int {
	for {
		j := scanStringSyntax(src, i)
		if j >= len(src) {
			return len(src)
		}
		switch src[j] {
		case '"':
			return j + 1
		case '\\':
			i = j + 2
			if i > len(src) {
				return len(src)
			}
		default:
			i = j + 1
		}
	}
}

// skipScalarTrusted consumes bytes through the next scalar delimiter.
func skipScalarTrusted(src []byte, i int) int {
	for i < len(src) {
		c := src[i]
		if c <= ' ' || c == ',' || c == '}' || c == ']' {
			return i
		}
		i++
	}
	return len(src)
}
