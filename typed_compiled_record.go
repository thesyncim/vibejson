package vibejson

import (
	"encoding/binary"
	"math/bits"
	"unsafe"

	"github.com/thesyncim/vibejson/x/byteview"
)

func (cursor *decoderCursor) decodeCompiledStruct(node *typedNode, dst unsafe.Pointer) error {
	if i := cursor.i; i < len(cursor.src) && cursor.src[i] == '{' && cursor.depth < cursor.maxDepth {
		cursor.depth++
		cursor.i = i + 1
	} else {
		null := false
		if !cursor.notNullFast() {
			var err error
			null, err = cursor.TryNull()
			if err != nil {
				return err
			}
		}
		if null {
			// encoding/json treats null on a struct as a no-op.
			if cursor.flags&decoderReplace != 0 {
				cursor.clearTypedReplaceReferences(node, dst)
				resetTyped(node, dst)
			}
			return nil
		}
		if err := cursor.BeginObject(node.name); err != nil {
			return err
		}
	}
	if cursor.flags&decoderReplace != 0 && len(node.hopResets) != 0 {
		cursor.prepareReplaceHops(node, dst)
	}
	var seen uint64
	var position int
	var first bool
	var inlineDec *decoderMapScratch
	if cursor.flags&decoderReplace != 0 && node.allSet == 0 {
		goto replacePrelude
	}
decodeFields:
	seen = 0
	position, first = 0, true
	// inlineDec stays nil until the first unknown member of a struct that
	// declares a catch-all, so structs without one add only this word.
	inlineDec = nil
	for {
		var field *typedField
		var key string
		var ok, matched bool
		var err error
		if uint(position) < uint(len(node.fields)) {
			field = &node.fields[position]
			if cursor.flags&decoderExpectedSlow == 0 {
				// Inline compact expected-key match for short keys (<= 7 bytes)
				// to avoid a call to matchObjectFieldExpected on every field.
				src := cursor.src
				i := uint(cursor.i)
				n := uint(len(src))
				if field.keyMask != 0 {
					var keyStart uint
					if first {
						if i < n && src[i] == '"' {
							keyStart = i + 1
						}
					} else if i+1 < n && src[i] == ',' && src[i+1] == '"' {
						keyStart = i + 2
					}
					if keyStart != 0 && keyStart+8 <= n &&
						(binary.LittleEndian.Uint64(src[keyStart:])^field.key)&field.keyMask == 0 {
						if field.keyLen <= 6 {
							next := keyStart + uint(field.keyLen) + 2
							if next < n {
								if src[next] > ' ' {
									cursor.i = int(next)
									matched, ok = true, true
									goto fieldMatched
								}
								if src[next] == ' ' && next+1 < n && src[next+1] > ' ' {
									cursor.i = int(next + 1)
									matched, ok = true, true
									goto fieldMatched
								}
							}
						} else if field.keyLen == 7 {
							keyEnd := keyStart + 7
							if keyEnd+2 <= n && binary.LittleEndian.Uint16(src[keyEnd:]) == quoteColonLE {
								next := keyEnd + 2
								if next < n {
									if src[next] > ' ' {
										cursor.i = int(next)
										matched, ok = true, true
										goto fieldMatched
									}
									if src[next] == ' ' && next+1 < n && src[next+1] > ' ' {
										cursor.i = int(next + 1)
										matched, ok = true, true
										goto fieldMatched
									}
								}
							}
						}
					}
				}
				if cursor.matchObjectFieldExpected(first, field) {
					matched, ok = true, true
				} else {
					cursor.flags |= decoderExpectedSlow
					key, matched, ok, err = cursor.nextObjectFieldExpectedSlow(first, field)
				}
			} else {
				key, matched, ok, err = cursor.nextObjectFieldExpectedSlow(first, field)
			}
		} else {
			if !first {
				src := cursor.src
				if i := cursor.i; i < len(src) && src[i] == '}' {
					cursor.i = i + 1
					cursor.depth--
					if cursor.flags&decoderReplace != 0 {
						cursor.resetMissingTypedFields(node, dst, seen)
					}
					releaseInlineMapScratch(inlineDec)
					return nil
				}
			}
			key, ok, err = cursor.NextObjectField(first)
		}
	fieldMatched:
		if err != nil {
			return err
		}
		if !ok {
			if cursor.flags&decoderReplace != 0 {
				cursor.resetMissingTypedFields(node, dst, seen)
			}
			releaseInlineMapScratch(inlineDec)
			return nil
		}
		first = false
		if matched {
			position++
		} else {
			field = node.findFieldSlow(key, !cursor.CaseSensitive())
			if field == nil {
				if node.inlineMap != nil {
					// The catch-all consumes the member, so it is never
					// "unknown" and DisallowUnknownFields does not apply.
					if inlineDec == nil {
						inlineDec = cursor.takeInlineDecoder(node.inlineMap)
						seen |= typedSeenInlineMap
					}
					if err := inlineDec.decodeInlineEntry(cursor, node.inlineMap, dst, key); err != nil {
						return prependDecodePathField(err, key)
					}
					continue
				}
				if err := cursor.Unknown(node.name, key); err != nil {
					return err
				}
				continue
			}
			// Resume expected-key matching after the matched field, so
			// documents whose member order is shifted from the struct order
			// (extra members, rotations) recover the fast path.
			position = int(field.pos) + 1
		}
		seen |= field.seen
		fieldNode := field.node
		fieldBase := dst
		if field.hop >= 0 {
			resolved, hopErr := resolveDecodeHops(dst, node.fieldHops[field.hop], cursor.i)
			if hopErr != nil {
				return prependDecodePathField(hopErr, field.name)
			}
			fieldBase = resolved
		}
		fieldDst := unsafe.Add(fieldBase, field.offset)
		// Inline fast paths for the most common compact scalar field kinds
		// (bool, int64, clean ASCII string) before the generated dispatch.
		switch field.op {
		case typedOpBool:
			src := cursor.src
			if i := uint(cursor.i); i+4 <= uint(len(src)) {
				if binary.LittleEndian.Uint32(src[i:i+4]) == wordTrueLE {
					*(*bool)(fieldDst) = true
					cursor.i = int(i + 4)
					continue
				}
				if i+5 <= uint(len(src)) && src[i] == 'f' && binary.LittleEndian.Uint32(src[i+1:i+5]) == wordAlseLE {
					*(*bool)(fieldDst) = false
					cursor.i = int(i + 5)
					continue
				}
			}
		case typedOpInt64:
			src := cursor.src
			n := uint(len(src))
			if i := uint(cursor.i); i < n {
				c0 := src[i]
				if c0 == '0' {
					if i+1 < n && (src[i+1] == ',' || src[i+1] == '}') {
						*(*int64)(fieldDst) = 0
						cursor.i = int(i + 1)
						continue
					}
				} else if c0 >= '1' && c0 <= '9' {
					u := int64(c0 - '0')
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
					if j < n && (src[j] == ',' || src[j] == '}' || src[j] <= ' ') {
						*(*int64)(fieldDst) = u
						cursor.i = int(j)
						continue
					}
				}
			}
		case typedOpString:
			src := cursor.src
			n := uint(len(src))
			if i := uint(cursor.i); i < n && src[i] == '"' {
				start := i + 1
				var end uint
				if start+8 <= n {
					if m := stringSpecialMask(binary.LittleEndian.Uint64(src[start : start+8])); m != 0 {
						off := uint(bits.TrailingZeros64(m)) >> 3
						if src[start+off] == '"' {
							end = start + off
							goto stringFastDone
						}
					} else if start+16 <= n {
						if m := stringSpecialMask(binary.LittleEndian.Uint64(src[start+8 : start+16])); m != 0 {
							off := uint(bits.TrailingZeros64(m)) >> 3
							if src[start+8+off] == '"' {
								end = start + 8 + off
								goto stringFastDone
							}
						} else if e := scanStringSpecial(src, int(start+16)); uint(e) < n && src[uint(e)] == '"' {
							end = uint(e)
							goto stringFastDone
						}
					}
				}
				goto slowFieldDispatch
			stringFastDone:
				sDst := (*string)(fieldDst)
				if cursor.flags&decoderZeroCopy != 0 {
					*sDst = byteview.String(src[start:end])
				} else if !cursor.reuseOwnedString(*sDst, int(start), int(end)) {
					*sDst = cursor.ownedString(int(start), int(end))
				}
				cursor.i = int(end + 1)
				continue
			}
		}
	slowFieldDispatch:
		var fieldErr error
		switch field.op {
		// BEGIN GENERATED TYPED CURSOR FIELD DISPATCH
		case typedOpBool:
			fieldErr = cursor.Bool((*bool)(fieldDst))
		case typedOpString:
			fieldErr = cursor.String((*string)(fieldDst))
		case typedOpNumber:
			fieldErr = cursor.Number((*string)(fieldDst))
		case typedOpInt8:
			fieldErr = cursor.Int((*int8)(fieldDst))
		case typedOpInt16:
			fieldErr = cursor.Int((*int16)(fieldDst))
		case typedOpInt32:
			fieldErr = cursor.Int((*int32)(fieldDst))
		case typedOpInt64:
			fieldErr = cursor.Int((*int64)(fieldDst))
		case typedOpUint8:
			fieldErr = cursor.Uint((*uint8)(fieldDst))
		case typedOpUint16:
			fieldErr = cursor.Uint((*uint16)(fieldDst))
		case typedOpUint32:
			fieldErr = cursor.Uint((*uint32)(fieldDst))
		case typedOpUint64:
			fieldErr = cursor.Uint((*uint64)(fieldDst))
		case typedOpFloat32:
			fieldErr = cursor.Float((*float32)(fieldDst))
		case typedOpFloat64:
			fieldErr = cursor.Float((*float64)(fieldDst))
		case typedOpStruct:
			fieldErr = cursor.decodeCompiledStruct(fieldNode, fieldDst)
		case typedOpSlice:
			fieldErr = cursor.decodeCompiledSlice(fieldNode, fieldDst)
		case typedOpArray:
			fieldErr = cursor.decodeCompiledArray(fieldNode, fieldDst)
		case typedOpPointer:
			fieldErr = cursor.decodeCompiledPointer(fieldNode, fieldDst)
		case typedOpMap:
			fieldErr = cursor.decodeCompiledMap(fieldNode, fieldDst)
		case typedOpAny:
			fieldErr = cursor.decodeCompiledAny(fieldDst)
		case typedOpBytes:
			fieldErr = cursor.decodeCompiledBytes(fieldNode, fieldDst)
		case typedOpQuoted:
			fieldErr = cursor.decodeQuotedField(fieldNode, fieldDst)
		case typedOpUnmarshaler:
			switch fieldNode.kind {
			case typedUnmarshalerJSON:
				fieldErr = cursor.decodeViaUnmarshaler(fieldNode, fieldDst)
			case typedUnmarshalerSimd:
				fieldErr = cursor.decodeViaSimdHook(fieldNode, fieldDst)
			default:
				fieldErr = cursor.decodeViaTextUnmarshaler(fieldNode, fieldDst)
			}
		case typedOpIface:
			fieldErr = cursor.decodeCompiledIface(fieldNode, fieldDst)
		// END GENERATED TYPED CURSOR FIELD DISPATCH
		default:
			fieldErr = cursor.decodeCompiled(fieldNode, fieldDst)
		}
		if fieldErr != nil {
			if field.op > typedOpInvalid && field.op < typedOpStruct {
				fieldErr = retagCompiledError(fieldErr, fieldNode.typ)
			}
			return prependDecodePathField(fieldErr, field.name)
		}
	}

replacePrelude:
	if node.hasDecWideSeen() {
		if node.hasDecResetIgnored() {
			// An unexported embedded pointer must be nil before resolving a
			// promoted key, exactly as in a fresh destination.
			resetTypedIgnored(node, dst)
		}
		return cursor.decodeCompiledStructWide(node, dst)
	}
	if len(node.fields) > 0 {
		resetTyped(node, dst)
	}
	goto decodeFields
}
