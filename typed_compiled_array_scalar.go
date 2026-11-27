package vibejson

import (
	"encoding/binary"

	"github.com/thesyncim/vibejson/x/byteview"
)

func decodeCompiledInt64Array(cursor *decoderCursor, node *typedNode, values []int64) error {
	replace := cursor.flags&decoderReplace != 0
	src := cursor.src
	n := uint(len(src))
	for index, first := 0, true; ; index, first = index+1, false {
		if !first {
			if i := uint(cursor.i); i < n {
				switch src[i] {
				case ',':
					if i+1 < n && src[i+1] > ' ' {
						cursor.i = int(i + 1)
						goto parseElem
					}
				case ']':
					cursor.i = int(i + 1)
					cursor.depth--
					if !replace && index < len(values) {
						clear(values[index:])
					}
					return nil
				}
			}
		} else if i := uint(cursor.i); i < n && (isDigit(src[i]) || src[i] == '-') {
			goto parseElem
		}
		{
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if !replace && index < len(values) {
					clear(values[index:])
				}
				return nil
			}
		}
	parseElem:
		if index < len(values) {
			i := uint(cursor.i)
			if i < n {
				c := src[i]
				if c >= '1' && c <= '9' {
					u := int64(c - '0')
					j := i + 1
					limit := i + 18
					if limit > n {
						limit = n
					}
					for j < limit {
						d := src[j] - '0'
						if d > 9 {
							break
						}
						u = u*10 + int64(d)
						j++
					}
					if j < n && (src[j] == ',' || src[j] == ']' || src[j] <= ' ') {
						values[index] = u
						cursor.i = int(j)
						continue
					}
				} else if c == '0' {
					if i+1 < n && (src[i+1] == ',' || src[i+1] == ']' || src[i+1] <= ' ') {
						values[index] = 0
						cursor.i = int(i + 1)
						continue
					}
				}
			}
			if err := cursor.Int(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}

func decodeCompiledUint64Array(cursor *decoderCursor, node *typedNode, values []uint64) error {
	replace := cursor.flags&decoderReplace != 0
	src := cursor.src
	n := uint(len(src))
	for index, first := 0, true; ; index, first = index+1, false {
		if !first {
			if i := uint(cursor.i); i < n {
				switch src[i] {
				case ',':
					if i+1 < n && src[i+1] > ' ' {
						cursor.i = int(i + 1)
						goto parseElem
					}
				case ']':
					cursor.i = int(i + 1)
					cursor.depth--
					if !replace && index < len(values) {
						clear(values[index:])
					}
					return nil
				}
			}
		} else if i := uint(cursor.i); i < n && isDigit(src[i]) {
			goto parseElem
		}
		{
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if !replace && index < len(values) {
					clear(values[index:])
				}
				return nil
			}
		}
	parseElem:
		if index < len(values) {
			i := uint(cursor.i)
			if i < n {
				c := src[i]
				if c >= '1' && c <= '9' {
					u := uint64(c - '0')
					j := i + 1
					limit := i + 19
					if limit > n {
						limit = n
					}
					for j < limit {
						d := src[j] - '0'
						if d > 9 {
							break
						}
						u = u*10 + uint64(d)
						j++
					}
					if j < n && (src[j] == ',' || src[j] == ']' || src[j] <= ' ') {
						values[index] = u
						cursor.i = int(j)
						continue
					}
				} else if c == '0' {
					if i+1 < n && (src[i+1] == ',' || src[i+1] == ']' || src[i+1] <= ' ') {
						values[index] = 0
						cursor.i = int(i + 1)
						continue
					}
				}
			}
			if err := cursor.Uint(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}

func decodeCompiledUint32Array(cursor *decoderCursor, node *typedNode, values []uint32) error {
	replace := cursor.flags&decoderReplace != 0
	src := cursor.src
	n := uint(len(src))
	for index, first := 0, true; ; index, first = index+1, false {
		if !first {
			if i := uint(cursor.i); i < n {
				switch src[i] {
				case ',':
					if i+1 < n && src[i+1] > ' ' {
						cursor.i = int(i + 1)
						goto parseElem
					}
				case ']':
					cursor.i = int(i + 1)
					cursor.depth--
					if !replace && index < len(values) {
						clear(values[index:])
					}
					return nil
				}
			}
		} else if i := uint(cursor.i); i < n && isDigit(src[i]) {
			goto parseElem
		}
		{
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if !replace && index < len(values) {
					clear(values[index:])
				}
				return nil
			}
		}
	parseElem:
		if index < len(values) {
			i := uint(cursor.i)
			if i < n {
				c := src[i]
				if c >= '1' && c <= '9' {
					u := uint32(c - '0')
					j := i + 1
					limit := i + 9
					if limit > n {
						limit = n
					}
					for j < limit {
						d := src[j] - '0'
						if d > 9 {
							break
						}
						u = u*10 + uint32(d)
						j++
					}
					if j < n && (src[j] == ',' || src[j] == ']' || src[j] <= ' ') {
						values[index] = u
						cursor.i = int(j)
						continue
					}
				} else if c == '0' {
					if i+1 < n && (src[i+1] == ',' || src[i+1] == ']' || src[i+1] <= ' ') {
						values[index] = 0
						cursor.i = int(i + 1)
						continue
					}
				}
			}
			if err := cursor.Uint(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}

func decodeCompiledBoolArray(cursor *decoderCursor, node *typedNode, values []bool) error {
	replace := cursor.flags&decoderReplace != 0
	src := cursor.src
	n := uint(len(src))
	for index, first := 0, true; ; index, first = index+1, false {
		if !first {
			if i := uint(cursor.i); i < n {
				switch src[i] {
				case ',':
					if i+1 < n && src[i+1] > ' ' {
						cursor.i = int(i + 1)
						goto parseElem
					}
				case ']':
					cursor.i = int(i + 1)
					cursor.depth--
					if !replace && index < len(values) {
						clear(values[index:])
					}
					return nil
				}
			}
		} else if i := uint(cursor.i); i < n && (src[i] == 't' || src[i] == 'f') {
			goto parseElem
		}
		{
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if !replace && index < len(values) {
					clear(values[index:])
				}
				return nil
			}
		}
	parseElem:
		if index < len(values) {
			if i := uint(cursor.i); i+4 <= n {
				if binary.LittleEndian.Uint32(src[i:i+4]) == wordTrueLE &&
					(i+4 == n || src[i+4] == ',' || src[i+4] == ']' || src[i+4] <= ' ') {
					values[index] = true
					cursor.i = int(i + 4)
					continue
				}
				if i+5 <= n && src[i] == 'f' && binary.LittleEndian.Uint32(src[i+1:i+5]) == wordAlseLE &&
					(i+5 == n || src[i+5] == ',' || src[i+5] == ']' || src[i+5] <= ' ') {
					values[index] = false
					cursor.i = int(i + 5)
					continue
				}
			}
			if err := cursor.Bool(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}

func decodeCompiledStringArray(cursor *decoderCursor, node *typedNode, values []string) error {
	replace := cursor.flags&decoderReplace != 0
	zeroCopy := cursor.flags&decoderZeroCopy != 0
	src := cursor.src
	n := uint(len(src))
	for index, first := 0, true; ; index, first = index+1, false {
		if !first {
			if i := uint(cursor.i); i < n {
				switch src[i] {
				case ',':
					if i+1 < n && src[i+1] > ' ' {
						cursor.i = int(i + 1)
						goto parseElem
					}
				case ']':
					cursor.i = int(i + 1)
					cursor.depth--
					if !replace && index < len(values) {
						clear(values[index:])
					}
					return nil
				}
			}
		} else if i := uint(cursor.i); i < n && src[i] == '"' {
			goto parseElem
		}
		{
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if !replace && index < len(values) {
					clear(values[index:])
				}
				return nil
			}
		}
	parseElem:
		if index < len(values) {
			if i := cursor.i; i < len(src) && src[i] == '"' {
				start := i + 1
				end := scanStringSpecial(src, start)
				if end < len(src) && src[end] == '"' {
					if zeroCopy {
						values[index] = byteview.String(src[start:end])
					} else if !cursor.reuseOwnedString(values[index], start, end) {
						values[index] = cursor.ownedString(start, end)
					}
					cursor.i = end + 1
					continue
				}
			}
			if err := cursor.String(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}
